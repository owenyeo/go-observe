// Generate promtool's native test format as JSON (also valid YAML). These tests
// execute real PromQL rather than merely comparing generated query strings.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/owenyeo/go-observe/grafana"
	"github.com/owenyeo/go-observe/slo"
)

func main() {
	out := flag.String("out", "artifacts/promql-test.json", "promtool test file")
	flag.Parse()
	objective := slo.Objective{ID: "success", Name: "Success", Target: .999, Window: 30 * 24 * time.Hour,
		SLI: slo.Ratio{Total: slo.Selector{Metric: "requests_total"}, Good: slo.Selector{Metric: "requests_total", Labels: map[string]string{"result": "success"}}}}
	scope := map[string]string{"service_name": "checkout", "environment": "staging"}
	burn, err := objective.BurnExpression(slo.DefaultBurnAlerts()[0], scope)
	if err != nil {
		fail(err)
	}
	_, _, ratio, err := objective.SLI.Queries(time.Hour, scope)
	if err != nil {
		fail(err)
	}
	series := func(result, instance, values string) map[string]any {
		return map[string]any{"series": fmt.Sprintf(`requests_total{service_name="checkout",environment="staging",result=%q,instance=%q}`, result, instance), "values": values}
	}
	expected := func(value float64) []any { return []any{map[string]any{"labels": "{}", "value": value}} }
	tests := []any{}
	add := func(name string, inputs []any, query string, samples []any) {
		tests = append(tests, map[string]any{"name": name, "interval": "1m", "input_series": inputs, "promql_expr_test": []any{map[string]any{"expr": query, "eval_time": "6h", "exp_samples": samples}}})
	}
	add("healthy does not burn budget", []any{series("success", "one", "0+100x400")}, burn, expected(0))
	add("sustained errors fire fast burn", []any{series("success", "one", "0+98x400"), series("failure", "one", "0+2x400")}, burn, expected(1))
	add("replica ratios are weighted by events", []any{series("success", "busy", "0+990x400"), series("failure", "busy", "0+10x400"), series("failure", "quiet", "0+10x400")}, "round("+ratio+", 0.000001)", expected(.980198))
	add("counter reset preserves success ratio", []any{series("success", "one", "0+100x180 0+100x220")}, ratio, expected(1))
	add("zero traffic does not fire budget alert", []any{series("success", "one", "0+0x400"), series("failure", "one", "0+0x400")}, burn, expected(0))
	add("missing telemetry stays absent", []any{}, burn, []any{})
	add("another environment cannot contribute", []any{map[string]any{"series": `requests_total{service_name="checkout",environment="production",result="failure"}`, "values": "0+100x400"}}, burn, []any{})
	// Run the dashboard's actual diagnostic query through Prometheus: template
	// braces belong to a quoted route value, not a selector insertion point.
	diagnostic := objective
	diagnostic.Response = slo.Response{Owner: "team", RunbookURL: "https://example.com/runbook", FirstCheck: "Inspect failures", Escalation: "Notify on-call"}
	diagnostic.SLI.Total.Labels = map[string]string{"route": "/orders/{id}"}
	diagnostic.SLI.Good.Labels = map[string]string{"route": "/orders/{id}", "result": "success"}
	bundle, err := grafana.Render(grafana.Definition{Service: "checkout", Version: "0.1.0", ReplicaLabel: "instance", Objectives: []slo.Objective{diagnostic}}, grafana.Target{Environment: "staging", URL: "https://example.com", FolderUID: "checkout", PrometheusUID: "prometheus", Scope: scope})
	if err != nil {
		fail(err)
	}
	var dashboard map[string]any
	if err := json.Unmarshal(bundle.Dashboards[0], &dashboard); err != nil {
		fail(err)
	}
	panels := dashboard["panels"].([]any)
	rows := panels[len(panels)-1].(map[string]any)["panels"].([]any)
	errorQuery := rows[1].(map[string]any)["targets"].([]any)[0].(map[string]any)["expr"].(string)
	errorQuery = strings.ReplaceAll(errorQuery, "${replica:regex}", "busy")
	routeSeries := func(result, instance, values string) map[string]any {
		return map[string]any{"series": fmt.Sprintf(`requests_total{service_name="checkout",environment="staging",route="/orders/{id}",result=%q,instance=%q}`, result, instance), "values": values}
	}
	add("replica drill-down preserves route braces and excludes other replicas", []any{
		routeSeries("success", "busy", "0+99x400"), routeSeries("failure", "busy", "0+1x400"), routeSeries("failure", "quiet", "0+100x400"),
	}, "round("+errorQuery+", 0.000001)", expected(.01))
	// Match the bucket format stored by the pinned Prometheus 3 backend. Keep a
	// real query check here: generating le="1" compiles but silently selects nothing.
	latency, err := slo.Latency(slo.Selector{Metric: "duration_seconds"}, 1, []float64{.2, 1, 2})
	if err != nil {
		fail(err)
	}
	_, _, latencyRatio, err := latency.Queries(time.Hour, nil)
	if err != nil {
		fail(err)
	}
	add("one-second latency uses canonical Prometheus 3 bucket", []any{
		map[string]any{"series": "duration_seconds_count", "values": "0+100x400"},
		map[string]any{"series": `duration_seconds_bucket{le="1.0"}`, "values": "0+99x400"},
	}, latencyRatio, expected(.99))
	content := map[string]any{"rule_files": []string{}, "evaluation_interval": "1m", "tests": tests}
	if err := os.MkdirAll(filepath.Dir(*out), 0755); err != nil {
		fail(err)
	}
	file, err := os.Create(*out)
	if err != nil {
		fail(err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(content); err != nil {
		fail(err)
	}
}

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
