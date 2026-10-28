package grafana

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/owenyeo/go-observe/slo"
)

func fixture(t *testing.T, baseURL string) Bundle {
	t.Helper()
	bundle, err := Render(Definition{Service: "checkout", Version: "0.1.0", ReplicaLabel: "instance", Objectives: []slo.Objective{{ID: "success", Name: "Request success", Target: .999, Window: 30 * 24 * time.Hour,
		SLI:      slo.Ratio{Total: slo.Selector{Metric: "requests_total"}, Good: slo.Selector{Metric: "requests_total", Labels: map[string]string{"result": "success"}}},
		Response: slo.Response{RunbookURL: "https://example.com/runbook", FirstCheck: "Check requests", Escalation: "Notify the service on-call"}}}},
		Target{Environment: "staging", URL: baseURL, FolderUID: "checkout", PrometheusUID: "prometheus", Scope: map[string]string{"service_name": "checkout", "environment": "staging"}})
	if err != nil {
		t.Fatal(err)
	}
	return resolvedFixture(t, bundle)
}

func TestRenderingIsDeterministicAndAlertsDoNotUseDashboardVariables(t *testing.T) {
	first := fixture(t, "https://example.com")
	second := fixture(t, "https://example.com")
	one, _ := json.Marshal(first)
	two, _ := json.Marshal(second)
	if string(one) != string(two) {
		t.Fatal("render changed without source changes")
	}
	if len(first.Alerts) != 2 || !first.Alerts[0].IsPaused {
		t.Fatal("unsafe alert defaults")
	}
	for _, rule := range first.Alerts {
		for _, query := range rule.Data {
			if expression, ok := query.Model["expr"].(string); ok && (strings.Contains(expression, "$") || !strings.Contains(expression, `environment="staging"`)) {
				t.Fatal("alert uses dynamic or wrong scope")
			}
		}
	}
	if !strings.Contains(string(first.Dashboards[0]), "replica:regex") {
		t.Fatal("replica drill-down absent")
	}
}

