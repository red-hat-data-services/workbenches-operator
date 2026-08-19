# Agent Guidelines for workbenches-operator

This file provides context and conventions for AI coding agents working on this repository.
`CLAUDE.md` is a symlink to this file.

## Project Summary

This is a Kubernetes operator (built with Kubebuilder/controller-runtime) that manages workbench (notebook) infrastructure for Open Data Hub and Red Hat OpenShift AI. It reconciles a cluster-scoped singleton `Workbenches` CR and applies upstream Kustomize manifests via server-side apply.

## Key Architectural Decisions

- **Singleton CR**: Only one `Workbenches` resource is allowed, and it must be named `default-workbenches`. Enforced via CEL on the CRD (`WorkbenchesInstanceName` in `api/v1alpha1`).
- **Manifest rendering**: The operator reads Kustomize bundles from a filesystem path (`--manifests-base-path`, default `/opt/manifests`) and renders them at runtime with the krusty engine — it does not embed manifests in Go code.
- **Committed manifests**: Operand manifests under `opt/manifests/` are fetched by `get_all_manifests.sh`, committed to the repo, and copied into the image at build time. Do not hand-edit them. The script has ODH and RHOAI source maps; `ODH_PLATFORM_TYPE` selects which (`OpenDataHub` default or `rhoai` for downstream).
- **Server-side apply**: Manifest application uses SSA with field manager `workbenches-operator`.
- **Platform awareness**: Platforms `OpenDataHub` and `SelfManagedRhoai` select different notebook overlays and default namespaces.
- **Immutable fields**: `workbenchNamespace` is immutable after initial creation (CEL-enforced). It names the legacy JupyterHub-era notebooks namespace (ensured on reconcile); operand deploy uses the resolved applications namespace.
- **Platform version handshake**: Controller watches ConfigMap `odh-workbenches-config` (in the resolved applications namespace) and records the platform version in `status.releases` (see `internal/platformconfig/`).
- **Operand namespace**: Notebook-controller manifests are applied into the resolved applications namespace (`APPLICATIONS_NAMESPACE` when set and DNS-1123 valid; otherwise `opendatahub` for OpenDataHub / `redhat-ods-applications` for SelfManagedRhoai). The legacy `workbenchNamespace` (or platform default `rhods-notebooks` / `opendatahub`) is ensured separately for Notebook CR placement.
- **Distribution alignment**: `status.distribution` reports the distribution context (`OpenDataHub`, `SelfManagedRHOAI`, or `Standalone`). Ready is gated on distribution alignment when a platform ConfigMap is present.
- **OwnerReferences and watches**: Operand resources receive `OwnerReferences` via `SetControllerReference` in `applyObjects()` (skip list: Namespace, CRD, ImageStream). The controller uses `Owns()` watches on ConfigMap, Secret, Service, ServiceAccount, Role, RoleBinding, ClusterRole, ClusterRoleBinding, MutatingWebhookConfiguration, ValidatingWebhookConfiguration, and Deployment (with `deploymentAvailabilityChangedPredicate`).
- **Component labels**: SSA applies `app.opendatahub.io/workbenches=true` and `app.kubernetes.io/part-of=workbenches` on every object's metadata, and additionally patches `spec.selector.matchLabels`/`spec.template.metadata.labels` on Deployments and `spec.selector` on Services (`setComponentLabels`/`patchNestedLabels` in `manifests.go`).
- **Status condition sanitization**: Before every status update, `sanitizeConditions()` replaces empty `Reason` fields with `"Unknown"` to avoid API validation errors from foreign conditions.
- **Managed vs Removed**: `managementState: Removed` cleans up operator-managed resources; deletion uses finalizer `components.platform.opendatahub.io/workbenches-cleanup`.

## Repository Layout

```text
cmd/main.go                     Entrypoint — manager, controller, optional webhooks
api/v1alpha1/                   Workbenches CRD Go types
internal/controller/            Reconciler + krusty/SSA rendering (manifests.go, imageparams.go, imagestreams.go)
internal/webhook/               Mutating webhooks (notebook connections, hardware profile + Kueue)
internal/webhook/tls/           Runtime TLS provider auto-detection + cert provisioning
internal/tlsconfig/             Cluster TLS security profile integration (OpenShift APIServer)
internal/metadata/              Label/annotation constants
internal/platform/              Platform constants and defaults (namespace, section title)
internal/platformconfig/        odh-workbenches-config ConfigMap + version handshake
internal/releases/              component_metadata.yaml → status.releases
internal/status/                ModuleStatus phase helpers
internal/gvk/                   Notebook, HardwareProfile, ImageStream, Namespace GVKs
config/                         Kustomize (base, default/OpenShift, certmanager, crd, rbac, manager, operator, webhook, samples)
charts/operator/                Helm chart (CRD/RBAC synced from generated config/)
opt/manifests/                  Upstream operand manifests (get_all_manifests.sh; do not hand-edit)
ci/                             Go directive bump helper script
hack/                           Boilerplate + Helm chart sync/verify scripts
.github/workflows/              CI (test, build, lint, e2e, manifest-sync, sync-branches, TLS lint, Semgrep)
.github/dependabot.yml          Dependabot: weekly GHA bumps + Go security updates
semgrep.yaml                    Semgrep TLS compliance rules
.gitleaks.toml                  Secret scanning configuration (gitleaks)
.tekton/                        Konflux build PipelineRuns
DEPENDENCIES.md                 Go / dependency / manifest upgrade guide
get_all_manifests.sh            Manifest fetch script
```

