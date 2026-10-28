package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func deploymentConfig(t *testing.T, namespace string, enabled bool, receiver string) DeploymentConfiguration {
	t.Helper()
	content := fmt.Sprintf(`[{"namespace":%q,"service":"checkout","environment":"staging","enableAlerts":%t,"owner":"oncall","contactPoint":%q}]`, namespace, enabled, receiver)
	config, err := ReadDeploymentConfiguration(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return config
}

// Publication fixtures always resolve ownership through the same public path
// consumers use. Render-only tests can still inspect unresolved definitions.
func resolvedFixture(t *testing.T, definitions Bundle) Bundle {
	t.Helper()
	bundle, err := deploymentConfig(t, definitions.Namespace, false, "").Resolve(definitions, definitions.Namespace, definitions.Target.Environment)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestConfigurationRejectsDuplicateTuples(t *testing.T) {
	config := deploymentConfig(t, "commerce", false, "")
	other := config[0]
	other.Namespace = "support"
	config = append(config, other)
	if err := config.Validate(); err != nil {
		t.Fatal("same service in another namespace was rejected:", err)
	}
	duplicate := config[0]
	duplicate.Owner = "another-team"
	duplicate.EnableAlerts = new(true)
	duplicate.ContactPoint = "other-sink"
	config = append(config, duplicate)
	encoded, _ := json.Marshal(config)
	if _, err := ReadDeploymentConfiguration(strings.NewReader(string(encoded))); err == nil {
		t.Fatal("duplicate tuple with conflicting settings accepted")
	}
	if _, err := config.Resolve(fixture(t, "https://example.com"), "", "staging"); err == nil {
		t.Fatal("library caller bypassed duplicate-tuple validation")
	}
}

func TestOwnershipComesOnlyFromDeploymentConfiguration(t *testing.T) {
	target := fixture(t, "https://example.com").Target
	definitions, err := Render(guardrailDefinition(), target)
	if err != nil {
		t.Fatal(err)
	}
	if definitions.Alerts[0].Annotations["owner"] != "" || definitions.Alerts[0].Labels["team"] != "" {
		t.Fatal("definition supplied a second source of human ownership")
	}
	client, err := NewClientWithConfig(ClientConfig{URL: target.URL, Token: "synthetic-token", HTTPClient: &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("unresolved ownership reached the network")
		return nil, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Apply(context.Background(), definitions); err == nil {
		t.Fatal("publication accepted unresolved ownership")
	}
	config := deploymentConfig(t, "", false, "")
	resolved, err := config.Resolve(definitions, "", "staging")
	if err != nil {
		t.Fatal(err)
	}
	config[0].Owner = "replacement-oncall"
	updated, err := config.Resolve(resolved, "", "staging")
	if err != nil || updated.Alerts[0].Annotations["owner"] != "replacement-oncall" || updated.Alerts[0].Labels["team"] != "replacement-oncall" {
		t.Fatalf("configuration did not own both published fields: %v", err)
	}
	if definitions.Alerts[0].Annotations["owner"] != "" || resolved.Alerts[0].Annotations["owner"] != "oncall" {
		t.Fatal("ownership change mutated an earlier artifact")
	}
	target.EnableAlerts = true
	target.ContactPoint = "legacy-source-route"
	if _, err := Render(guardrailDefinition(), target); err == nil {
		t.Fatal("definition renderer accepted deployment choices")
	}
}

func namespacedFixture(t *testing.T, namespace string, target Target) Bundle {
	t.Helper()
	definition := guardrailDefinition()
	definition.Namespace = namespace
	first := definition.Guardrails[0]
	definition.Guardrails = nil
	for index := 0; index < 6; index++ {
		rule := first
		rule.ID = fmt.Sprintf("queue-%d", index)
		definition.Guardrails = append(definition.Guardrails, rule)
	}
	target.Scope = maps.Clone(target.Scope)
	target.Scope["service_namespace"] = definition.Namespace
	bundle, err := Render(definition, target)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestConfigurationResolvesSixRulesWithoutChangingDefinitions(t *testing.T) {
	bundle := namespacedFixture(t, "commerce", fixture(t, "https://example.com").Target)
	original, _ := json.Marshal(bundle)
	for _, enabled := range []bool{true, false} {
		config := deploymentConfig(t, bundle.Namespace, enabled, "local-sink")
		resolved, err := config.Resolve(bundle, bundle.Namespace, "staging")
		if err != nil {
			t.Fatal(err)
		}
		for index, rule := range resolved.Alerts {
			if rule.IsPaused == enabled || rule.Labels["team"] != "oncall" || rule.Annotations["owner"] != "oncall" || rule.NotificationSettings["receiver"] != "local-sink" {
				t.Fatalf("inconsistent resolved alert: %+v", rule)
			}
			before := bundle.Alerts[index]
			if rule.UID != before.UID || rule.For != before.For || rule.Labels["go_observe_owner"] != before.Labels["go_observe_owner"] || !reflect.DeepEqual(rule.Data, before.Data) || rule.Annotations["runbook_url"] != before.Annotations["runbook_url"] {
				t.Fatal("configuration changed definition content or resource ownership")
			}
		}
		again, err := config.Resolve(bundle, bundle.Namespace, "staging")
		if err != nil || !reflect.DeepEqual(resolved, again) {
			t.Fatal("resolution is not deterministic")
		}
	}
	retained, _ := json.Marshal(bundle)
	if string(original) != string(retained) {
		t.Fatal("resolution mutated the definition artifact")
	}
	config := deploymentConfig(t, bundle.Namespace, false, "")
	paused, err := config.Resolve(bundle, bundle.Namespace, "staging")
	if err != nil || paused.Target.ContactPoint != "" || len(paused.Alerts[0].NotificationSettings) != 0 {
		t.Fatal("paused configuration did not remove routing")
	}
}

func TestNamespacesIsolateIdentityScopeAndConfiguration(t *testing.T) {
	definition := guardrailDefinition()
	target := fixture(t, "https://example.com").Target
	var bundles []Bundle
	for _, namespace := range []string{"commerce", "support"} {
		definition.Namespace = namespace
		target.Scope = maps.Clone(target.Scope)
		target.Scope["service_namespace"] = namespace
		bundle, err := Render(definition, target)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(bundle.Alerts[0].Data[0].Model["expr"].(string), `service_namespace="`+namespace+`"`) {
			t.Fatal("namespace missing from query scope")
		}
		bundles = append(bundles, bundle)
	}
	one, two := bundles[0].Alerts[0], bundles[1].Alerts[0]
	if one.UID == two.UID || one.RuleGroup == two.RuleGroup || one.Labels["go_observe_owner"] == two.Labels["go_observe_owner"] {
		t.Fatal("namespaces share resource identity")
	}
	config := deploymentConfig(t, "commerce", true, "local-sink")
	for _, selection := range [][2]string{{"support", "staging"}, {"commerce", "production"}} {
		if _, err := config.Resolve(bundles[0], selection[0], selection[1]); err == nil {
			t.Fatal("cross-scope retargeting accepted")
		}
	}
	if _, err := config.Resolve(bundles[1], "support", "staging"); err == nil {
		t.Fatal("missing exact configuration inherited another namespace")
	}
	definition.Namespace = "commerce"
	target.Scope["service_namespace"] = "support"
	if _, err := Render(definition, target); err == nil {
		t.Fatal("mismatched query namespace accepted")
	}
	const legacyUID = "observe-053d34cb017cb307f45f42fc"
	if ResourceUID("checkout", "staging", "alert", "backlog") != legacyUID || NamespacedResourceUID("", "checkout", "staging", "alert", "backlog") != legacyUID {
		t.Fatal("legacy IDs changed")
	}
}

func TestDashboardExportsCannotRetainAnotherNamespaceOwner(t *testing.T) {
	definition := Definition{Namespace: "commerce", Service: "checkout", Version: "0.1.0", CustomDashboards: []json.RawMessage{
		json.RawMessage(`{"uid":"custom","title":"Queue","tags":["queues","go-observe:support:checkout:staging","go-observe:commerce:checkout:staging"],"panels":[]}`),
	}}
	target := fixture(t, "https://example.com").Target
	target.Scope["service_namespace"] = definition.Namespace
	bundle, err := Render(definition, target)
	if err != nil {
		t.Fatal(err)
	}
	var dashboard map[string]any
	_ = json.Unmarshal(bundle.Dashboards[0], &dashboard)
	if !reflect.DeepEqual(dashboard["tags"], []any{"queues", "go-observe:commerce:checkout:staging"}) {
		t.Fatal("export retained ambiguous ownership tags")
	}
}

func TestConfigurationRejectsInvalidSettings(t *testing.T) {
	valid, _ := json.Marshal(deploymentConfig(t, "commerce", true, "local-sink"))
	for _, input := range []string{
		`{}`, `[]`, `null`, string(valid) + `{}`,
		strings.Replace(string(valid), `"enableAlerts":true`, `"enableAlerts":null`, 1),
		strings.Replace(string(valid), `"enableAlerts":true,`, "", 1),
		strings.Replace(string(valid), `"owner":"oncall"`, `"owner":""`, 1),
		strings.Replace(string(valid), `"contactPoint":"local-sink"`, `"contactPoint":""`, 1),
		strings.Replace(string(valid), `"owner":"oncall"`, `"token":"secret"`, 1),
	} {
		if _, err := ReadDeploymentConfiguration(strings.NewReader(input)); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}

func TestInvalidArtifactsFailBeforeNetworkAccess(t *testing.T) {
	for _, mutate := range []func(*Rule){
		func(rule *Rule) { rule.For = "not-a-duration" },
		func(rule *Rule) { rule.For = "-1s" },
		func(rule *Rule) { rule.For = "500ms" },
		func(rule *Rule) { rule.Title = " " },
		func(rule *Rule) { rule.Condition = "missing" },
		func(rule *Rule) { rule.Data = nil },
		func(rule *Rule) { rule.Data = append(rule.Data, rule.Data[0]) },
		func(rule *Rule) { rule.Data[0].Model = nil },
		func(rule *Rule) { rule.Data[0].DatasourceUID = "" },
		func(rule *Rule) { rule.Data[0].Model["expr"] = "" },
		func(rule *Rule) { rule.Data[0].RelativeTimeRange["from"] = -1 },
	} {
		bundle := fixture(t, "https://example.com")
		mutate(&bundle.Alerts[0])
		client, err := NewClientWithConfig(ClientConfig{URL: bundle.Target.URL, Token: "synthetic-token", HTTPClient: &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid artifact reached the network")
			return nil, nil
		})}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Apply(context.Background(), bundle); err == nil {
			t.Fatal("invalid artifact accepted")
		}
	}
}

func TestActivationChecksEveryReceiverIntegrationBeforeWrites(t *testing.T) {
	for _, receivers := range []string{`[]`, `[{"name":"sink","type":"webhook","disableResolveMessage":true}]`, `[{"name":"sink","type":""}]`, `[{"name":"sink","type":"webhook"},{"name":"sink","type":"email","disableResolveMessage":true}]`} {
		bundle, err := deploymentConfig(t, "", true, "sink").Resolve(fixture(t, "https://example.com"), "", "staging")
		if err != nil {
			t.Fatal(err)
		}
		client, err := NewClientWithConfig(ClientConfig{URL: bundle.Target.URL, Token: "synthetic-token", HTTPClient: &http.Client{Transport: testTransport(func(request *http.Request) (*http.Response, error) {
			if request.Method != "GET" {
				t.Fatal("invalid receiver allowed writes")
			}
			body := `{}`
			switch request.URL.Path {
			case "/api/datasources/uid/prometheus":
				body = `{"type":"prometheus"}`
			case "/api/v1/provisioning/alert-rules":
				body = `[]`
			case "/api/v1/provisioning/contact-points":
				body = receivers
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Apply(context.Background(), bundle); err == nil {
			t.Fatal("invalid receiver accepted")
		}
	}
}
