# go-observe

Go instrumentation and SLO-driven Grafana dashboards/alerts, with a CLI for CI.
Each application process owns one telemetry instance. Definitions and publishing
run separately; the SDK adds no central server.

This is a standalone module hosted at `github.com/owenyeo/go-observe`. No company account, collector, cluster or cloud provider
is required. The Go language minimum and dependencies are declared in `go.mod`;
the tested build toolchain is pinned in `.go-version`. CI also tests the Go minimum
from `go.mod`. The SDK is licensed under [MIT](LICENSE).

## Version policy

| What is versioned | Source of truth |
|---|---|
| Go build/test toolchain | [`.go-version`](.go-version), read by both CI workflows |
| Go libraries | [`go.mod`](go.mod) and `go.sum`; stable OpenTelemetry modules stay on the same release |
| Grafana and Prometheus test servers | [`examples/local/compose.yml`](examples/local/compose.yml) |
| SDK release | [`VERSION`](VERSION) |
| A consuming service's monitoring release | That service's `observability/VERSION` |

CI and local checks run `promtool` from the Compose Prometheus service, so its
version cannot drift from the test server. Upgrade these pins deliberately and
run the Go, PromQL and local Grafana checks together. GitHub Actions remain pinned
to reviewed commit SHAs. Passing this set does not establish compatibility with
every older/newer backend version.

Examples require `-version`; local examples use the SDK's `VERSION` for convenience.
Real services pass their own monitoring version, independently of the SDK release.

## Try it locally

```sh
export GOTOOLCHAIN="go$(cat .go-version)"
go test -race ./...
go run ./examples/service
```

The example serves requests on `127.0.0.1:8080` and metrics on `127.0.0.1:9090`.
From a second terminal:

```sh
export GOTOOLCHAIN="go$(cat .go-version)"
curl http://127.0.0.1:8080/work
curl 'http://127.0.0.1:8080/work?fail=1'
curl 'http://127.0.0.1:8080/work?application=1'
curl http://127.0.0.1:9090/metrics
go run ./examples/monitoring -version "$(cat VERSION)" | go run ./cmd/observe render -out artifacts/staging.json
go run ./cmd/observe validate -bundle artifacts/staging.json -version-file VERSION
```

For an isolated dashboard preview, see [local preview](docs/local-preview.md).

## Instrument a service

```go
t, err := telemetry.New(ctx, telemetry.Config{
    Service: "checkout", Environment: "staging", Logger: logger,
    Registry: existingRegistry, // optional; otherwise a private registry is created
})
if err != nil { return err }

httpStats, err := httpmetrics.New(t, httpmetrics.Config{
    Routes: []string{"GET /orders/{id}"},
    Route: func(r *http.Request) string { return r.Pattern },
})
if err != nil { return err }
handler := httpStats.Middleware(router)
```

Register business counters/histograms through `t.Registerer`; standard Prometheus
types remain available. Use `httpmetrics.MarkFailed(r.Context())` when an operation
fails despite HTTP 200. Serve `t.MetricsHandler()` on your private/authenticated
listener. Shutdown drains only a tracing provider created by the SDK.

Standard labels are `service_name` and optional `environment`. Scrape configuration
adds replica/cluster labels such as `instance`, `pod` or `cluster`. Custom metric
labels must not duplicate the standard constant labels.

Optional tracing uses OTLP/HTTP with caller-supplied endpoint/headers or an existing
provider. Set `SampleRatio` explicitly (.1 = 10%, 1 = all, 0 = none). HTTPS is the
default; trusted local HTTP requires `AllowInsecure`. Public inbound trace context
is ignored by default; set `TrustIncomingTrace` only at a trusted boundary.

Logs use `slog`: readable message, separate fields and optional trace/span IDs.
Export failures increment `observe_trace_export_failures_total`; trace setup
failure leaves metrics/logs working. Runtime/process collectors are optional:
register standard Prometheus Go/process collectors through `t.Registerer` if needed.

## Define monitoring in Go

Keep a small Go program in the service's `observability/` directory. It should
accept `-environment` and `-version`, call `grafana.Render`, and emit the bundle
as JSON. [The example](examples/monitoring/main.go) includes success/latency SLOs
and telemetry guardrails; adapt its eligibility and targets to the actual journey.

```go
objective := slo.Objective{
    ID: "request-success", Name: "Request success",
    Target: .999, Window: 30 * 24 * time.Hour,
    SLI: slo.Ratio{
        Total: slo.Selector{Metric: "operations_total"},
        Good:  slo.Selector{Metric: "operations_total",
            Labels: map[string]string{"result": "success"}},
    },
    Response: slo.Response{Owner: "checkout-team",
        RunbookURL: "https://example.com/runbooks/checkout",
        FirstCheck: "Inspect operation failures and dependencies.",
        Escalation: "Notify the service on-call."},
}
```

Total and Good select eligible/successful monotonic counters. Good must be a subset
of Total. `slo.Latency` requires the exact configured histogram bucket. HTTP latency
alone measures speed, not correctness; combine both into explicitly instrumented
good-event counters if your SLO requires both.