## Build and Test Commands

```bash
make build              # Build the manager binary
make run                # Run controller locally
make test               # Unit + integration tests (fmt/vet + envtest, K8s 1.32.0)
make unit-test          # Unit/integration tests without fmt/vet
make test-e2e           # End-to-end tests (requires a running cluster)
make test-coverage      # HTML coverage report
make lint               # golangci-lint
make manifests          # Regenerate CRD, RBAC, webhook YAML from Go markers
make generate           # Regenerate DeepCopy methods
make manifests-fetch    # Fetch upstream manifests into opt/manifests/
make image-build        # Build container image (podman by default)
make install / deploy   # Install CRDs / deploy via kustomize
make chart-sync         # Sync generated CRD + ClusterRole into Helm chart
make helm-deploy        # Deploy via Helm
```

There are no `test-upgrade`, `test-handler`, or `bundle` Makefile targets.

## Code Conventions

### Go Style
- Go 1.26.x (`go.mod`; Dockerfile uses `ubi9/go-toolset:1.26`). See [DEPENDENCIES.md](DEPENDENCIES.md) for version bumps.
- Kubebuilder RBAC/CRD/webhook markers in controller and type files — keep them up to date when changing permissions or fields.
- Run `make manifests` after modifying kubebuilder markers.
- Run `make generate` after modifying CRD types to regenerate `zz_generated.deepcopy.go`.
- After RBAC/CRD marker changes that affect the chart, run `make chart-sync` (CI verifies sync/inventory).
- Error wrapping uses `fmt.Errorf("context: %w", err)`.
- Logging uses `controller-runtime`'s `log.FromContext(ctx)` — use `.V(1)` for debug-level.

### Testing
- Unit/integration tests use Ginkgo/Gomega with `envtest`.
- Test files live alongside source in the same package (`internal/**/*_test.go`).
- E2e tests live in `tests/e2e/` and run against a real cluster (Kind in CI).
- `TestRenderRealManifests` in `manifests_test.go` needs `opt/manifests` populated — run `make manifests-fetch` if missing.

### Labels and Annotations
- Defined in `internal/metadata/` — always use constants, never hardcode label/annotation strings.
- Operator-managed resources: `app.opendatahub.io/workbenches=true`, `app.kubernetes.io/part-of=workbenches`.
- Namespace ownership: `opendatahub.io/generated-namespace=true`.
- Notebook webhooks read: `opendatahub.io/connections`, `opendatahub.io/hardware-profile-name` (+ optional namespace annotation).

### Manifests
- Sources and sync process are documented in [DEPENDENCIES.md](DEPENDENCIES.md) and `opt/README.md`.
- Do not edit files under `opt/manifests/` directly — they are overwritten by `get_all_manifests.sh` / the daily `manifest-sync` workflow.
- At render time the controller copies the tree, overlays `RELATED_IMAGE_*` onto existing keys in `params.env` / `params-latest.env` (`imageParamMap` in `internal/controller/imageparams.go`), then merges CR-derived params (`section-title`, `mlflow-enabled`, `gateway-url`), and applies platform-specific overlays.
- Keep `imageParamMap` in sync when upstream manifests add/rename image keys, and with opendatahub-operator's workbenches module `relatedImages` list (see [DEPENDENCIES.md](DEPENDENCIES.md) "Upgrading Upstream Manifests").

## CRD

**GVK:** `components.platform.opendatahub.io/v1alpha1/Workbenches` (cluster-scoped)

**Name:** must be `default-workbenches`.

Key spec fields:
- `managementState`: `Managed` (default) or `Removed`
- `workbenchNamespace`: legacy JupyterHub-era notebooks namespace (immutable); ensured on reconcile; **not** the operand deploy target
- `platform`: `OpenDataHub` or `SelfManagedRhoai`
- `gatewayDomain`, `mlflowEnabled`: projected by the orchestrator
- `workbenchesV2`: optional submodule config (`*WorkbenchesV2Spec`); contains `managementState` (default `Removed`). When nil or Removed, workbenches-v2 manifests are not deployed. When `Managed`, deploys from `workbenches/workspaces-controller/overlays/gateway`.

