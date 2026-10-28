package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/prometheus/common/model"
)

const maxBodyBytes = 16 << 20

// Client uses Grafana's classic/native dashboard and alert provisioning APIs.
// Unsupported provisioning is detected during preflight, before any writes.
// Cloud and self-hosted installations must grant access to the declared folder.
// Clients can be shared by goroutines; callers must serialize Apply operations
// targeting the same resources to avoid conflicting releases.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient permits HTTP only when explicitly requested for a trusted local/private
// installation. Redirects are rejected, so credentials cannot follow another origin.
func NewClient(baseURL, token string, allowInsecure bool) (*Client, error) {
	return NewClientWithConfig(ClientConfig{URL: baseURL, Token: token, AllowInsecure: allowInsecure})
}

// ClientConfig supplies credentials and an optional HTTP client for custom TLS,
// transports or timeouts. NewClientWithConfig copies the client, preserving caller
// ownership, and always rejects redirects. A supplied Transport must be safe for
// concurrent use and is trusted to protect the Authorization header.
type ClientConfig struct {
	URL           string
	Token         string
	AllowInsecure bool
	HTTPClient    *http.Client
}

// NewClientWithConfig creates a client without mutating the supplied HTTP client.
// A nil HTTPClient uses a 20-second timeout; supplied timeout settings are retained.
func NewClientWithConfig(cfg ClientConfig) (*Client, error) {
	baseURL, token, allowInsecure := cfg.URL, cfg.Token, cfg.AllowInsecure
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || token == "" {
		return nil, errors.New("grafana needs an absolute HTTP(S) URL without embedded credentials and a nonempty token")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && allowInsecure) {
		return nil, errors.New("grafana requires HTTPS; explicit insecure access is only for trusted local/private installations")
	}
	httpClient := http.Client{Timeout: 20 * time.Second}
	if cfg.HTTPClient != nil {
		httpClient = *cfg.HTTPClient
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("grafana redirects are rejected") }
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), token: token, http: &httpClient}, nil
}

// HTTPError reports an unsuccessful HTTP status without exposing response bodies.
// Use errors.As to inspect StatusCode, for example for rate limiting (429).
type HTTPError struct {
	Method     string
	Path       string
	StatusCode int
}

// Error returns the method, API path and status; it never includes credentials.
func (e *HTTPError) Error() string {
	return fmt.Sprintf("grafana %s %s returned HTTP %d", e.Method, e.Path, e.StatusCode)
}

// TransportError withholds the underlying transport message, which may contain
// credentials. errors.Is still recognizes context cancellation and deadlines;
// errors.As can inspect Timeout through the standard net.Error interface.
type TransportError struct {
	timedOut     bool
	contextError error
}

// Error returns a sanitized transport failure message.
func (e *TransportError) Error() string {
	return "grafana request transport failed; transport details withheld"
}

// Unwrap exposes only a recognized context error, never arbitrary transport data.
func (e *TransportError) Unwrap() error { return e.contextError }

// Timeout reports whether the underlying transport timed out.
func (e *TransportError) Timeout() bool { return e.timedOut }

// Temporary is retained for net.Error compatibility; it does not prescribe retries.
func (e *TransportError) Temporary() bool { return false }

func transportError(err error) error {
	safe := &TransportError{}
	var networkError net.Error
	if errors.As(err, &networkError) {
		safe.timedOut = networkError.Timeout()
	}
	if errors.Is(err, context.Canceled) {
		safe.contextError = context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		safe.contextError = context.DeadlineExceeded
		safe.timedOut = true
	}
	return safe
}

func (c *Client) request(ctx context.Context, method, path string, input, output any, missingOK bool) (bool, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return false, err
		}
		if len(encoded) > maxBodyBytes {
			return false, errors.New("grafana request exceeds 16 MiB")
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return false, errors.New("could not create Grafana request")
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return false, transportError(err)
	}
	defer response.Body.Close()
	if response.StatusCode == 404 && missingOK {
		return false, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return false, &HTTPError{Method: method, Path: path, StatusCode: response.StatusCode}
	}
	if output == nil {
		return true, nil
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	if err != nil {
		return false, transportError(err)
	}
	if len(encoded) > maxBodyBytes {
		return false, errors.New("grafana response exceeds 16 MiB")
	}
	if err := json.Unmarshal(encoded, output); err != nil {
		return false, errors.New("grafana returned invalid JSON")
	}
	return true, nil
}

