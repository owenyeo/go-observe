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
| Go build/test toolchain | [`.go-version`](.go-version), read by SDK CI |
| Go libraries | [`go.mod`](go.mod) and `go.sum`; stable OpenTelemetry modules stay on the same release |
| Grafana and Prometheus test servers | [`examples/local/compose.yml`](examples/local/compose.yml) |
| SDK release | Git tag `vX.Y.Z`; [`VERSION`](VERSION) must agree |
| A consuming service's monitoring release | That service's `observability/VERSION` |

CI and local checks run `promtool` from the Compose Prometheus service, so its
version cannot drift from the test server. Upgrade these pins deliberately and
run the Go, PromQL and local Grafana checks together. GitHub Actions remain pinned
to reviewed commit SHAs. Passing this set does not establish compatibility with
every older/newer backend version.

Examples require `-version` and use `examples/monitoring/VERSION`. Real services pass
their own monitoring version, independently of the SDK's Git tag and `VERSION`.

## Install

```sh
go get github.com/owenyeo/go-observe@v0.1.1
```

While this repository is private, configure Git authentication and
`GOPRIVATE=github.com/owenyeo/*` first. Anonymous installation requires a public repository.

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
go run ./examples/monitoring -version "$(cat examples/monitoring/VERSION)" | go run ./cmd/observe normalize -out artifacts/staging.json
go run ./cmd/observe validate -bundle artifacts/staging.json -version-file examples/monitoring/VERSION
```

For an isolated dashboard preview, see [local preview](docs/local-preview.md).

## Instrument a service

```go
t, err := telemetry.New(ctx, telemetry.Config{
    Service: "checkout", Environment: "staging", Logger: logger,
    Registerer: existingRegistry, Gatherer: existingRegistry, // optional pair
})
if err != nil { return err }

httpStats, err := httpmetrics.New(t, httpmetrics.Config{
    Routes: []string{"GET /orders/{id}"},
    Route: func(r *http.Request) string { return r.Pattern },
})
if err != nil { return err }
handler := httpStats.Middleware(router)
```

Register business counters/histograms through `t.Registerer()`; standard Prometheus
types remain available. Use `httpmetrics.MarkFailed(r.Context())` when an operation
fails despite HTTP 200. Serve `t.MetricsHandler()` on your private/authenticated
listener. Shutdown drains only a tracing provider created by the SDK.

Standard labels are `service_name` and optional `environment`. Scrape configuration
adds replica/cluster labels such as `instance`, `pod` or `cluster`. Custom metric
labels must not duplicate the standard constant labels: when `Environment` is set,
registering a metric with its own `environment` label through `t.Registerer()` returns an error. Both registry
interfaces must be supplied together and describe the same backing registry. To
use globals explicitly, pass `prometheus.DefaultRegisterer` and
`prometheus.DefaultGatherer`; otherwise the SDK creates a private registry. Metrics
already registered outside the SDK are gathered as-is, without added labels.

Optional tracing uses OTLP/HTTP with caller-supplied endpoint/headers or an existing
provider. Set `SampleRatio` explicitly, for example `new(float64(.1))` for 10% of
new root traces. A nil ratio disables optional tracing with a warning; explicit
zero drops new roots. Trusted parent sampling decisions are preserved, including
sampled parents when the root ratio is zero. HTTPS is the
default; trusted local HTTP requires `AllowInsecure`. Public inbound trace context
is ignored by default; set `TrustIncomingTrace` only at a trusted boundary.

Logs use `slog`: readable message, separate fields and optional trace/span IDs.
Request completion logs are disabled by default; set `httpmetrics.Config.AccessLogs`
to true to emit them at Info. Metrics and spans are independent of that switch.
`observe_trace_export_enabled` reports successful configuration, not sampling or delivery.
Supplied exporters transfer to the SDK once metric setup succeeds; failed trace
setup closes them. If `New` returns an error, the caller keeps ownership. A supplied
tracer provider takes precedence, so any unused exporter also stays caller-owned.
The OTLP endpoint must include its full path, usually `/v1/traces`.
Export failures increment `observe_trace_export_failures_total`; trace setup
failure leaves metrics/logs working. Runtime/process collectors are optional:
register standard Prometheus Go/process collectors through `t.Registerer()` if needed.

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
it as a generated SLO overview. Without objectives, setting this field alone only
links alerts to that dashboard; it does not create, update or claim the dashboard.
This explicit reference takes precedence over custom dashboards. With no explicit
UID, alerts link to the first generated/custom dashboard, or omit `dashboard_url`
if there is none. Removing the reference clears the previous alert link on apply.
The [existing-metrics example](examples/existing-metrics/main.go) demonstrates
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
- The latency helper accepts both integer bucket encodings (`1` and `1.0`). Each
  histogram should expose one encoding per bucket; duplicate encodings for the
  same replica would count the bucket twice.
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

## Local development and migration

Before v0.1.0, callers using `Config.Registry` should supply both `Registerer` and
`Gatherer` instead. Change `t.Registerer` to `t.Registerer()`, provide a sample-ratio
pointer, and enable `AccessLogs` only where desired. `observe normalize` replaces
`observe render`: the Go definition program performs rendering; the CLI validates
and formats its JSON output. Reusable workflow callers remove `sdk-repository` and
`sdk-ref`; the service's `go.mod` now selects the SDK for both rendering and publishing.

For local consumption, use `go mod edit -replace=github.com/owenyeo/go-observe=/path/to/go-observe`
and `go get github.com/owenyeo/go-observe`.