Each target supplies URL, existing folder/datasource UIDs, environment and exact
scope matchers. Scope conflicts are rejected for generated SLIs. Existing metric
names work directly: adoption does not require renaming them or using the middleware.
Custom guardrails use `${scope}` inside selectors. Custom dashboard JSON can be
included in `Definition.CustomDashboards`; its author must scope its queries. Both
classic dashboard JSON and `dashboard.grafana.app/v2` resources are supported. For
v2, identity comes from `metadata.name`, content from `spec`, and the destination
namespace from `Target.Namespace` (or the resource if unset). Render binds the
folder and strips exported server metadata. Set the namespace explicitly when
promoting to a different Grafana stack.
`Definition.OverviewUID` preserves an existing dashboard identifier when adopting
it. The [existing-metrics example](examples/existing-metrics/main.go) demonstrates
this without changing instrumentation or embedding a company's deployment details.

The generated dashboard shows success and budget remaining with traffic/error
diagnostics collapsed. Replica filters affect diagnostics, not the service SLO.
Default burn policies consume 2% of the budget in 1h/5m and 5% in 6h/30m. Thresholds
scale with the SLO window; service-specific policies can replace the defaults.

## Review and publish

```sh
go run ./cmd/observe diff -bundle artifacts/staging.json
go run ./cmd/observe apply -bundle artifacts/staging.json
```

Set `GRAFANA_TOKEN` through your secret store/CI. Neither command reads credentials
from the bundle. `diff` is read-only; `apply` checks every resource's ownership,
folder and datasources before writing, then reads changes back. Generated UIDs are
stable by service/environment/definition ID. Another owner's resource is a hard
failure; adoption of existing dashboards requires deliberate ownership tagging.

Library callers can use `grafana.NewClientWithConfig` with an optional
`*http.Client` for custom certificates, transport or timeout settings. The SDK
copies the client and rejects redirects to protect credentials. `errors.Is`
recognizes cancellation/deadlines; `errors.As` exposes `*grafana.HTTPError` for
status codes and `net.Error` for timeouts, without exposing response bodies or
arbitrary transport messages.

Create instances before serving. Telemetry methods and HTTP middleware support
concurrent use; configuration and monitoring definitions must remain immutable.
Route/exclusion callbacks must be safe for concurrent requests. Serialize release
operations for the same Grafana resources. Stop producers before Shutdown; only
the provider/exporter owned by the SDK is drained.

Alerts are paused by default. To activate, explicitly set `Target.EnableAlerts`
and an existing `ContactPoint` with an integration and recovery notifications.
Test firing/recovery/routing before enabling production. Configure schedules,
grouping and notification templates in Grafana; global policies are not overwritten.

Rules carry summary, description (including observed values), owner, first checks,
runbook, escalation and a dashboard URL. Slack templates can iterate `.Alerts`
and display those annotations. Severity names come from the service definitions;
the SDK does not impose an organisation's incident priority scheme.

See [release workflow](docs/releases.md) for version gating, promotion and rollback.

## Compatibility and limits

- Local compatibility is exercised against the exact versions in the Compose file.
  Classic dashboards use `/api/dashboards/db`; native v2 resources use
  `/apis/dashboard.grafana.app/v2/namespaces/<namespace>/dashboards`. Alerts use
  `/api/v1/provisioning/alert-rules`. Required APIs are checked before writes.
  Classic/alert compatibility APIs are deprecated by Grafana. Native support is
  limited to the stable v2 resource format; alpha/beta exports must be migrated.
  Dashboard comparison is symmetric and excludes classic server bookkeeping.
  Server content normalization that changes authored fields fails readback rather
  than silently accepting drift.
- The latency helper uses Prometheus 3's normalized histogram bucket labels.
  Backends storing legacy labels require explicit `slo.Ratio` selectors.
- Direct rolling-window queries need matching retention and sufficient samples.
  Large fleets may need recording rules; this SDK does not install ruler rules.
- No traffic yields unknown SLO attainment and does not fire a burn alert. Missing
  series stays NoData; separate coverage/probe alerts are still needed.
- HTTP middleware measures requests reaching a handler. Edge availability,
  business completion and background-worker outcomes need explicit instruments.
- Low-volume services may require synthetic checks or another alert policy.
- Publication is not transactional across resources and never deletes obsolete
  rules automatically. Rerun a partial release or apply the previous artifact;
  remove retired resources deliberately.
- This first version provides Go helpers. Other languages may emit compatible metrics, but no Python
  SDK or YAML authoring is included.

## Agents

[AGENTS.md](AGENTS.md) defines SDK usage, dashboard, SLO and alert standards.
Installing a dependency does not load its guidance into agents automatically.
Add this to the consuming repository's root `AGENTS.md`:

> Before changing instrumentation or monitoring definitions, resolve the pinned
> go-observe module directory (`go list -m -f '{{.Dir}}' github.com/owenyeo/go-observe`)
> and read its `AGENTS.md`. Follow its dashboard, SLO and alert standards.

For local consumption, use `go mod edit -replace=github.com/owenyeo/go-observe=/path/to/go-observe`
and `go get github.com/owenyeo/go-observe`.
