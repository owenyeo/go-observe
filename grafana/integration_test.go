package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"
)

// TestLocalGrafana is deliberately opt-in and restricted to loopback. Never use
// real production credentials for SDK compatibility tests.
func TestLocalGrafana(t *testing.T) {
	baseURL := os.Getenv("OBSERVE_TEST_GRAFANA")
	if baseURL == "" {
		t.Skip("set OBSERVE_TEST_GRAFANA to the isolated compose instance")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost" && parsed.Hostname() != "::1") {
		t.Fatal("integration tests only permit loopback Grafana")
	}
	httpClient := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("redirect rejected") }}
	request := func(method, path string, input, output any) int {
		t.Helper()
		var body io.Reader
		if input != nil {
			encoded, _ := json.Marshal(input)
			body = bytes.NewReader(encoded)
		}
		r, err := http.NewRequest(method, baseURL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		r.SetBasicAuth("admin", "local-observe-demo")
		r.Header.Set("Content-Type", "application/json")
		response, err := httpClient.Do(r)
		if err != nil {
			t.Fatal("local Grafana unavailable")
		}
		defer response.Body.Close()
		if output != nil && response.StatusCode < 300 {
			if err := json.NewDecoder(response.Body).Decode(output); err != nil {
				t.Fatal(err)
			}
		}
		return response.StatusCode
	}
	var account struct {
		ID int `json:"id"`
	}
	if code := request("POST", "/api/serviceaccounts", map[string]any{"name": fmt.Sprintf("sdk-check-%d", time.Now().UnixNano()), "role": "Admin"}, &account); code != 201 {
		t.Fatalf("create local test identity: HTTP %d", code)
	}
	defer request("DELETE", fmt.Sprintf("/api/serviceaccounts/%d", account.ID), nil, nil)
	var credential struct {
		Key string `json:"key"`
	}
	if code := request("POST", fmt.Sprintf("/api/serviceaccounts/%d/tokens", account.ID), map[string]any{"name": "test", "secondsToLive": 600}, &credential); code != 200 {
		t.Fatalf("local test credential: HTTP %d", code)
	}
	if code := request("POST", "/api/folders", map[string]any{"uid": "checkout", "title": "SDK test"}, nil); code != 200 {
		t.Fatalf("local test folder: HTTP %d", code)
	}
	defer request("DELETE", "/api/folders/checkout?forceDeleteRules=true", nil, nil)
	bundle := fixture(t, baseURL)
	client, err := NewClient(baseURL, credential.Key, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Apply(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	changes, err := client.Diff(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if change.Action != "unchanged" {
			t.Fatalf("retained %s is not idempotent", change.Kind)
		}
	}
	bundle.Version = "0.1.1"
	bundle.Alerts[0].Annotations["first_check"] = "Check the changed runbook instruction."
	if _, err := client.Apply(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	// Removing a panel override must publish an update, then remain idempotent.
	var classic map[string]any
	_ = json.Unmarshal(bundle.Dashboards[0], &classic)
	panel := classic["panels"].([]any)[0].(map[string]any)
	panel["timeFrom"] = "6h"
	bundle.Dashboards[0], _ = json.Marshal(classic)
	if _, err := client.Apply(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	delete(panel, "timeFrom")
	bundle.Dashboards[0], _ = json.Marshal(classic)
	changes, err = client.Diff(context.Background(), bundle)
	if err != nil || changes[0].Action != "update" {
		t.Fatalf("removed override did not require update: %v", err)
	}
	if _, err := client.Apply(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	changes, err = client.Diff(context.Background(), bundle)
	if err != nil || changes[0].Action != "unchanged" {
		t.Fatalf("removed override not retained: %v", err)
	}

	// Exercise the same publisher against a real v2 resource and its API version.
	native := nativeFixture(t, bundle.Target)
	if _, err := client.Apply(context.Background(), native); err != nil {
		t.Fatal(err)
	}
	changes, err = client.Diff(context.Background(), native)
	if err != nil || changes[0].Action != "unchanged" {
		t.Fatalf("native publication not idempotent: %v", err)
	}
	var resource map[string]any
	_ = json.Unmarshal(native.Dashboards[0], &resource)
	spec := resource["spec"].(map[string]any)
	spec["description"] = "Changed diagnostic instructions."
	native.Dashboards[0], _ = json.Marshal(resource)
	if _, err := client.Apply(context.Background(), native); err != nil {
		t.Fatal(err)
	}
	changes, err = client.Diff(context.Background(), native)
	if err != nil || changes[0].Action != "unchanged" {
		t.Fatalf("native update not retained: %v", err)
	}
	queryOptions := spec["elements"].(map[string]any)["notes"].(map[string]any)["spec"].(map[string]any)["data"].(map[string]any)["spec"].(map[string]any)["queryOptions"].(map[string]any)
	queryOptions["timeFrom"] = "6h"
	native.Dashboards[0], _ = json.Marshal(resource)
	if _, err := client.Apply(context.Background(), native); err != nil {
		t.Fatal(err)
	}
	delete(queryOptions, "timeFrom")
	native.Dashboards[0], _ = json.Marshal(resource)
	changes, err = client.Diff(context.Background(), native)
	if err != nil || changes[0].Action != "update" {
		t.Fatalf("removed native override did not require update: %v", err)
	}
	if _, err := client.Apply(context.Background(), native); err != nil {
		t.Fatal(err)
	}
	changes, err = client.Diff(context.Background(), native)
	if err != nil || changes[0].Action != "unchanged" {
		t.Fatalf("removed native override not retained: %v", err)
	}
}