// Change is a reviewable comparison, containing no token or contact-point secrets.
type Change struct {
	Kind   string          `json:"kind"`
	UID    string          `json:"uid"`
	Action string          `json:"action"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after"`
}

type dashboardResponse struct {
	Dashboard map[string]any `json:"dashboard"`
	Meta      struct {
		FolderUID string `json:"folderUid"`
	} `json:"meta"`
}

// Diff performs all prerequisite and ownership checks without writing. The
// existing folder and datasource bindings must match before publication can begin.
func (c *Client) Diff(ctx context.Context, bundle Bundle) ([]Change, error) {
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	// Rendered definitions are valid offline before deployment choices exist.
	// Publication always needs resolved ownership, even while rules are paused.
	for _, rule := range bundle.Alerts {
		if strings.TrimSpace(rule.Annotations["owner"]) == "" || rule.Labels["team"] != rule.Annotations["owner"] {
			return nil, errors.New("resolve deployment configuration before publishing: each alert needs a consistent owner and team")
		}
	}
	if strings.TrimSuffix(bundle.Target.URL, "/") != c.baseURL {
		return nil, errors.New("bundle destination differs from configured Grafana URL")
	}
	var folder map[string]any
	if _, err := c.request(ctx, "GET", "/api/folders/"+url.PathEscape(bundle.Target.FolderUID), nil, &folder, false); err != nil {
		return nil, err
	}
	datasources := map[string]string{bundle.Target.PrometheusUID: "prometheus"}
	if bundle.Target.LokiUID != "" {
		datasources[bundle.Target.LokiUID] = "loki"
	}
	for uid, kind := range datasources {
		var datasource struct {
			Type string `json:"type"`
		}
		if _, err := c.request(ctx, "GET", "/api/datasources/uid/"+url.PathEscape(uid), nil, &datasource, false); err != nil {
			return nil, err
		}
		if datasource.Type != kind {
			return nil, fmt.Errorf("datasource %s is not %s", uid, kind)
		}
	}
	if len(bundle.Alerts) > 0 {
		var rules []json.RawMessage
		if _, err := c.request(ctx, "GET", "/api/v1/provisioning/alert-rules", nil, &rules, false); err != nil {
			return nil, fmt.Errorf("alert provisioning capability check failed: %w", err)
		}
	}
	if bundle.Target.EnableAlerts {
		var receivers []struct {
			Name                  string `json:"name"`
			Type                  string `json:"type"`
			DisableResolveMessage bool   `json:"disableResolveMessage"`
		}
		if _, err := c.request(ctx, "GET", "/api/v1/provisioning/contact-points", nil, &receivers, false); err != nil {
			return nil, err
		}
		configured := false
		for _, receiver := range receivers {
			if receiver.Name != bundle.Target.ContactPoint {
				continue
			}
			// A contact point may contain several integrations. Every selected
			// integration must be usable and send recovery notifications.
			if strings.TrimSpace(receiver.Type) == "" || receiver.DisableResolveMessage {
				return nil, errors.New("contact point has an integration without recovery notifications")
			}
			configured = true
		}
		if !configured {
			return nil, errors.New("contact point needs an integration with recovery notifications enabled")
		}
	}
	changes := []Change{}
	nativeCapabilityChecked := false
	for _, raw := range bundle.Dashboards {
		var desired map[string]any
		if err := json.Unmarshal(raw, &desired); err != nil {
			return nil, err
		}
		document, err := parseDashboard(desired)
		if err != nil {
			return nil, err
		}
		uid := document.uid()
		if document.native && !nativeCapabilityChecked {
			// A resource 404 cannot distinguish missing content from an unsupported
			// API. Check discovery before any classic or native resources are written.
			var discovery map[string]any
			if _, err := c.request(ctx, "GET", "/apis/"+nativeDashboardVersion, nil, &discovery, false); err != nil {
				return nil, err
			}
			nativeCapabilityChecked = true
		}
		current, folder, found, err := c.readDashboard(ctx, document, true)
		if err != nil {
			return nil, err
		}
		if found && (folder != bundle.Target.FolderUID || !hasDashboardOwner(current, "go-observe:"+owner(bundle.Namespace, bundle.Service, bundle.Target.Environment))) {
			return nil, fmt.Errorf("dashboard %s belongs to a different folder or owner", uid)
		}
		before, _ := json.Marshal(current)
		action := "create"
		if found {
			action = "update"
			if dashboardEqual(desired, current) {
				action = "unchanged"
			}
		} else {
			before = nil
		}
		changes = append(changes, Change{Kind: "dashboard", UID: uid, Action: action, Before: before, After: raw})
	}
	for _, rule := range bundle.Alerts {
		var current map[string]any
		found, err := c.request(ctx, "GET", "/api/v1/provisioning/alert-rules/"+url.PathEscape(rule.UID), nil, &current, true)
		if err != nil {
			return nil, err
		}
		if found {
			labels, _ := current["labels"].(map[string]any)
			if current["folderUID"] != bundle.Target.FolderUID || labels["go_observe_owner"] != owner(bundle.Namespace, bundle.Service, bundle.Target.Environment) {
				return nil, fmt.Errorf("alert %s belongs to a different folder or owner", rule.UID)
			}
		}
		after, _ := json.Marshal(rule)
		var desired map[string]any
		_ = json.Unmarshal(after, &desired)
		before, _ := json.Marshal(current)
		action := "create"
		if found {
			action = "update"
			if alertContainsContent(current, desired) {
				action = "unchanged"
			}
		} else {
			before = nil
		}
		changes = append(changes, Change{Kind: "alert", UID: rule.UID, Action: action, Before: before, After: after})
	}
	return changes, nil
}

