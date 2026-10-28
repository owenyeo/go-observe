// Adopt an existing HTTP metric family without changing application code.
// This example uses common web metric names, not a specific deployment's settings.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/owenyeo/go-observe/grafana"
	"github.com/owenyeo/go-observe/slo"
)

func main() {
	environment := flag.String("environment", "staging", "target environment")
	version := flag.String("version", "", "monitoring release version (required)")
	flag.Parse()
	definition := grafana.Definition{
		Service: "web", Version: *version, ReplicaLabel: "pod",
		OverviewUID: "existing-" + *environment + "-web",
		Objectives: []slo.Objective{{
			ID: "completed-request-success", Name: "Completed request success",
			Description: "Eligible completed application requests exclude client errors and health checks. HTTP-200 application errors count as failures. Edge availability is outside this measurement.",
			Target:      .999, Window: 30 * 24 * time.Hour,
			SLI: slo.Ratio{
				Total: slo.Selector{Metric: "web_http_requests_total", Labels: map[string]string{"traffic": "request"}, Regex: map[string]string{"status": "2..|3..|5.."}},
				Good:  slo.Selector{Metric: "web_http_requests_total", Labels: map[string]string{"traffic": "request", "outcome": "success"}, Regex: map[string]string{"status": "2..|3.."}},
			},
			Response: slo.Response{RunbookURL: "https://example.com/runbooks/web", FirstCheck: "Compare 5xx and application_error events with dependency failures.", Escalation: "Contact the service on-call and assess affected customers."},
		}},
	}
	// Replace bindings with the consuming service's reviewed environment settings.
	// Preserve the real dashboard UID only after an explicit ownership adoption.
	target := grafana.Target{Environment: *environment, URL: "https://grafana.example.com", FolderUID: "existing-web", PrometheusUID: "existing-prom",
		Scope: map[string]string{"service_name": "web", "environment": *environment}}
	bundle, err := grafana.Render(definition, target)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(bundle); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
