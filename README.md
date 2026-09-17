# Workbenches Operator

Kubernetes operator for the Open Data Hub (ODH) **Workbenches** platform component. It reconciles a cluster-scoped `Workbenches` custom resource and deploys the notebook controller stack (Kubeflow notebook controller, ODH notebook controller, and notebook manifests) on OpenShift.

This operator is designed to run as a standalone module operator within the ODH platform orchestration model. In production, the platform orchestrator creates and updates the `Workbenches` CR and renders the operator Helm chart; fields such as `platform`, `gatewayDomain`, and `mlflowEnabled` are projected from platform configuration.

## Overview

When `spec.managementState` is `Managed`, the operator:

1. Validates the `Workbenches` spec.
2. Ensures the resolved applications namespace exists (creating it if needed) for operands and platform ConfigMap — `APPLICATIONS_NAMESPACE` when set and DNS-1123 valid; otherwise the platform default (`opendatahub` / `redhat-ods-applications`).
3. Renders upstream Kustomize manifests: overlays `RELATED_IMAGE_*` environment variables onto `params.env`/`params-latest.env` image keys (`internal/controller/imageparams.go`), then merges CR-derived parameters (`section-title`, `mlflow-enabled`, `gateway-url`).
4. Applies resources to the cluster using Server-Side Apply (SSA).
5. Populates `status.releases` from upstream `component_metadata.yaml` (when present).
6. Updates `status.distribution` to reflect the reconciled distribution context.
7. Derives `status.phase` using the platform **ModuleStatus** lifecycle model.
8. Reports readiness when all labelled operand deployments are available and distribution is aligned.

When `spec.managementState` is `Removed`, the operator cleans up managed resources and sets `status.phase` to `Failed` (the ModuleStatus contract has no separate Removed phase).

A **finalizer** (`components.platform.opendatahub.io/workbenches-cleanup`) is added to every `Workbenches` CR. On `Removed` or CR deletion, the operator deletes operand resources identified by component labels before clearing the finalizer.

The controller sets **OwnerReferences** on operand resources (except Namespace, CRD, ImageStream) and uses `Owns()` watches for drift reconciliation. It also watches labelled operand **Deployments** for availability changes (ready/available replica counts), so status updates promptly when deployments become ready or regress without waiting for a spec change. Component labels (`app.opendatahub.io/workbenches=true`, `app.kubernetes.io/part-of=workbenches`) are applied consistently to all objects' metadata, Deployment selectors/pod templates, and Service selectors.

Upstream manifests are **committed** under `opt/manifests/` for hermetic container builds (Konflux/airgapped environments cannot fetch from GitHub at build time). The image copies that tree into `/opt/manifests`. At runtime the operator copies manifests to a temporary directory before rendering so the baked-in tree stays immutable.

## Architecture

```text
┌─────────────────────┐     watches      ┌──────────────────────────┐
│ Workbenches CR      │ ───────────────► │ workbenches-operator     │
│ (default-workbenches│                  │                          │
└─────────────────────┘                  │  reconcile loop          │
                                         │       │                  │
┌─────────────────────┐  availability    │       ▼                  │
│ Operand Deployments │ ───────────────► │  Kustomize render        │
│ (component label)   │                  │  (params.env injection)  │
└─────────────────────┘                  │       │                  │
                                         │       ▼                  │
                                         │  Server-Side Apply       │
                                         └──────────┬───────────────┘
                                                    │
┌─────────────────────┐                             │
│ Notebook CRs        │  mutating webhooks          │
│ (kubeflow.org/v1)   │ ◄───────────────────────────┘
└─────────────────────┘   (connection + hardware profile)
                                                    │
                    ┌───────────────────────────────┼───────────────────────────────┐
                    ▼                               ▼                               ▼
     kf-notebook-controller          odh-notebook-controller              notebooks
     (OpenShift overlay)             (base)                          (ODH or RHOAI overlay)
```

### Manifest sources (ODH and RHOAI)

Manifests under `opt/manifests/` are fetched and committed to this repo. Sources are defined in `opt/manifest-sources.sh` as ODH (upstream) and RHOAI (downstream) maps, selected by `ODH_PLATFORM_TYPE` (`OpenDataHub` or `rhoai`) — same pattern as opendatahub-operator / rhods-operator. Git refs in that file are per operator branch and are kept by `sync-branches.yaml`.