// Apply preflights the entire bundle before the first write, then reads each
// changed resource back. Grafana has no transaction across dashboards/rules:
// a later failure can leave a partial release; rerun or apply the previous bundle.
// No DELETE operations or global notification-policy writes are performed.
func (c *Client) Apply(ctx context.Context, bundle Bundle) ([]Change, error) {
	changes, err := c.Diff(ctx, bundle)
	if err != nil {
		return nil, err
	}
	for _, change := range changes {
		if change.Action == "unchanged" {
			continue
		}
		if change.Kind == "dashboard" {
			var desired map[string]any
			_ = json.Unmarshal(change.After, &desired)
			document, err := parseDashboard(desired)
			if err != nil {
				return changes, err
			}
			if document.native {
				method, path := "POST", document.collectionPath()
				if change.Action == "update" {
					var previous map[string]any
					_ = json.Unmarshal(change.Before, &previous)
					prior, _ := parseDashboard(previous)
					version, _ := prior.metadata()["resourceVersion"].(string)
					if version == "" {
						return changes, errors.New("native dashboard needs resourceVersion for optimistic update")
					}
					document.metadata()["resourceVersion"] = version
					method, path = "PUT", document.resourcePath()
				}
				if _, err := c.request(ctx, method, path, desired, nil, false); err != nil {
					return changes, err
				}
			} else {
				if change.Action == "update" {
					var previous map[string]any
					_ = json.Unmarshal(change.Before, &previous)
					// Preserve optimistic version checks instead of overwriting concurrent edits.
					desired["version"] = previous["version"]
					desired["id"] = previous["id"]
				}
				if _, err := c.request(ctx, "POST", "/api/dashboards/db", map[string]any{"dashboard": desired, "folderUid": bundle.Target.FolderUID, "overwrite": false, "message": "Monitoring release " + bundle.Version}, nil, false); err != nil {
					return changes, err
				}
			}
			retained, folder, _, err := c.readDashboard(ctx, document, false)
			if err != nil {
				return changes, err
			}
			if folder != bundle.Target.FolderUID || !dashboardEqual(desired, retained) {
				return changes, fmt.Errorf("dashboard %s did not retain reviewed content", change.UID)
			}
		} else {
			method, path := "POST", "/api/v1/provisioning/alert-rules"
			if change.Action == "update" {
				method = "PUT"
				path += "/" + url.PathEscape(change.UID)
			}
			var desired map[string]any
			_ = json.Unmarshal(change.After, &desired)
			if _, err := c.request(ctx, method, path, desired, nil, false); err != nil {
				return changes, err
			}
			var retained map[string]any
			if _, err := c.request(ctx, "GET", "/api/v1/provisioning/alert-rules/"+url.PathEscape(change.UID), nil, &retained, false); err != nil {
				return changes, err
			}
			if !alertContainsContent(retained, desired) {
				return changes, fmt.Errorf("alert %s did not retain reviewed content", change.UID)
			}
		}
	}
	return changes, nil
}

