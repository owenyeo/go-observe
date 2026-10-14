package grafana_test

import (
	"fmt"

	"github.com/owenyeo/go-observe/grafana"
	"github.com/owenyeo/go-observe/slo"
)

func ExampleRender() {
	bundle, err := grafana.Render(grafana.Definition{
		Service: "checkout", Version: "0.1.0",
		Guardrails: []grafana.Guardrail{{ID: "scrape", Name: "Metrics scrape failed", Query: `up{${scope}}`, Source: "prometheus", Operator: "lt", Threshold: 1, Severity: "warning",
			Response: slo.Response{Owner: "checkout-team", RunbookURL: "https://example.com/runbooks/scrape", FirstCheck: "Inspect collector reachability.", Escalation: "Notify the service on-call."}}},
	}, grafana.Target{Environment: "staging", URL: "https://grafana.example.com", FolderUID: "checkout", PrometheusUID: "prometheus", Scope: map[string]string{"environment": "staging", "service_name": "checkout"}})
	if err != nil {
		panic(err)
	}
	fmt.Println(len(bundle.Alerts), bundle.Alerts[0].IsPaused)
	// Output: 1 true
}