| Component | ODH (upstream) | RHOAI (downstream) | Path |
|-----------|----------------|-------------------|------|
| `workbenches/kf-notebook-controller` | [opendatahub-io/kubeflow](https://github.com/opendatahub-io/kubeflow) | [red-hat-data-services/kubeflow](https://github.com/red-hat-data-services/kubeflow) | `components/notebook-controller/config` |
| `workbenches/odh-notebook-controller` | [opendatahub-io/kubeflow](https://github.com/opendatahub-io/kubeflow) | [red-hat-data-services/kubeflow](https://github.com/red-hat-data-services/kubeflow) | `components/odh-notebook-controller/config` |
| `workbenches/notebooks` | [opendatahub-io/notebooks](https://github.com/opendatahub-io/notebooks) | [red-hat-data-services/notebooks](https://github.com/red-hat-data-services/notebooks) | `manifests` |

**Local refresh:**

```bash
make manifests-fetch                              # ODH / upstream (default)
make manifests-fetch ODH_PLATFORM_TYPE=rhoai      # RHOAI / downstream
```

Do not edit files under `opt/manifests/` manually. After fetching, inspect the tree, run `make test`, then commit both `opt/manifest-sources.sh` (if sources changed) and `opt/manifests/`.

A scheduled GitHub Action ([`.github/workflows/manifests-sync-main.yaml`](.github/workflows/manifests-sync-main.yaml)) runs daily against `main`, refreshes **ODH** manifests, validates rendering with `TestRenderRealManifests`, and opens/updates a PR when content changes. Pushes to `stable` and `v1.x` (and a daily schedule for `stable`) run [`.github/workflows/manifests-sync-stable.yaml`](.github/workflows/manifests-sync-stable.yaml), which commits fetched manifests directly (`stable` also bumps ODH `branch@sha` pins in `opt/manifest-sources.sh` first; `v1.x` keeps tag pins). Operand refs live in `opt/manifest-sources.sh` and are kept on the target branch by [`.github/workflows/sync-branches.yaml`](.github/workflows/sync-branches.yaml). See [`opt/README.md`](opt/README.md) and [`DEPENDENCIES.md`](DEPENDENCIES.md).

The main sync workflow needs permission to open PRs: enable **Settings → Actions → General → Allow GitHub Actions to create and approve pull requests**, or configure a fine-grained personal access token (scoped to this repository with `contents: write` and `pull_requests: write`) as a repository secret. Direct commits on `stable`/`v1.x` also require those branches to allow GitHub Actions to push (or the PAT to bypass branch protection).

Override individual sources:

```bash
./get_all_manifests.sh --workbenches/kf-notebook-controller=org:repo:branch@sha:source_path
ODH_PLATFORM_TYPE=rhoai ./get_all_manifests.sh
```

### Platform support

| Platform | `spec.platform` value | Operand namespace | Notebooks overlay |
|----------|----------------------|-----------------|-------------------|
| Open Data Hub | `OpenDataHub` | Resolved applications namespace (typically `opendatahub`) | `workbenches/notebooks/odh/base` |
| Self-managed RHOAI | `SelfManagedRhoai` | Resolved applications namespace (typically `redhat-ods-applications`) | `workbenches/notebooks/rhoai/base` |

`spec.workbenchNamespace` is a legacy field (historically `rhods-notebooks` for JupyterHub-era notebooks) and is **not** used when deploying module operands.

The resolved applications namespace is `APPLICATIONS_NAMESPACE` when set and DNS-1123 valid; otherwise the operator falls back by platform (`opendatahub` for OpenDataHub, `redhat-ods-applications` for SelfManagedRhoai).

The operator targets **OpenShift** only. The Kubeflow notebook controller always uses the OpenShift overlay.

### Webhooks

When `--enable-webhooks=true` (default), the operator serves two mutating admission webhooks for `Notebook` resources (`kubeflow.org/v1`):

| Webhook | Path | Name | Purpose |
|---------|------|------|---------|
| Connection | `/workbenches-connection-notebook` | `connection-notebook.opendatahub.io` | Injects connection secrets from the `opendatahub.io/connections` annotation |
| Hardware profile | `/workbenches-hardware-profile` | `hardwareprofile-notebook-injector.opendatahub.io` | Applies `HardwareProfile` settings (resources, nodeSelector, tolerations) from `opendatahub.io/hardware-profile-name` / `opendatahub.io/hardware-profile-namespace` annotations; injects Kueue LocalQueue label |

The hardware profile webhook reads `HardwareProfile` CRs (`infrastructure.opendatahub.io/v1`) as unstructured objects to avoid a dependency on the platform operator API. It also reads `spec.scheduling.kueue.localQueueName` and sets the `kueue.x-k8s.io/queue-name` label on the Notebook — when Kueue is active, nodeSelector/tolerations are skipped (Kueue handles placement). The webhook emits Kubernetes Warning Events (`ContainerNameMismatch`, `ValidationError`) on validation failures.

#### Webhook TLS

At startup, the operator **auto-detects** the available TLS provider by probing cluster API groups (`internal/webhook/tls/`):

| Provider | Detection | Behaviour |
|----------|-----------|-----------|
| **OpenShift** (highest priority) | `route.openshift.io` or `security.openshift.io` API group present | Annotates Service with `service.beta.openshift.io/serving-cert-secret-name` and MutatingWebhookConfiguration with `inject-cabundle` |
| **cert-manager** | `cert-manager.io` API group present | Creates `ClusterIssuer` + `Certificate`, annotates MutatingWebhookConfiguration with `cert-manager.io/inject-ca-from` |
| **None** | Neither detected | Webhooks are disabled; MutatingWebhookConfiguration is deleted |

A periodic retry (30s) handles resources (Service, MutatingWebhookConfiguration) that appear after startup.

Additionally, on OpenShift the operator integrates with the cluster-wide **TLS security profile** (`internal/tlsconfig/`): it reads the APIServer TLS profile at startup and applies its cipher/TLS-version settings to the metrics and webhook servers. A watcher triggers graceful shutdown on profile changes. On non-OpenShift clusters, Mozilla Intermediate defaults are used.

The Helm chart value `webhooks.tlsProvider` (`openshift`, `certmanager`, or `""`) controls which Kustomize overlay is used for static TLS resources at chart templating time.

## Custom Resource

**API group:** `components.platform.opendatahub.io/v1alpha1`
**Kind:** `Workbenches`
**Scope:** Cluster (singleton — `metadata.name` must be `default-workbenches`)

### Spec

| Field | Description |
|-------|-------------|
| `managementState` | `Managed` (default) or `Removed` |
| `workbenchNamespace` | Legacy JupyterHub-era notebooks namespace. Immutable after creation. **Not used** for module operand deploy. |
| `gatewayDomain` | Data science gateway domain. Typically projected by the orchestrator from `GatewayConfig`. Injected as `gateway-url` into operand manifests. |
| `platform` | `OpenDataHub` or `SelfManagedRhoai`. Typically projected by the orchestrator. Controls notebooks overlay and UI `section-title`. |
| `mlflowEnabled` | Whether MLflow integration is active. Typically projected by the orchestrator. Injected as `mlflow-enabled` into operand manifests. |

Notebook-controller operands and the platform ConfigMap (`odh-workbenches-config`) always target the **resolved applications namespace**: `APPLICATIONS_NAMESPACE` (Helm `operatorNamespace` / optional `applicationsNamespace`) when set and DNS-1123 valid; otherwise the platform default (`opendatahub` for OpenDataHub, `redhat-ods-applications` for SelfManagedRhoai).

### Status

The controller publishes conditions including `Ready`, `ProvisioningSucceeded`, `DeploymentsAvailable`, `Degraded`, and `ReleaseMetadataAvailable`. Before every status update, conditions with empty `Reason` fields are sanitized to `"Unknown"` to prevent API validation errors from foreign conditions:

| Field | Description |
|-------|-------------|
| `phase` | ModuleStatus lifecycle phase (see below) |
| `distribution` | Distribution context: `name` (`OpenDataHub`, `SelfManagedRHOAI`, or `Standalone`) + `version` |
| `applicationsNamespace` | Resolved operand namespace (`APPLICATIONS_NAMESPACE`, or platform default: `opendatahub` / `redhat-ods-applications`) |
| `workbenchNamespace` | Echo of legacy `spec.workbenchNamespace` (not the operand deploy target) |
| `releases` | Component versions from `component_metadata.yaml` and platform version handshake |
| `observedGeneration` | Last reconciled `metadata.generation` |

#### ModuleStatus phases

`status.phase` follows the platform ModuleStatus specification:

| Phase | Meaning |
|-------|---------|
| `Pending` | First observe before provisioning has started |
| `Initializing` | Manifests applied; waiting for operand deployments to become available |
| `Ready` | All component deployments available and `Ready=True` |
| `Upgrading` | Spec changed while previously ready; rollout in progress |
| `Degraded` | Was ready but deployments regressed (e.g. scale-to-zero, crash loop) |
| `Failed` | Unrecoverable reconcile error, or component removed |

Phase priority (highest first): `Failed` → `Ready` → `Upgrading` → `Degraded` → `Pending` → `Initializing`.

`Ready=True` requires all deployments labelled `app.opendatahub.io/workbenches=true` in the resolved applications namespace (`APPLICATIONS_NAMESPACE`, or platform default) to have the desired number of ready replicas, and the distribution to be aligned (when a platform ConfigMap is present). Scale-to-zero is treated as unavailable. Release metadata is informational; a missing or malformed `component_metadata.yaml` does not block provisioning.

When no `odh-workbenches-config` ConfigMap exists, the operator reports `Standalone` as the distribution name.

### Example

```yaml
apiVersion: components.platform.opendatahub.io/v1alpha1
kind: Workbenches
metadata:
  name: default-workbenches
spec:
  managementState: Managed
  workbenchNamespace: opendatahub
  platform: OpenDataHub
```

See [`config/samples/components_v1alpha1_workbenches.yaml`](config/samples/components_v1alpha1_workbenches.yaml) for a sample manifest.

## Prerequisites

- **Go** 1.26+ (see [`DEPENDENCIES.md`](DEPENDENCIES.md) for upgrade guidance)
- **kubectl** configured against a target cluster
- **Helm** 3.x (for Helm-based deployment)
- **yq** (for `make chart-sync-rbac`)
- **Podman** or **Docker** (for container image builds)
- **Git** (for fetching upstream manifests)
- An **OpenShift** cluster for end-to-end deployment

Optional local overrides can be placed in `local.mk` (gitignored); the Makefile includes this file automatically.

## Development

### Fetch upstream manifests

Manifests are committed in `opt/manifests/` (required for image builds and local runs). Refresh when sources or upstream content change:

```bash
make manifests-fetch
```

### Generate code and manifests

```bash
make manifests   # CRDs, RBAC, webhooks from kubebuilder markers
make generate    # DeepCopy methods
```

### Lint and test

```bash
make lint        # golangci-lint (see .golangci.yml)
make test        # fmt, vet, envtest, unit tests
make unit-test   # tests only (used in CI)
make test-e2e    # end-to-end tests (requires a running cluster)
make test-coverage  # HTML coverage report
```

Tests use [envtest](https://book.kubebuilder.io/reference/envtest.html) with Kubernetes **1.32.0** assets. CI runs `TestRenderRealManifests` against the committed `opt/manifests/` tree. E2e tests (`tests/e2e/`) use Ginkgo and run against a real cluster (Kind in CI via `e2e.yml`).

### Build the manager binary

```bash
make build
```

Output: `bin/manager`

### Run locally against a cluster

```bash
make manifests generate fmt vet
go run ./cmd/main.go \
  --manifests-base-path="$(pwd)/opt/manifests" \
  --leader-elect=false
```

Ensure CRDs are installed and a `Workbenches` resource exists (see [Deployment](#deployment)).

### Operator flags

| Flag | Default | Description |
|------|---------|-------------|
| `--manifests-base-path` | `/opt/manifests` | Root directory for baked-in component manifests |
| `--enable-webhooks` | `true` | Enable notebook mutating webhooks (connection + hardware profile, port 9443) |
| `--leader-elect` | `false` | Enable leader election for controller manager |
| `--health-probe-bind-address` | `:8081` | Health/readiness probe address |
| `--metrics-bind-address` | `0` (disabled) | Metrics endpoint; use `:8443` for HTTPS |
| `--metrics-secure` | `true` | Serve metrics over HTTPS |
| `--enable-http2` | `false` | Enable HTTP/2 for the metrics and webhook servers |

## Build and publish container image

The `Dockerfile` builds the manager on UBI9 Go toolset (`go-toolset:1.26`) with FIPS-friendly `-tags strictfipsruntime`, then copies committed `opt/manifests` into the runtime image (no network fetch at build time).

```bash
# Default image: quay.io/opendatahub/odh-workbenches-operator:odh-stable
# Requires opt/manifests/ (run make manifests-fetch if missing)
make image-build

# Override image name and container engine
make image-build IMG=quay.io/my-org/odh-workbenches-operator:v0.1.0 CONTAINER_ENGINE=docker

make image-push
make image-build-push   # build and push
```

> **Note:** For production deployments, use an immutable image reference (digest) rather than a mutable tag. The default `:odh-stable` tag is intended for development iteration.

## Deployment

### Kustomize (development)

```bash
make install    # apply CRDs
make deploy IMG=quay.io/opendatahub/odh-workbenches-operator:odh-stable  # or @sha256:<digest> for prod
```

Kustomize overlays deploy into `workbenches-operator-system` with the `workbenches-operator-` name prefix. `config/base/` contains shared resources; `config/default/` adds OpenShift webhook TLS patches. For vanilla Kubernetes webhook certs, use `config/certmanager/` instead.

To remove:

```bash
make undeploy
make uninstall
```

### Helm (standalone or local testing)

The operator Helm chart lives in [`charts/operator/`](charts/operator/). Generated artifacts (CRD copy, RBAC rules) are synced from `config/` — run `make chart-sync` after changing kubebuilder markers.

```bash
make helm-lint       # lint chart (runs chart-sync)
make helm-template   # render templates locally
make helm-deploy     # install/upgrade release
make helm-undeploy   # uninstall release and CRD
```

Common overrides:

```bash
make helm-deploy \
  IMG=quay.io/opendatahub/odh-workbenches-operator:odh-stable \
  HELM_NAMESPACE=workbenches-operator-system \
  APPLICATIONS_NAMESPACE=opendatahub
```

For RHOAI-style installs, set `APPLICATIONS_NAMESPACE=redhat-ods-applications`.

Helm values of note:

| Value | Purpose |
|-------|---------|
| `createOperatorNamespace` | `false` for platform integration; `true` for standalone installs |
| `operatorNamespace` | Operator Deployment namespace. **Platform sets this alone** to ApplicationsNamespace (simplest model). |
| `applicationsNamespace` | Optional. When empty, defaults to `operatorNamespace`. Standalone may set it to keep a dedicated operator NS while operands use another NS. |
| `rbac.enableRuntimeEscalation` | Grants bind/escalate on upstream operand ClusterRoles |
| `controllerImage.relatedImageEnv` | `RELATED_IMAGE_ODH_WORKBENCHES_OPERATOR_IMAGE` for platform image injection |
| `webhooks.tlsProvider` | `openshift`, `certmanager`, or `""` |
| `devLogging` | Enable debug-level console logging (default `false`) |

In product builds, the platform orchestrator renders this chart and injects a single namespace via `ModuleConfig.NamespaceValueKey` (`operatorNamespace` = ApplicationsNamespace), plus the controller image. That one value drives both the operator Deployment and `APPLICATIONS_NAMESPACE` / operand ConfigMap placement.

Verify chart drift in CI or locally:

```bash
make chart-verify-sync      # RBAC/CRD sync with config/
make chart-verify-inventory # kustomize vs Helm resource parity
make chart-verify-params    # params.env matches values.yaml image
```

Run `make helm-undeploy` before switching from a Kustomize-based install to Helm (or vice versa).

### Create a Workbenches instance

After the operator is running:

```bash
kubectl apply -f config/samples/components_v1alpha1_workbenches.yaml
kubectl get workbenches default-workbenches
```

## CI/CD

### GitHub Actions

Most workflows run on pushes and PRs to `main`, `stable`, and `v1.x`. Manifest sync on `main` is scheduled; on `stable`/`v1.x` it runs on push (and daily on `stable`). Other scheduled and path-filtered workflows are noted below.

| Workflow | Purpose |
|----------|---------|
| [`test.yml`](.github/workflows/test.yml) | Unit tests and manifest rendering validation |
| [`build.yml`](.github/workflows/build.yml) | `make build` |
| [`lint.yml`](.github/workflows/lint.yml) | pre-commit, golangci-lint, go vet, go mod verify, kube-linter, Helm lint, chart sync checks, verify-manifests, verify-generate |
| [`e2e.yml`](.github/workflows/e2e.yml) | End-to-end tests on Kind cluster |
| [`govulncheck.yaml`](.github/workflows/govulncheck.yaml) | Go vulnerability scan on push to `main` (also `workflow_dispatch`) |
| [`disconnected-readiness.yaml`](.github/workflows/disconnected-readiness.yaml) | Airgapped/disconnected readiness check on PRs |
| [`operator-chaos-validation.yaml`](.github/workflows/operator-chaos-validation.yaml) | operator-chaos shift-left validation (knowledge, CRD diff, upgrade dry-run) on PRs touching `chaos/`, `api/`, `internal/controller/`, or `config/crd/` |
| [`go-directive-updater.yaml`](.github/workflows/go-directive-updater.yaml) | Weekly Go patch version bumps |
| [`manifests-sync-main.yaml`](.github/workflows/manifests-sync-main.yaml) | Daily upstream manifest sync PRs against `main` |
| [`manifests-sync-stable.yaml`](.github/workflows/manifests-sync-stable.yaml) | On push to `stable`/`v1.x` (daily on `stable`): commit manifests directly (SHA pin bump on `stable` only) |
| [`sync-branches.yaml`](.github/workflows/sync-branches.yaml) | Manual/workflow_call branch sync (`main→stable`, `stable→v1.x`); keeps target `opt/manifests`, `opt/manifest-sources.sh`, and `.tekton` |
| [`tls-lint.yml`](.github/workflows/tls-lint.yml) | TLS configuration lint with SARIF upload |
| [`semgrep-tls.yml`](.github/workflows/semgrep-tls.yml) | Semgrep TLS compliance rules on PRs |

Coverage is uploaded to Codecov ([`codecov.yml`](codecov.yml)).

[Dependabot](.github/dependabot.yml) is configured for weekly GitHub Actions version bumps and Go module security-only updates.

Local hygiene hooks live in [`.pre-commit-config.yaml`](.pre-commit-config.yaml) (`golangci-lint` is skipped in CI because `lint.yml` already runs it).

Security scanning: [gitleaks](.gitleaks.toml) for secret detection, [Semgrep](semgrep.yaml) for TLS compliance rules, and [`govulncheck`](.github/workflows/govulncheck.yaml) for Go dependency vulnerabilities.

### Konflux / Tekton

Pipelines in [`.tekton/`](.tekton/) build and publish the operator image via Konflux. Builds are hermetic: they consume committed `opt/manifests/` rather than cloning upstream at build time.

Branch sync keeps the target `.tekton/` directory, so do not copy branch-specific tags from this README. The live trigger and `output-image` are in those PipelineRuns. Typical mapping:

| Pipeline | Trigger | Output image |
|----------|---------|--------------|
| `odh-workbenches-operator-on-pull-request` | PR to that branch | `quay.io/opendatahub/odh-workbenches-operator:odh-pr` (expires after 7d) |
| `odh-workbenches-operator-on-push` | Push to that branch | `quay.io/opendatahub/odh-workbenches-operator:<tag>` (for example `:main` on `main`, `:odh-stable` on `stable`) |

## Project layout

```text
.
├── api/v1alpha1/              # Workbenches CRD Go types
├── charts/operator/           # Helm chart (synced from config/)
├── chaos/                     # operator-chaos knowledge (ODH/RHOAI profiles) + experiments
├── ci/                        # Go version bump helper scripts
├── cmd/main.go                # Operator entrypoint
├── config/
│   ├── base/                  # Base Kustomize resources (manager, RBAC, webhook)
│   ├── certmanager/           # cert-manager overlay (non-OpenShift)
│   ├── crd/                   # Generated CRD manifests
│   ├── default/               # OpenShift overlay (webhook TLS patches)
│   ├── operator/              # Deployment name alignment overlay
│   ├── rbac/                  # RBAC (incl. escalate role for upstream ClusterRoles)
│   ├── samples/               # Sample Workbenches CR
│   └── webhook/               # MutatingWebhookConfiguration and service
├── internal/
│   ├── controller/            # Reconciler, manifest rendering (manifests.go, imageparams.go, imagestreams.go)
│   ├── gvk/                   # GroupVersionKind helpers
│   ├── metadata/              # Label and annotation constants
│   ├── platform/              # Platform type helpers
│   ├── platformconfig/        # Platform ConfigMap + version handshake
│   ├── releases/              # component_metadata.yaml loader
│   ├── status/                # ModuleStatus phase computation
│   ├── tlsconfig/             # Cluster TLS security profile integration (OpenShift APIServer)
│   └── webhook/               # Notebook mutating webhooks (connection, hardware profile + Kueue)
│       └── tls/               # Runtime TLS provider auto-detection + cert provisioning
├── opt/
│   ├── README.md              # Manifest contributor guidance
│   ├── manifest-sources.sh    # Per-branch operand org/repo/ref map
│   └── manifests/             # Committed upstream manifests (hermetic builds)
├── hack/                      # Chart sync/verify scripts
├── .github/dependabot.yml     # Dependabot config (GHA + Go security)
├── .gitleaks.toml             # Secret scanning configuration (gitleaks)
├── .pre-commit-config.yaml    # pre-commit hooks (CI + local)
├── semgrep.yaml               # Semgrep TLS compliance rules
├── get_all_manifests.sh       # Upstream manifest fetch script
├── tests/e2e/                 # End-to-end Ginkgo tests (Kind in CI)
├── DEPENDENCIES.md            # Go, dependency, and tool upgrade guide
├── Dockerfile                 # Hermetic container image build
└── Makefile                   # Build, test, Helm, and deploy targets
```

## Makefile quick reference

Run `make help` for the full list. Common targets:

| Target | Description |
|--------|-------------|
| `manifests-fetch` | Download ODH or RHOAI manifests to `opt/manifests/` (`ODH_PLATFORM_TYPE`) |
| `manifests` / `generate` | Regenerate CRD/RBAC/webhooks and DeepCopy code |
| `lint` / `lint-fix` | Run golangci-lint |
| `test` / `unit-test` | Run Go tests |
| `test-e2e` | End-to-end tests (requires running cluster) |
| `test-coverage` | HTML coverage report |
| `build` | Compile `bin/manager` |
| `run` | Run controller locally |
| `image-build` / `image-push` | Build or push container image (`opt/manifests` required) |
| `chart-sync` / `chart-sync-rbac` / `chart-sync-crd` | Sync generated config into Helm chart |
| `helm-lint` / `helm-deploy` / `helm-undeploy` | Helm chart validation and deployment |
| `chart-verify-sync` / `chart-verify-inventory` / `chart-verify-params` | Verify Helm chart matches `config/` |
| `install` / `deploy` | Kustomize-based CRD install and operator deploy |

## Tool versions

| Tool | Makefile variable | Version |
|------|-------------------|---------|
| Go | (`go.mod`) | 1.26.7 |
| Kustomize | `KUSTOMIZE_VERSION` | v5.6.0 |
| controller-gen | `CONTROLLER_TOOLS_VERSION` | v0.18.0 |
| setup-envtest | `ENVTEST_VERSION` | release-0.23 |
| golangci-lint | `GOLANGCI_LINT_VERSION` | v2.12.2 |

For Go, dependency, and upstream manifest upgrades, see [`DEPENDENCIES.md`](DEPENDENCIES.md).

## Contributing

Review [`OWNERS`](OWNERS) for approvers and reviewers. Open pull requests against `main`.

- When upstream notebook controller manifests add or rename ClusterRoles, update [`config/rbac/rbac_escalate_role.yaml`](config/rbac/rbac_escalate_role.yaml) and run `make chart-sync-rbac`.
- After changing kubebuilder markers, run `make manifests` and `make chart-sync`.
- When refreshing upstream manifests, commit `opt/manifests/` together with any `opt/manifest-sources.sh` source changes.
- When operand topology, webhooks, or notebook ImageStreams change, update the matching chaos profile under [`chaos/profiles/odh`](chaos/profiles/odh) or [`chaos/profiles/rhoai`](chaos/profiles/rhoai).
- See [`DEPENDENCIES.md`](DEPENDENCIES.md) for Go version, dependency, and upstream manifest upgrade procedures.
- Agent-oriented project conventions live in [`AGENTS.md`](AGENTS.md).

## License

Licensed under the [Apache License 2.0](LICENSE).
