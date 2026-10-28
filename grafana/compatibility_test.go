package grafana

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/owenyeo/go-observe/slo"
)

type testTransport func(*http.Request) (*http.Response, error)

func (transport testTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestCustomHTTPClientPreservesSafeErrorClassification(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			original := &http.Client{Timeout: 3 * time.Second, Transport: testTransport(func(*http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("private-token: %w", cause)
			})}
			client, err := NewClientWithConfig(ClientConfig{URL: "https://example.com", Token: "private-token", HTTPClient: original})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Diff(context.Background(), fixture(t, "https://example.com"))
			if !errors.Is(err, cause) || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("unsafe/unclassifiable error: %v", err)
			}
			var networkError net.Error
			if !errors.As(err, &networkError) || networkError.Timeout() != (cause == context.DeadlineExceeded) {
				t.Fatalf("timeout classification lost: %v", err)
			}
			if original.CheckRedirect != nil || original.Timeout != 3*time.Second || client.http.Timeout != original.Timeout {
				t.Fatal("caller HTTP settings mutated or ignored")
			}
		})
	}
}

func TestHTTPStatusErrorIsInspectable(t *testing.T) {
	client, err := NewClientWithConfig(ClientConfig{URL: "https://example.com", Token: "private-token", HTTPClient: &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Body: http.NoBody}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Diff(context.Background(), fixture(t, "https://example.com"))
	var status *HTTPError
	if !errors.As(err, &status) || status.StatusCode != 429 {
		t.Fatalf("status classification lost: %v", err)
	}
}

func TestReplicaQueriesPreserveBracesInRouteValues(t *testing.T) {
	definition := Definition{Service: "checkout", Version: "0.1.0", ReplicaLabel: "instance", Objectives: []slo.Objective{{
		ID: "success", Name: "Request success", Target: .999, Window: 30 * 24 * time.Hour,
		SLI:      slo.Ratio{Total: slo.Selector{Metric: "requests_total", Labels: map[string]string{"route": "/orders/{id}"}}, Good: slo.Selector{Metric: "requests_total", Labels: map[string]string{"route": "/orders/{id}", "result": "success"}}},
		Response: slo.Response{RunbookURL: "https://example.com/runbook", FirstCheck: "Inspect errors", Escalation: "Notify on-call"},
	}}}
	bundle, err := Render(definition, Target{Environment: "staging", URL: "https://example.com", FolderUID: "checkout", PrometheusUID: "prometheus", Scope: map[string]string{"environment": "staging"}})
	if err != nil {
		t.Fatal(err)
	}
	var dashboard map[string]any
	_ = json.Unmarshal(bundle.Dashboards[0], &dashboard)
	panels := dashboard["panels"].([]any)
	for _, panel := range panels[len(panels)-1].(map[string]any)["panels"].([]any) {
		query := panel.(map[string]any)["targets"].([]any)[0].(map[string]any)["expr"].(string)
		if !strings.Contains(query, `route="/orders/{id}"`) || !strings.Contains(query, `instance=~"${replica:regex}"`) {
			t.Fatalf("corrupted diagnostic query: %s", query)
		}
	}
	if definition.Objectives[0].SLI.Total.Regex != nil {
		t.Fatal("Render mutated caller selectors")
	}
}

func nativeFixture(t *testing.T, target Target) Bundle {
	t.Helper()
	raw, err := os.ReadFile("testdata/native-dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := Render(Definition{Service: "checkout", Version: "0.1.0", CustomDashboards: []json.RawMessage{raw}}, target)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestNativeDashboardScopeAndIdentity(t *testing.T) {
	target := fixture(t, "https://example.com").Target
	target.GrafanaNamespace = "stacks-123"
	bundle := nativeFixture(t, target)
	encoded, err := json.Marshal(bundle.Target)
	if err != nil || !strings.Contains(string(encoded), `"grafanaNamespace":"stacks-123"`) || strings.Contains(string(encoded), `"namespace":`) {
		t.Fatal("Grafana API namespace was serialized as the logical namespace")
	}
	var resource map[string]any
	_ = json.Unmarshal(bundle.Dashboards[0], &resource)
	document, err := parseDashboard(resource)
	if err != nil || document.namespace() != target.GrafanaNamespace || document.folder() != target.FolderUID {
		t.Fatalf("native target not retained: %v", err)
	}
	document.metadata()["namespace"] = "other"
	bundle.Dashboards[0], _ = json.Marshal(resource)
	if bundle.Validate() == nil {
		t.Fatal("cross-namespace resource accepted")
	}
}

func TestDashboardComparisonDetectsRemovedContent(t *testing.T) {
	for _, native := range []bool{false, true} {
		bundle := fixture(t, "https://example.com")
		if native {
			bundle = nativeFixture(t, bundle.Target)
		}
		var expected, actual map[string]any
		_ = json.Unmarshal(bundle.Dashboards[0], &expected)
		_ = json.Unmarshal(bundle.Dashboards[0], &actual)
		document, _ := parseDashboard(actual)
		if native {
			document.spec()["elements"].(map[string]any)["notes"].(map[string]any)["spec"].(map[string]any)["data"].(map[string]any)["spec"].(map[string]any)["queryOptions"].(map[string]any)["timeFrom"] = "6h"
		} else {
			document.spec()["panels"].([]any)[0].(map[string]any)["timeFrom"] = "6h"
		}
		if dashboardEqual(expected, actual) {
			t.Fatal("removed time override was ignored")
		}
	}
}

func TestNativePreflightRejectsUnsupportedAPIAndOwnershipCollisions(t *testing.T) {
	for _, problem := range []string{"unsupported API", "different owner", "different folder", "missing resourceVersion"} {
		t.Run(problem, func(t *testing.T) {
			target := fixture(t, "https://example.com").Target
			bundle := nativeFixture(t, target)
			var current map[string]any
			_ = json.Unmarshal(bundle.Dashboards[0], &current)
			document, _ := parseDashboard(current)
			document.metadata()["resourceVersion"] = "1"
			switch problem {
			case "different owner":
				document.spec()["tags"] = []any{"go-observe:another:staging"}
			case "different folder":
				document.metadata()["annotations"].(map[string]any)["grafana.app/folder"] = "other"
			case "missing resourceVersion":
				delete(document.metadata(), "resourceVersion")
			}
			writes := 0
			client, err := NewClientWithConfig(ClientConfig{URL: target.URL, Token: "synthetic-token", HTTPClient: &http.Client{Transport: testTransport(func(request *http.Request) (*http.Response, error) {
				if request.Method != "GET" {
					writes++
					return nil, errors.New("unexpected write")
				}
				status, body := 200, `{}`
				switch request.URL.Path {
				case "/api/folders/checkout":
					body = `{"uid":"checkout"}`
				case "/api/datasources/uid/prometheus":
					body = `{"type":"prometheus"}`
				case "/apis/" + nativeDashboardVersion:
					if problem == "unsupported API" {
						status = 404
					}
				case document.resourcePath():
					encoded, _ := json.Marshal(current)
					body = string(encoded)
				default:
					return nil, fmt.Errorf("unexpected path %s", request.URL.Path)
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Apply(context.Background(), bundle); err == nil || writes != 0 {
				t.Fatal("native preflight permitted a write")
			}
		})
	}
}