func TestAllOwnershipChecksHappenBeforeWritingAndApplyIsIdempotent(t *testing.T) {
	dashboards := map[string]map[string]any{}
	rules := map[string]map[string]any{}
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/folders/checkout":
			io.WriteString(w, `{"uid":"checkout"}`)
		case r.URL.Path == "/api/datasources/uid/prometheus":
			io.WriteString(w, `{"type":"prometheus"}`)
		case r.Method == "GET" && r.URL.Path == "/api/v1/provisioning/alert-rules":
			io.WriteString(w, `[]`)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/dashboards/uid/"):
			uid := strings.TrimPrefix(r.URL.Path, "/api/dashboards/uid/")
			content, exists := dashboards[uid]
			if !exists {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"dashboard": content, "meta": map[string]string{"folderUid": "checkout"}})
		case r.Method == "POST" && r.URL.Path == "/api/dashboards/db":
			var payload struct {
				Dashboard map[string]any `json:"dashboard"`
			}
			json.NewDecoder(r.Body).Decode(&payload)
			payload.Dashboard["id"] = float64(1)
			payload.Dashboard["version"] = float64(1)
			dashboards[payload.Dashboard["uid"].(string)] = payload.Dashboard
			writes++
			io.WriteString(w, `{}`)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/provisioning/alert-rules/"):
			uid := strings.TrimPrefix(r.URL.Path, "/api/v1/provisioning/alert-rules/")
			content, exists := rules[uid]
			if !exists {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(content)
		case (r.Method == "POST" || r.Method == "PUT") && strings.HasPrefix(r.URL.Path, "/api/v1/provisioning/alert-rules"):
			var payload map[string]any
			json.NewDecoder(r.Body).Decode(&payload)
			// Grafana retains equivalent pending periods in its canonical units.
			switch payload["for"] {
			case "120s":
				payload["for"] = "2m"
			case "300s":
				payload["for"] = "5m"
			case "600s":
				payload["for"] = "10m"
			}
			rules[payload["uid"].(string)] = payload
			writes++
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected API operation: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	bundle := fixture(t, server.URL)
	bundle.Alerts[0].For = "120s"
	bundle.Alerts[1].For = "300s"
	client, err := NewClient(server.URL, "synthetic-test-token", true)
	if err != nil {
		t.Fatal(err)
	}
	rules[bundle.Alerts[1].UID] = map[string]any{"folderUID": "checkout", "labels": map[string]any{"go_observe_owner": "someone-else"}}
	if _, err := client.Apply(context.Background(), bundle); err == nil || writes != 0 {
		t.Fatal("collision did not stop publication before writes")
	}
	delete(rules, bundle.Alerts[1].UID)
	if _, err := client.Apply(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	if writes != 3 {
		t.Fatalf("got %d writes, want 3", writes)
	}
	if _, err := client.Apply(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	if writes != 3 {
		t.Fatal("unchanged bundle was republished")
	}
	bundle.Alerts[0].For = "600s"
	changes, err := client.Apply(context.Background(), bundle)
	if err != nil || writes != 4 || changes[1].Action != "update" {
		t.Fatalf("changed pending period was not published: writes=%d, error=%v", writes, err)
	}
	changes, err = client.Apply(context.Background(), bundle)
	if err != nil || writes != 4 {
		t.Fatalf("updated pending period was republished: writes=%d, error=%v", writes, err)
	}
	for _, change := range changes {
		if change.Action != "unchanged" {
			t.Fatalf("repeated apply returned %s for %s", change.Action, change.UID)
		}
	}
}

func guardrailDefinition() Definition {
	return Definition{Service: "checkout", Version: "0.1.0", Guardrails: []Guardrail{{
		ID: "backlog", Name: "Queue backlog", Description: "Queued requests need attention.",
		Query: `sum(queued_requests{${scope}})`, Source: "prometheus", Operator: "gt",
		Threshold: 100, Pending: 2 * time.Minute, Severity: "warning",
		Response: slo.Response{RunbookURL: "https://example.com/runbook", FirstCheck: "Inspect queue age", Escalation: "Notify the service on-call"},
	}}}
}

func TestAlertPendingDurationComparison(t *testing.T) {
	for _, test := range []struct {
		name     string
		actual   any
		expected any
		want     bool
	}{
		{name: "zero", actual: "0s", expected: "0s", want: true},
		{name: "minutes", actual: "2m", expected: "120s", want: true},
		{name: "compound", actual: "2m30s", expected: "150s", want: true},
		{name: "hours", actual: "1h", expected: "3600s", want: true},
		{name: "days", actual: "1d", expected: "86400s", want: true},
		{name: "weeks", actual: "1w", expected: "604800s", want: true},
		{name: "years", actual: "1y", expected: "31536000s", want: true},
		{name: "changed duration", actual: "5m", expected: "120s"},
		{name: "malformed actual", actual: "unknown", expected: "0s"},
		{name: "malformed expected", actual: "0s", expected: "unknown"},
		{name: "empty", actual: "", expected: "0s"},
		{name: "missing", actual: nil, expected: "0s"},
		{name: "wrong type", actual: float64(120), expected: "120s"},
		{name: "negative", actual: "-2m", expected: "-120s"},
	} {
		t.Run(test.name, func(t *testing.T) {
			actual := map[string]any{"for": test.actual, "serverField": "ignored"}
			expected := map[string]any{"for": test.expected}
			if got := alertContainsContent(actual, expected); got != test.want {
				t.Fatalf("comparison = %v, want %v", got, test.want)
			}
			if actual["for"] != test.actual || expected["for"] != test.expected {
				t.Fatal("comparison mutated caller content")
			}
		})
	}
}

func TestAlertDurationComparisonPreservesOtherFields(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"annotation", func(rule map[string]any) { rule["annotations"].(map[string]any)["summary"] = "2m" }},
		{"nested for", func(rule map[string]any) { rule["annotations"].(map[string]any)["for"] = "2m" }},
		{"label", func(rule map[string]any) { rule["labels"].(map[string]any)["window"] = "2m" }},
		{"scope", func(rule map[string]any) { rule["labels"].(map[string]any)["environment"] = "production" }},
		{"query", func(rule map[string]any) { rule["data"].([]any)[0].(map[string]any)["expr"] = "vector(300)" }},
		{"threshold", func(rule map[string]any) { rule["data"].([]any)[0].(map[string]any)["threshold"] = float64(300) }},
		{"pause state", func(rule map[string]any) { rule["isPaused"] = false }},
		{"removed dashboard reference", func(rule map[string]any) {
			rule["annotations"].(map[string]any)["dashboard_url"] = "https://example.com/d/old"
		}},
		{"removed custom annotation", func(rule map[string]any) { rule["annotations"].(map[string]any)["guidance"] = "old guidance" }},
		{"removed custom label", func(rule map[string]any) { rule["labels"].(map[string]any)["customer_tier"] = "premium" }},
		{"removed receiver", func(rule map[string]any) { rule["notification_settings"] = map[string]any{"receiver": "old-sink"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			const content = `{"for":"120s","annotations":{"summary":"120s","for":"120s"},"labels":{"window":"120s","environment":"staging"},"data":[{"expr":"vector(120)","threshold":120}],"isPaused":true}`
			var actual, expected map[string]any
			if err := json.Unmarshal([]byte(content), &actual); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(content), &expected); err != nil {
				t.Fatal(err)
			}
			actual["for"] = "2m"
			test.mutate(actual)
			if alertContainsContent(actual, expected) {
				t.Fatal("duration normalization hid an authored field difference")
			}
		})
	}
}

