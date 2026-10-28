# Monitoring releases — SDK 0.1.2

The service owns its Go definitions and `observability/VERSION`. Definitions fix
queries, thresholds, runbooks, IDs and environment-specific destinations. Shared
SDK deployment configuration selects pause state, human owner/team and an existing
Grafana receiver. Neither requires an incident worker. Application instrumentation
and collector discovery still follow the service's normal deployment process.

## 0.1.2 release notes and migration

- Deployment configuration is a flat list of `{namespace, service, environment,
  enableAlerts, owner, contactPoint}` entries. Duplicate tuples fail validation.
- `Response.Owner` is removed. Configuration alone supplies the `team` label and
  `owner` annotation. `Render` produces paused definitions without deployment
  ownership or routing; `diff`/`apply` require resolved ownership, including when
  paused. Move existing response owners into deployment configuration and run
  `observe normalize -config ...` before publication.
- Rename `Target.Namespace` to `Target.GrafanaNamespace` and the corresponding JSON
  field to `target.grafanaNamespace`. `Bundle.Namespace` remains the logical service
  group. Re-render old artifacts with the new SDK; there is no legacy field alias.
- Generated identities remain unchanged for empty logical namespaces. Introducing
  a namespace intentionally creates distinct identities and metric scope.
- Alert labels, annotations and notification settings are now compared exactly.
  Extra labels/annotations added by Grafana or through its UI now show as **update**
  in `observe diff` and are removed on apply unless included in the reviewed bundle.
  Removed owners and receivers also produce updates. Grafana bookkeeping and
  query-model defaults remain tolerated. Review existing extras before publication.
- Bundle validation rejects invalid pending periods, missing/duplicate query
  references and nonexistent condition references before network access.
- HTTP metrics record `101 Switching Protocols` correctly while retaining interim
  `100`/`103` handling.

Adopting 0.1.2 requires updating the consumer's SDK/workflow pins and the API uses
above, then creating a monitoring release to retain the new definition artifacts.
Later deployment-config edits reuse those definitions without rebuilding them.

## Two publication modes

| Mode | Definition input | Behavior |
| --- | --- | --- |
| `release` | `previous-ref`, no `release-run-id` | Require a monitoring version increase, render both environments, review and publish staging. |
| `release` | `release-run-id` | Reuse the original release's staging definitions with the selected configuration revision. |
| `promote` | `release-run-id` | Reuse the original release's production definitions with the selected configuration revision. |

Both modes require `source-ref`, `configuration-ref` and `configuration-file`.
Supplying `release-run-id` selects reuse; omit `previous-ref` in that case.
Configuration-only changes simply start another release or promotion with a new
config ref. There is no separate `configure` mode or target-environment override.

All modes prepare an actual Grafana before/after diff, then wait at the destination
GitHub environment gate. The apply job consumes the same checksummed resolved
artifact and publisher binary. It performs preflight again, applies changes and
verifies an unchanged readback before retaining a successful publication record.
Preparation does not use a protected GitHub environment: configure the environment
reviewers so approval happens after the diff is available.

Pin `.github/workflows/publish.yml` to a reviewed SDK commit. The first adoption
requires updating both that workflow pin and the consumer's SDK dependency to
`v0.1.2` or a later compatible version.

`source-ref` is the immutable service commit that produced the definitions. The
renderer and publisher use that consumer module's dependency graph with workspace
mode off and dependency updates disabled. Local SDK replacements are rejected.
The workflow records and verifies the pinned SDK module version and checksum.
Re-publication builds the publisher from that same pin; it never compiles
or runs the service's definition program.

`configuration-ref` is a separate immutable commit in the caller's repository.
`configuration-file` is a path in that revision, checked against directory escape.
Both are required for release and promotion.
A newer config commit does not change `source-ref` or the definition digest. A
shared configuration can contain many namespaces/services; the exact artifact
identity selects one entry. There is no fallback to another namespace or service.

`namespace` must match `Definition.Namespace` (empty for legacy artifacts).
`release` selects `staging-environment`; `promote` selects `production-environment`.
These names default to `staging` and `production` and can be changed by the caller.
The artifact must already be bound to that environment; config cannot retarget queries.