func hasDashboardOwner(content map[string]any, tag string) bool {
	document, err := parseDashboard(content)
	return err == nil && hasTag(document.spec(), tag)
}

func dashboardEqual(expected, actual map[string]any) bool {
	desired, err := parseDashboard(expected)
	if err != nil {
		return false
	}
	retained, err := parseDashboard(actual)
	if err != nil || desired.native != retained.native || desired.uid() != retained.uid() {
		return false
	}
	if desired.native && (desired.namespace() != retained.namespace() || desired.folder() != retained.folder()) {
		return false
	}
	return reflect.DeepEqual(normalizeDashboard(desired.spec(), !desired.native), normalizeDashboard(retained.spec(), !retained.native))
}

// Compare both directions so fields removed from source cannot survive unnoticed.
// Only classic Grafana bookkeeping is ignored; arbitrary extra fields,
// including a retained nonempty panel timeFrom, are authored content differences.
func normalizeDashboard(content map[string]any, classic bool) map[string]any {
	normalized := maps.Clone(content)
	if classic {
		for _, field := range []string{"id", "version", "schemaVersion", "iteration"} {
			delete(normalized, field)
		}
	}
	return normalized
}

// alertContainsContent accounts for Grafana's duration formatting only at the
// top-level pending period. Query, label and annotation strings stay literal.
func alertContainsContent(actual, expected map[string]any) bool {
	actualPending, actualIsString := actual["for"].(string)
	expectedPending, expectedIsString := expected["for"].(string)
	if actualIsString && expectedIsString {
		// Grafana uses Prometheus durations, including day/week units that
		// time.ParseDuration does not support. Malformed values are not normalized.
		actualDuration, actualErr := model.ParseDuration(actualPending)
		expectedDuration, expectedErr := model.ParseDuration(expectedPending)
		if actualErr == nil && expectedErr == nil && actualDuration == expectedDuration {
			expected = maps.Clone(expected)
			expected["for"] = actualPending
		}
	}
	// Labels, annotations and routing are authored state. Subset comparison
	// would silently keep removed owners, guidance or receivers. Grafana's
	// top-level bookkeeping and query model defaults remain tolerated below.
	for _, field := range []string{"labels", "annotations", "notification_settings"} {
		actualFields, _ := actual[field].(map[string]any)
		expectedFields, _ := expected[field].(map[string]any)
		if len(actualFields) != len(expectedFields) || (len(expectedFields) > 0 && !reflect.DeepEqual(actualFields, expectedFields)) {
			return false
		}
	}
	return containsContent(actual, expected)
}

// containsContent tolerates server-added defaults, but compares every authored
// field and ordered array element. It never ignores queries, thresholds or scope.
func containsContent(actual, expected any) bool {
	switch expected := expected.(type) {
	case map[string]any:
		actual, ok := actual.(map[string]any)
		if !ok {
			return false
		}
		for key, value := range expected {
			if !containsContent(actual[key], value) {
				return false
			}
		}
		return true
	case []any:
		actual, ok := actual.([]any)
		if !ok || len(actual) != len(expected) {
			return false
		}
		for index, value := range expected {
			if !containsContent(actual[index], value) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(actual, expected)
	}
}
