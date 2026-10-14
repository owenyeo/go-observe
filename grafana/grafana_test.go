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
		Response: slo.Response{Owner: "service-team", RunbookURL: "https://example.com/runbook", FirstCheck: "Check requests", Escalation: "Notify the service on-call"}}}},
		Target{Environment: "staging", URL: baseURL, FolderUID: "checkout", PrometheusUID: "prometheus", Scope: map[string]string{"service_name": "checkout", "environment": "staging"}})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
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
