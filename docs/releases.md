# Release monitoring separately from the application

The service owns its Go definitions and `observability/VERSION`. A monitoring
release changes dashboards/alerts; application instrumentation still goes through
the service's normal build/deploy. Collector discovery is a separate prerequisite.

1. Pin the SDK and add a Go definition program supporting `-environment`/`-version`.
2. Render every target on PRs without publication credentials. Review the queries,
   ownership, routing, dashboard layout and generated asset diff.
3. Bump `observability/VERSION`. On the main push, call the SDK's reusable
   `.github/workflows/publish.yml` with `mode: release`, the exact service
   commit SHA, and the previous push SHA. It rejects unchanged/decreasing versions,
   renders staging + production, publishes staging and retains both bundles with
   checksums and the pinned source/SDK identity for 30 days.
4. After staging checks, call the same workflow with `mode: promote`, the same
   source SHA and the successful staging run ID. It downloads the production
   bundle from that run instead of rebuilding. No second version bump is needed.
5. For rollback, download the previous successful bundle and explicitly run
   `observe apply` with that environment's token. Changes are reconciled in place.

The reusable workflow requires `source-ref`, `mode` and `GRAFANA_TOKEN`; release
also requires `previous-ref`, promotion requires `release-run-id`. Pin the workflow
itself with `uses: owenyeo/go-observe/.github/workflows/publish.yml@<commit-sha>`.
The renderer and publisher both use the consuming module's `go.mod` and `go.sum`,
with workspace mode off and dependency updates disabled. SDK replacements are rejected.
Promotion checks the recorded SDK module version/checksum and reuses the existing
bundle; it does not rerender it. Monitoring versions remain independent of SDK versions.

`module-directory` defaults to `.`; set it to `web` if the Go module is in `web/`.
`definition-package` is relative to that module, while `version-file` is relative
to the repository root. The initial compiler resolves the service's `go.mod`; both
programs then run with the pinned SDK's tested `.go-version`.
For private SDK access, callers need access to the private reusable workflow and
may supply `MODULE_READ_TOKEN` with repository read permission for dependency fetches.
`private-module-patterns` defaults to `github.com/owenyeo/go-observe` and is appended
to existing `GOPRIVATE` configuration, even without that token. Include every private
namespace needed by the service, for example:

```yaml
with:
  private-module-patterns: github.com/owenyeo/go-observe,github.com/example-org/*
secrets:
  MODULE_READ_TOKEN: ${{ secrets.MODULE_READ_TOKEN }}
```

The token must have read access to all required private GitHub repositories.
Tokens are not stored in bundles or Git URLs. Private module patterns bypass the
public Go proxy/checksum database; they do not grant repository access. Other Git
hosts require their own authentication configuration.

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