Status:
- Conditions: `Ready`, `ProvisioningSucceeded`, `DeploymentsAvailable`, `Degraded`, `ReleaseMetadataAvailable`, `WorkbenchesV2Ready`
- `phase`: `Pending` | `Initializing` | `Ready` | `Upgrading` | `Degraded` | `Failed`
- `distribution`: `name` (`OpenDataHub` | `SelfManagedRHOAI` | `Standalone`) + `version`
- `releases[]`, `observedGeneration`
- `applicationsNamespace` (resolved operand NS: `APPLICATIONS_NAMESPACE`, or platform default `opendatahub` / `redhat-ods-applications`)
- `workbenchNamespace` (echo of legacy `spec.workbenchNamespace`)

## Webhooks

Registered in `internal/webhook/webhook.go` via `RegisterAllWebhooks`:

1. **Connection injection** (`internal/webhook/notebook/`) — path `/workbenches-connection-notebook`
   - Reads `opendatahub.io/connections`, validates secrets, injects `envFrom`
2. **Hardware profile** (`internal/webhook/hardwareprofile/`) — path `/workbenches-hardware-profile`
   - Reads `opendatahub.io/hardware-profile-name`, applies resources/tolerations/nodeSelector
   - **Kueue integration**: reads `spec.scheduling.kueue.localQueueName` from HardwareProfile and sets `kueue.x-k8s.io/queue-name` label on the Notebook. When Kueue is active, nodeSelector/tolerations are skipped (Kueue handles placement). Label is cleared on profile removal or change.
   - Emits Kubernetes Warning Events (`ContainerNameMismatch`, `ValidationError`) on validation failures via `EventWriter`.

`--enable-webhooks` defaults to `true` in `cmd/main.go`. The kustomize manager Deployment does not override it. Helm can toggle via `webhooks.enabled`.

### TLS

- **Webhook serving certs** (`internal/webhook/tls/`): at startup, `Detect()` auto-discovers the TLS provider by probing API groups — preference order: **OpenShift** (service-CA) → **cert-manager** → **None** (webhooks disabled). The `Ensure()` routine configures the appropriate Service annotations, CA-bundle injection on the MutatingWebhookConfiguration, and for cert-manager creates a `ClusterIssuer` + `Certificate`. A periodic retry (30s) handles resources that appear after startup.
- **Cluster TLS profile** (`internal/tlsconfig/`): on OpenShift, reads the APIServer TLS security profile and applies its cipher/TLS-version settings to the metrics server and webhook server. A watcher triggers graceful shutdown on profile changes. On non-OpenShift, Mozilla Intermediate defaults are used.

## CI

GitHub Actions in `.github/workflows/`:
- `test.yml` — unit tests + Codecov; separate job for `TestRenderRealManifests`
- `build.yml` — binary build
- `lint.yml` — golangci-lint, go vet, kube-linter, helm-lint, chart sync/inventory verify, **verify-manifests** and **verify-generate** (ensure generated code is committed)
- `e2e.yml` — end-to-end tests on Kind cluster (PRs touching code/Dockerfile)
- `manifest-sync.yaml` — daily refresh of `opt/manifests/` (opens PR)
- `go-directive-updater.yaml` — weekly `go` directive patch bump in `go.mod`
- `sync-branches.yaml` — manual/workflow_call sync between branches (`main→stable`, `stable→v1.x`); excludes `opt/manifests`
- `tls-lint.yml` — TLS configuration lint (`tls-config-lint`) with SARIF upload
- `semgrep-tls.yml` — Semgrep TLS compliance rules on PRs

Dependabot (`.github/dependabot.yml`): weekly GitHub Actions version bumps + Go module security-only updates.

Konflux builds: `.tekton/` PipelineRuns for push and pull request.

## Contributing

- When upstream notebook controller manifests add or rename ClusterRoles, update `config/rbac/rbac_escalate_role.yaml` and run `make chart-sync-rbac`.
- After changing kubebuilder markers, run `make manifests` and `make chart-sync`.
- When refreshing upstream manifests, commit `opt/manifests/` together with any `get_all_manifests.sh` source changes.
- See [DEPENDENCIES.md](DEPENDENCIES.md) for Go version, dependency, and upstream manifest upgrade procedures.
- Review `OWNERS` for approvers and reviewers. Open pull requests against `main`.

## Common Pitfalls

- Forgetting `make manifests` after changing kubebuilder markers leads to stale CRD/RBAC/webhook YAML.
- Forgetting `make generate` after changing CRD types leads to missing DeepCopy methods.
- Forgetting `make chart-sync` after CRD/RBAC changes leaves the Helm chart out of sync (CI will fail verify targets).
- Tests that use real manifests (`TestRenderRealManifests`) fail without `opt/manifests` present.
- Creating a `Workbenches` CR named anything other than `default-workbenches` is rejected by CEL.
- The `config/manager/kustomization.yaml` image reference may contain local overrides — check before committing.
- Do not invent e2e/upgrade/contrib paths or Makefile targets that are not in this tree; see [DEPENDENCIES.md](DEPENDENCIES.md) for upgrade workflows.