func TestGuardrailDashboardLinks(t *testing.T) {
	target := fixture(t, "https://example.com/grafana/").Target
	custom := json.RawMessage(`{"uid":"custom-detail","title":"Queue detail","schemaVersion":41,"panels":[]}`)
	for _, test := range []struct {
		name           string
		overviewUID    string
		custom         bool
		wantUID        string
		wantDashboards int
	}{
		{name: "no dashboard"},
		{name: "existing dashboard reference", overviewUID: "existing-overview", wantUID: "existing-overview"},
		{name: "custom dashboard", custom: true, wantUID: "custom-detail", wantDashboards: 1},
		{name: "explicit reference takes precedence", overviewUID: "existing-overview", custom: true, wantUID: "existing-overview", wantDashboards: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition := guardrailDefinition()
			definition.OverviewUID = test.overviewUID
			if test.custom {
				definition.CustomDashboards = []json.RawMessage{custom}
			}
			bundle, err := Render(definition, target)
			if err != nil {
				t.Fatal(err)
			}
			if len(bundle.Dashboards) != test.wantDashboards {
				t.Fatalf("got %d publication dashboards, want %d", len(bundle.Dashboards), test.wantDashboards)
			}
			link, present := bundle.Alerts[0].Annotations["dashboard_url"]
			if test.wantUID == "" {
				if present {
					t.Fatalf("dashboard link must be omitted: %q", link)
				}
			} else if link != "https://example.com/grafana/d/"+test.wantUID {
				t.Fatalf("unexpected dashboard link: %q", link)
			}
			if test.custom && !strings.Contains(string(bundle.Dashboards[0]), `"uid":"custom-detail"`) {
				t.Fatal("existing reference replaced the declared custom dashboard")
			}
		})
	}
	for _, uid := range []string{"../other", "with spaces", "dashboard?edit", strings.Repeat("x", 41)} {
		definition := guardrailDefinition()
		definition.OverviewUID = uid
		if _, err := Render(definition, target); err == nil {
			t.Errorf("invalid existing dashboard UID accepted: %q", uid)
		}
	}
	// SLO definitions still link to the overview they generate.
	bundle := fixture(t, target.URL)
	want := "https://example.com/grafana/d/" + ResourceUID(bundle.Service, target.Environment, "dashboard", "overview")
	if bundle.Alerts[0].Annotations["dashboard_url"] != want {
		t.Fatal("generated overview link changed")
	}
}

func TestRedirectsCannotReceiveCredentialsAndErrorsDoNotExposeBodies(t *testing.T) {
	var leaked atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(r.Header.Get("Authorization") != "") }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer redirect.Close()
	client, _ := NewClient(redirect.URL, "private-token", true)
	if _, err := client.Diff(context.Background(), fixture(t, redirect.URL)); err == nil || leaked.Load() {
		t.Fatal("redirect followed")
	}
	// Caller-supplied redirect settings must not weaken credential protection.
	provided := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	client, _ = NewClientWithConfig(ClientConfig{URL: redirect.URL, Token: "private-token", AllowInsecure: true, HTTPClient: provided})
	if _, err := client.Diff(context.Background(), fixture(t, redirect.URL)); err == nil || leaked.Load() {
		t.Fatal("custom client allowed a credential-bearing redirect")
	}
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403); io.WriteString(w, "private-token") }))
	defer forbidden.Close()
	client, _ = NewClient(forbidden.URL, "private-token", true)
	_, err := client.Diff(context.Background(), fixture(t, forbidden.URL))
	if err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatal("credential-bearing error exposed")
	}
}

func TestBundleRejectsCrossFolderAndUnsafeActivation(t *testing.T) {
	bundle := fixture(t, "https://example.com")
	bundle.Alerts[0].FolderUID = "other"
	if bundle.Validate() == nil {
		t.Fatal("cross-folder rule accepted")
	}
	bundle = fixture(t, "https://example.com")
	bundle.Target.EnableAlerts = true
	if bundle.Validate() == nil {
		t.Fatal("activation without contact point accepted")
	}
}

func TestAlertQueryAllowsRegexAnchorsButRejectsDashboardVariables(t *testing.T) {
	query, err := scopedQuery(`sum(requests_total{${scope},result=~"^error$"})`, map[string]string{"environment": "staging"})
	if err != nil || !strings.Contains(query, `result=~"^error$"`) {
		t.Fatal("valid regex anchor was treated as a dashboard variable")
	}
	for _, variable := range []string{"$replica", "${replica:regex}", "$__rate_interval"} {
		if _, err := scopedQuery(`sum(requests_total{${scope},instance="`+variable+`"})`, map[string]string{"environment": "staging"}); err == nil {
			t.Fatal("dashboard variable accepted in alert query")
		}
	}
}
