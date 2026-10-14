# Release monitoring separately from the application

The service owns its Go definitions and `observability/VERSION`. A monitoring
release changes dashboards/alerts; application instrumentation still goes through
the service's normal build/deploy. Collector discovery is a separate prerequisite.

1. Pin the SDK and add a Go definition program supporting `-environment`/`-version`.
2. Render every target on PRs without publication credentials. Review the queries,
   ownership, routing, dashboard layout and generated asset diff.
3. Bump `observability/VERSION`. On the main push, call the SDK's reusable
   `.github/workflows/publish.yml` with `mode: release`, the exact source and SDK
   commit SHAs, and the previous push SHA. It rejects unchanged/decreasing versions,
   renders staging + production, publishes staging and retains both bundles with
   checksums and the pinned source/SDK identity for 30 days.
4. After staging checks, call the same workflow with `mode: promote`, the same
   source/SDK SHAs and the successful staging run ID. It downloads the production
   bundle from that run instead of rebuilding. No second version bump is needed.
5. For rollback, download the previous successful bundle and explicitly run
   `observe apply` with that environment's token. Changes are reconciled in place.

The reusable workflow requires `sdk-repository`, `sdk-ref`, `source-ref`, `mode`
and `GRAFANA_TOKEN`; release also requires `previous-ref`, promotion requires
`release-run-id`. `definition-package`, `version-file` and both environment names
are configurable. Host the SDK before referencing this workflow remotely.
Both SDK workflows select Go from the checked-out SDK's `.go-version`. Library
dependencies come from its `go.mod`/`go.sum`; monitoring versions belong to the
service and do not have to match the SDK version.

Create staging/production GitHub environments with their own folder-scoped
Grafana credentials. Set production reviewers using the project's existing
approval policy. Keep tokens available only to reviewed publication jobs. The
workflow does not configure environment permissions or push/merge application code.

Production URL/folder/datasource/scope bindings must be declared in the definition
program so the staging release can render the production bundle without production
credentials. Alert activation is an explicit reviewed definition change; both
paused and active configurations can be rendered before publishing.

The reusable workflow uses POSIX `sh`. It checks artifact source identity and
checksums, rejects promotion from PR runs/unsuccessful runs and serializes publication.
Keep your caller's release trigger version-gated, for example a push to main with
`paths: ['observability/VERSION']`. Other workflow triggers alone must not bypass
the library's numeric version gate.

Changing the SDK version, datasource binding, alert threshold or dashboard query
is a monitoring configuration change. Bump the monitoring version to release it.
Do not let unreviewed UI edits become the permanent source of truth: export them
to the owning definition/custom dashboard file and release through the same path.