```yaml
jobs:
  republish-monitoring:
    uses: owenyeo/go-observe/.github/workflows/publish.yml@<reviewed-sdk-commit>
    with:
      mode: release # use promote for production
      source-ref: <original-service-release-commit>
      release-run-id: '<successful-release-run-id>'
      configuration-ref: <reviewed-configuration-commit>
      configuration-file: observability/publishing.json
      namespace: commerce
    secrets:
      GRAFANA_READ_TOKEN: ${{ secrets.MONITORING_READ_TOKEN }}
      GRAFANA_TOKEN: ${{ secrets.MONITORING_WRITE_TOKEN }}
      MODULE_READ_TOKEN: ${{ secrets.MODULE_READ_TOKEN }}
```

The placeholders above are illustrative. Select the workflow commit and exact
release/configuration commits through the consuming project's review policy.
The configuration schema and executable local example are in the
[README](../README.md#namespaces-and-deployment-configuration).

## Review, credentials and integrity

Provide a folder-scoped read token as `GRAFANA_READ_TOKEN` to preparation. It must
be available before the environment gate, for example as a repository/organisation
secret explicitly passed by the caller. For legacy callers it falls back to the
passed `GRAFANA_TOKEN`; use the separate read credential when adopting this flow.
The apply job uses `GRAFANA_TOKEN`, including an environment secret of that name
when configured. Configure separate staging/production destinations, credentials
and reviewers. The workflow does not create GitHub environment protections.

The prepared artifact `monitoring-prepared-<run-id>-<attempt>` contains:

- `reviewed/resolved.json` and the full `diff.json` before/after comparison.
- The selected configuration snapshot, and `publication.json` with
  source/configuration revisions, definition/configuration/resolved digests, SDK
  identity, monitoring version, destination identity and resource UID sets.
- The publisher and checksums; the checksum manifest's digest is passed directly
  from preparation to apply, so replacing the artifact manifest also fails.
- Original environment-specific definition bundles and their checksum manifest.

The job summary lists changes and publication identity. Download the prepared
artifact to review complete queries, ownership and routing before approval.
Apply never checks out a newer config, re-renders definitions or re-resolves
settings. All publication modes share a write lock per repository/environment.
Callers in different repositories targeting the same resources must additionally
coordinate publication; the lock is not a global Grafana transaction.

A new `monitoring-published-<run-id>-<attempt>-<environment>` artifact appears only
after apply/readback succeeds. It retains the exact resolved snapshot and receipt.
An initial release also retains `monitoring-<source-ref>` with both definition bundles.
Reuse always references that original definition-release run. Re-publication and
promotion require a successful non-PR release run whose source SHA matches, and
verify its source, SDK identity and checksums. Artifacts are retained
for 30 days: archive them securely for longer rollback/configuration windows.
An expired artifact fails rather than rebuilding unreleased source.

Publication is not transactional. A failure may leave some resources updated;
it cannot produce a successful receipt for that attempt. Inspect the failure and
explicitly rerun or review/apply a previously successful resolved artifact to roll
back. No automatic rollback, resource deletion or recovery scheduler is installed.
Concurrent Grafana UI edits can still occur after review: apply checks current
ownership and prerequisites, but does not promise a lock on alert content.

## Consumer build settings

`module-directory` defaults to `.`; set it to `web` if the module is in `web/`.
`definition-package` is relative to that module; `version-file` is relative to the
repository root. The renderer accepts `-environment` and `-version`. Declare the
namespace in its Go definitions and scope. The initial compiler resolves the
service's `go.mod`; both programs use the SDK's tested `.go-version` afterward.

For private dependencies, supply `MODULE_READ_TOKEN` with repository read access.
`private-module-patterns` defaults to `github.com/owenyeo/go-observe` and appends to
existing `GOPRIVATE`, even without that token. Include all private module patterns
needed by the consumer. Tokens stay out of artifacts and Git URLs; private module
patterns control proxy/checksum lookup and do not themselves grant access. Other
Git hosts require their own authentication setup.

Definition changes, SDK upgrades, datasource bindings, thresholds and queries
still require a monitoring version bump. Trigger release from the project's
approved release branch, gated by its monitoring VERSION change. Activation,
owner and receiver changes reuse that release via `release-run-id`. Review
firing/recovery against a test destination before live activation: receiver preflight verifies existence,
integration type and recovery settings, not successful end-to-end delivery.

This version references existing receivers. Creating contact points, IRM
integrations, schedules and escalation policies remains future SDK work. Grafana
owns evaluation, notification delivery and incident handling throughout.
