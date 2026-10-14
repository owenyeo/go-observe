# Working with go-observe

This SDK instruments Go services and generates Grafana dashboards/alerts from Go
definitions. Read `README.md` and the examples before changing its public API.

## Use the SDK

- Create one `telemetry.Telemetry` in each application process and inject it into
  handlers/workers. There is no global singleton or central SDK server.
- Reuse an existing Prometheus registry or tracer provider when supplied. Do not
  replace global providers, install collectors, or publish Grafana assets at startup.
- Register custom business metrics through `Telemetry.Registerer`. Prefer existing
  metrics over a duplicate family. Counters count events; histograms use seconds.
- Declare a finite HTTP route catalogue. Use route templates, never raw paths.
- Use `httpmetrics.MarkFailed(ctx)` for explicit business failure. Do not inspect
  response bodies or treat HTTP 200 as proof that a workflow succeeded.
- Keep identifiers, phone numbers, tokens, URLs containing query strings and raw
  payloads out of metric labels and default logs/spans. Correlation IDs belong in
  structured logs/traces, not metric labels.
- Keep logs as readable messages with separate fields. Never stringify JSON into
  a message field. Applications choose the log sink and metrics listener/auth.
- Optional trace failures must leave the application serving. Export queues are
  bounded; preserve caller ownership when shutting providers down.

## Define SLOs

- Start from a user journey: what can the user do, and what constitutes failure?
- Document eligible events, successful events, measurement boundary, target and
  window. A handler counter does not include requests lost at an edge/proxy.
- State how client errors, retries, partial failures and cancellations count.
- Good events must be a subset of Total. Both must be monotonic counters. Aggregate
  event counts across replicas; never average replica success percentages.
- For latency use a real cumulative histogram bucket matching the exact threshold.
  A p95 panel is useful diagnosis, but not a good-events/total-events latency SLO.
- Keep missing telemetry, low traffic and actual failure distinct. Do not replace
  a missing denominator with zero or display zero traffic as 100% reliability.
- A rolling-window query only has the data retained by the backend. Do not claim
  full-window compliance for a new instrument, partial coverage or sparse samples.
- Targets in examples are illustrative; service owners approve real objectives.
- Keep infrastructure guardrails separate from customer-facing SLO burn alerts.

## Design dashboards

- Begin with SLO success and budget remaining. Put traffic/errors and replica
  diagnostics in collapsed rows; use linked custom dashboards for more detail.
- Use explicit units, readable titles, short descriptions and consistent colors:
  red for violated targets, orange for depleted budget, green for healthy values.
- Preserve No data/unknown instead of coloring absent telemetry as healthy.
- Bind the environment/cluster in generated queries. Replica selection is for
  diagnostics; do not let UI filters redefine the service-wide SLO or alert.
- Reuse stable dashboard/panel identifiers to preserve links and update in place.
- Native dashboard resources require stable v2 format and an explicit destination
  namespace when moving stacks. Scope their queries just as for classic JSON.
- Custom dashboard JSON has one source of truth in Git. Keep its queries scoped,
  its UIDs stable and its layout compact. Export UI edits for review before release.
- Keep log messages readable; expand structured fields only when useful.

## Implement alerts

- SLO alerts use two windows and error-budget consumption; operational guardrails
  use explicit PromQL/LogQL thresholds with `${scope}`. Alert queries cannot use
  dashboard variables. Return a healthy zero when metrics exist, not an empty
  vector that Grafana interprets as NoData.
- State the trigger, threshold/window, pending time, recovery behavior and the
  treatment of missing data/evaluation errors. NoData/Error must remain visible.
- Use readable alert names and service/environment/severity/owner labels. Keep
  grouping meaningful and avoid duplicate alerts for the same underlying symptom.
- Include symptom, observed value, owner, first checks, runbook, dashboard link
  and escalation guidance. Match severity to the consuming project's policy;
  the SDK does not impose P1–P5 or classify unknown impact as harmless.
- Ship new rules paused until owners review the signal and routing. Test firing,
  recovery and a test destination before enabling production notifications.
- Contact points and on-call schedules are deployment prerequisites. The SDK
  uses an explicit existing receiver and does not replace global routing policy.

## Review and release

- Go definitions live with the service; `observability/VERSION` versions monitoring
  separately from the application. Pin the SDK revision and Go dependencies.
- Read `.go-version` for the tested Go toolchain and `go.mod` for library pins.
  Read `examples/local/compose.yml` for Grafana/Prometheus versions; invoke
  `promtool` through that Compose service instead of repeating an image version.
  Keep stable OpenTelemetry modules aligned. Validate pin changes with all three
  Go, PromQL and local Grafana checks; do not silently adopt `latest` versions.
- Generate/validate without credentials. Keep publication tokens in CI secrets;
  never put them in source, artifacts, command flags or application config.
- Before network writes, inspect `observe diff`. Apply only declared resources
  in their configured folder. Ownership collisions must fail before writes.
- Promote the tested, immutable release artifact to production. A second version
  bump is not required for promotion. Roll back by applying a prior artifact.
- Publication is not transactional across resources: report partial failures and
  rerun or roll back. Do not silently claim atomic or exactly-once deployment.
- Stay within the user's requested local/remote scope. A local implementation
  does not authorize pushing, creating a PR or deploying live resources.

## Checks

```sh
export GOTOOLCHAIN="go$(cat .go-version)"
gofmt -l .
go vet ./...
go test -race ./...
go run ./examples/monitoring -version "$(cat VERSION)" | go run ./cmd/observe validate -version-file VERSION
go run ./examples/promql-check -out artifacts/promql-test.json
docker compose -p go-observe-preview -f examples/local/compose.yml run --rm --no-deps -T \
  -v "$PWD/artifacts:/checks:ro" --entrypoint /bin/promtool prometheus test rules /checks/promql-test.json
```

Run `TestLocalGrafana` only against the isolated loopback compose instance; never
against an organisation's live stack. Document any compatibility limits honestly.

## When this SDK is a dependency

Dependency instructions are not loaded automatically. Consuming projects should
add a reference in their own root `AGENTS.md`, telling agents to resolve the pinned
module directory with `go list -m -f '{{.Dir}}' github.com/owenyeo/go-observe` and read this
file before editing instrumentation, monitoring definitions or dashboard assets.
