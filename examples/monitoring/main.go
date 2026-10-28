// Definitions belong to the service repository and are reviewed alongside its code.
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
	namespace := flag.String("namespace", "", "logical service namespace; empty preserves legacy IDs")
	version := flag.String("version", "", "monitoring release version (required)")
	flag.Parse()
	response := slo.Response{RunbookURL: "https://example.com/runbooks/checkout", FirstCheck: "Compare failed requests with dependency errors and recent releases.", Escalation: "Notify the service on-call; involve the incident lead for widespread customer impact."}
	latency, err := slo.Latency(slo.Selector{Metric: "http_server_request_duration_seconds", Labels: map[string]string{"route": "GET /work"}}, .2, []float64{.005, .01, .025, .05, .1, .2, .3, .5, 1, 2, 5, 10})
	if err != nil {
		fail(err)
	}
	definition := grafana.Definition{Namespace: *namespace, Service: "checkout", Version: *version, ReplicaLabel: "instance",
		Objectives: []slo.Objective{
			{ID: "request-success", Name: "Request success", Description: "Completed work requests excluding client errors; explicit application failures count as unsuccessful. Process/edge failures require a separate availability probe.", Target: .999, Window: 30 * 24 * time.Hour, Response: response,
				SLI: slo.Ratio{Total: slo.Selector{Metric: "http_server_requests_total", Labels: map[string]string{"route": "GET /work"}, Regex: map[string]string{"status": "2..|3..|5.."}}, Good: slo.Selector{Metric: "http_server_requests_total", Labels: map[string]string{"route": "GET /work", "outcome": "success"}, Regex: map[string]string{"status": "2..|3.."}}}},
			{ID: "request-latency", Name: "Requests within 200 ms", Description: "Fraction of completed work requests finishing within 200 ms; this measures duration, independently of outcome.", Target: .995, Window: 30 * 24 * time.Hour, SLI: latency, Response: response},
		}, Guardrails: []grafana.Guardrail{
			{ID: "metrics-scrape-failed", Name: "Metrics scrape failed", Description: "At least one monitored replica could not be scraped. Inspect collector connectivity and the private metrics endpoint.", Query: "sum(up{${scope}} == bool 0)", Source: "prometheus", Operator: "gt", Threshold: 0, Pending: 2 * time.Minute, Severity: "warning", Response: response},
			{ID: "metrics-missing", Name: "Service metrics missing", Description: "The expected request counter is absent. No request observations also occurs before the first request; establish traffic or synthetic coverage before interpreting it as an outage.", Query: "absent(http_server_requests_total{${scope}}) or (0 * sum(http_server_requests_total{${scope}}))", Source: "prometheus", Operator: "gt", Threshold: 0, Pending: 5 * time.Minute, Severity: "warning", Response: response},
		}}
	url := os.Getenv("GRAFANA_URL")
	if url == "" {
		url = "http://127.0.0.1:8793"
	}
	scope := map[string]string{"service_name": "checkout", "environment": *environment}
	if *namespace != "" {
		scope["service_namespace"] = *namespace
	}
	bundle, err := grafana.Render(definition, grafana.Target{
		Environment: *environment, URL: url, FolderUID: "checkout", PrometheusUID: "prometheus",
		Scope: scope,
	})
	if err != nil {
		fail(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(bundle); err != nil {
		fail(err)
	}
}

func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
