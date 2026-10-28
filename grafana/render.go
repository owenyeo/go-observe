// Package grafana generates reviewed monitoring assets independently of serving.
// Runtime instrumentation does not import this package or need Grafana credentials.
package grafana

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/owenyeo/go-observe/slo"
)

// A regex end anchor ($) is valid PromQL; only Grafana variable syntax is forbidden.
var dashboardVariable = regexp.MustCompile(`\$(?:\{|[A-Za-z_])`)

// Definition belongs in the consuming repository as Go source. Version identifies
// a monitoring release separately from the application's binary version.
// Definitions and targets must remain immutable while Render runs.
type Definition struct {
	// Namespace isolates identically named services. Empty preserves pre-v0.1.2 IDs.
	Namespace string
	Service   string
	Version   string
	// OverviewUID selects the generated SLO overview's UID. Without objectives,
	// it only links alerts; it does not add that dashboard to the bundle.
	// When empty, alerts link to the first dashboard in the bundle, if any.
	OverviewUID      string
	Objectives       []slo.Objective
	Guardrails       []Guardrail
	CustomDashboards []json.RawMessage // classic JSON or dashboard.grafana.app/v2 resources.
	ReplicaLabel     string
}

// Guardrail is an operational threshold, separate from an SLO's error budget.
type Guardrail struct {
	ID          string
	Name        string
	Description string
	Query       string // PromQL or LogQL with ${scope} inside a selector.
	Source      string // prometheus or loki.
	Operator    string // gt or lt.
	Threshold   float64
	Pending     time.Duration
	Severity    string
	Response    slo.Response
}

var releaseVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var resourceID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,40}$`)

// ResourceUID derives a stable identifier without embedding deployment names.
func ResourceUID(service, environment, kind, id string) string {
	return NamespacedResourceUID("", service, environment, kind, id)
}

// NamespacedResourceUID isolates resources across logical service groups. An empty
// namespace preserves the original ResourceUID contract for existing deployments.
func NamespacedResourceUID(namespace, service, environment, kind, id string) string {
	identity := service + "\x00" + environment + "\x00" + kind + "\x00" + id
	if namespace != "" {
		identity = namespace + "\x00" + identity
	}
	hash := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("observe-%x", hash[:12])
}

func owner(namespace, service, environment string) string {
	if namespace != "" {
		return namespace + ":" + service + ":" + environment
	}
	return service + ":" + environment
}

// Render is deterministic and offline. Its output is safe to commit for review;
// endpoint and credential configuration is supplied separately by CI.
func Render(def Definition, target Target) (Bundle, error) {
	bundle := Bundle{SchemaVersion: 1, Namespace: def.Namespace, Service: def.Service, Version: def.Version, Target: target,
		Dashboards: []json.RawMessage{}, Alerts: []Rule{}}
	if !slo.ValidID(def.Service) || !slo.ValidID(target.Environment) || !releaseVersion.MatchString(def.Version) {
		return Bundle{}, errors.New("service/environment need stable lowercase IDs; monitoring version needs MAJOR.MINOR.PATCH")
	}
	if !resourceID.MatchString(target.FolderUID) || !resourceID.MatchString(target.PrometheusUID) || len(target.Scope) == 0 {
		return Bundle{}, errors.New("target needs a folder, Prometheus datasource and explicit query scope")
	}
	if def.OverviewUID != "" && !resourceID.MatchString(def.OverviewUID) {
		return Bundle{}, errors.New("overview UID must contain 1–40 letters, digits, underscores or hyphens")
	}
	if target.EnableAlerts || target.ContactPoint != "" {
		return Bundle{}, errors.New("render produces paused definitions; resolve activation and routing through deployment configuration")
	}
	seen := map[string]bool{}
	for _, objective := range def.Objectives {
		if err := objective.Validate(); err != nil {
			return Bundle{}, fmt.Errorf("SLO %s: %w", objective.ID, err)
		}
		if seen[objective.ID] {
			return Bundle{}, errors.New("duplicate objective ID")
		}
		seen[objective.ID] = true
		for _, policy := range objective.Alerts() {
			expression, err := objective.BurnExpression(policy, target.Scope)
			if err != nil {
				return Bundle{}, err
			}
			rule := newRule(def.Namespace, def.Service, target, objective.ID+"-"+policy.ID, objective.Name+" — "+policy.Name, "prometheus", expression, "gt", 0, policy.Pending, policy.Severity, objective.Response)
			rule.Annotations["slo_id"] = objective.ID
			rule.Annotations["description"] = fmt.Sprintf("%s Target %.4g%% over %s; both %s and %s windows exceed the budget-consumption threshold. Inspect error_fraction={{ $values.C.Value }} and eligible_events={{ $values.D.Value }}.", objective.Description, 100*objective.Target, objective.Window, policy.LongWindow, policy.ShortWindow)
			total, _, ratio, _ := objective.SLI.Queries(policy.ShortWindow, target.Scope)
			rule.Data = append(rule.Data, sourceQuery("C", "prometheus", target.PrometheusUID, "1 - "+ratio, int64(policy.LongWindow/time.Second)), sourceQuery("D", "prometheus", target.PrometheusUID, total, int64(policy.LongWindow/time.Second)))
			bundle.Alerts = append(bundle.Alerts, rule)
		}
	}
	seen = map[string]bool{}
	for _, guardrail := range def.Guardrails {
		if !slo.ValidID(guardrail.ID) || seen[guardrail.ID] || guardrail.Name == "" || guardrail.Severity == "" || !strings.Contains(guardrail.Query, "${scope}") {
			return Bundle{}, errors.New("guardrail needs a unique ID, name, severity and ${scope} in its query")
		}
		seen[guardrail.ID] = true
		if guardrail.Source != "prometheus" && guardrail.Source != "loki" {
			return Bundle{}, errors.New("guardrail source must be prometheus or loki")
		}
		if guardrail.Source == "loki" && !resourceID.MatchString(target.LokiUID) {
			return Bundle{}, errors.New("Loki guardrails need a Loki datasource UID")
		}
		if guardrail.Operator != "gt" && guardrail.Operator != "lt" {
			return Bundle{}, errors.New("guardrail operator must be gt or lt")
		}
		if math.IsNaN(guardrail.Threshold) || math.IsInf(guardrail.Threshold, 0) || guardrail.Pending < 0 || guardrail.Pending%time.Second != 0 {
			return Bundle{}, errors.New("invalid guardrail threshold/pending period")
		}
		if err := guardrail.Response.Validate(); err != nil {
			return Bundle{}, err
		}
		query, err := scopedQuery(guardrail.Query, target.Scope)
		if err != nil {
			return Bundle{}, err
		}
		rule := newRule(def.Namespace, def.Service, target, "guardrail-"+guardrail.ID, guardrail.Name, guardrail.Source, query, guardrail.Operator, guardrail.Threshold, guardrail.Pending, guardrail.Severity, guardrail.Response)
		rule.Annotations["description"] = guardrail.Description + " Observed={{ $values.A.Value }}; threshold=" + fmt.Sprint(guardrail.Threshold) + "; assess customer impact before assigning incident priority."
		bundle.Alerts = append(bundle.Alerts, rule)
	}
	if len(def.Objectives) > 0 {
		dash, err := overview(def, target)
		if err != nil {
			return Bundle{}, err
		}
		bundle.Dashboards = append(bundle.Dashboards, dash)
	}
	for _, raw := range def.CustomDashboards {
		var content map[string]any
		if err := json.Unmarshal(raw, &content); err != nil {
			return Bundle{}, fmt.Errorf("custom dashboard: %w", err)
		}
		document, err := parseDashboard(content)
		if err != nil {
			return Bundle{}, err
		}
		if document.native {
			if target.GrafanaNamespace != "" {
				document.metadata()["namespace"] = target.GrafanaNamespace
			}
			// Deployment metadata comes from the reviewed target, not a saved export.
			content["metadata"] = map[string]any{"name": document.uid(), "namespace": document.metadata()["namespace"],
				"annotations": map[string]any{"grafana.app/folder": target.FolderUID}}
		}
		tags, _ := document.spec()["tags"].([]any)
		// An exported dashboard must not retain another namespace's ownership.
		// Ordinary tags remain; the reviewed definition supplies one owner tag.
		ownedTags := make([]any, 0, len(tags)+1)
		for _, tag := range tags {
			value, _ := tag.(string)
			if !strings.HasPrefix(value, "go-observe:") {
				ownedTags = append(ownedTags, tag)
			}
		}
		document.spec()["tags"] = append(ownedTags, "go-observe:"+owner(def.Namespace, def.Service, target.Environment))
		encoded, err := json.Marshal(content)
		if err != nil {
			return Bundle{}, err
		}
		bundle.Dashboards = append(bundle.Dashboards, encoded)
	}
	dashboardUID := def.OverviewUID
	if dashboardUID == "" && len(bundle.Dashboards) > 0 {
		var first map[string]any
		_ = json.Unmarshal(bundle.Dashboards[0], &first)
		document, _ := parseDashboard(first)
		dashboardUID = document.uid()
	}
	if dashboardUID != "" {
		for index := range bundle.Alerts {
			bundle.Alerts[index].Annotations["dashboard_url"] = strings.TrimSuffix(target.URL, "/") + "/d/" + dashboardUID
		}
	}
	if err := bundle.Validate(); err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}

func scopedQuery(query string, scope map[string]string) (string, error) {
	selector, err := (slo.Selector{Metric: "scope", Labels: scope}).Expression(nil)
	if err != nil {
		return "", err
	}
	query = strings.ReplaceAll(query, "${scope}", strings.TrimSuffix(strings.TrimPrefix(selector, "scope{"), "}"))
	if dashboardVariable.MatchString(query) {
		return "", errors.New("alert queries cannot depend on dashboard variables")
	}
	return query, nil
}

func sourceQuery(id, source, uid, expression string, lookback int64) Query {
	query := Query{
		RefID:             id,
		DatasourceUID:     uid,
		RelativeTimeRange: map[string]int64{"from": lookback, "to": 0},
		Model: map[string]any{
			"refId": id, "expr": expression,
			"instant": true, "range": false,
			"intervalMs": 1000, "maxDataPoints": 43200,
			"datasource": map[string]string{"type": source, "uid": uid},
		},
	}
	if source == "loki" {
		query.QueryType = "instant"
		query.Model["queryType"] = "instant"
	}
	return query
}

func newRule(namespace, service string, target Target, id, name, source, expression, operator string, threshold float64, pending time.Duration, severity string, response slo.Response) Rule {
	uid := target.PrometheusUID
	if source == "loki" {
		uid = target.LokiUID
	}
	// Grafana evaluates the data-source result A through a threshold expression B.
	// Keep the expression model separate from routing and response metadata.
	condition := map[string]any{
		"type":      "query",
		"evaluator": map[string]any{"type": operator, "params": []float64{threshold}},
		"operator":  map[string]string{"type": "and"},
		"query":     map[string]any{"params": []string{"B"}},
		"reducer":   map[string]any{"type": "last", "params": []any{}},
	}
	thresholdQuery := Query{
		RefID: "B", DatasourceUID: "__expr__",
		RelativeTimeRange: map[string]int64{"from": 0, "to": 0},
		Model: map[string]any{
			"refId": "B", "type": "threshold", "expression": "A",
			"datasource": map[string]string{"uid": "__expr__", "type": "__expr__"},
			"conditions": []any{condition},
		},
	}
	rule := Rule{
		UID:          NamespacedResourceUID(namespace, service, target.Environment, "alert", id),
		Title:        service + " / " + target.Environment + " / " + name,
		FolderUID:    target.FolderUID,
		RuleGroup:    NamespacedResourceUID(namespace, service, target.Environment, "group", "alerts"),
		Condition:    "B",
		For:          fmt.Sprint(int64(pending/time.Second)) + "s",
		NoDataState:  "NoData",
		ExecErrState: "Error",
		IsPaused:     true,
		Labels: map[string]string{
			"service_name": service, "environment": target.Environment,
			"severity": severity, "alert_id": id,
			"go_observe_owner": owner(namespace, service, target.Environment),
		},
		Annotations: map[string]string{
			"summary":     name,
			"first_check": response.FirstCheck,
			"runbook_url": response.RunbookURL,
			"escalation":  response.Escalation,
		},
		Data: []Query{sourceQuery("A", source, uid, expression, 21600), thresholdQuery},
	}
	if namespace != "" {
		rule.Labels["service_namespace"] = namespace
		rule.Title = namespace + " / " + rule.Title
	}
	return rule
}
