# Upgrading Go Version and Dependencies in Workbenches Operator

This guide outlines the steps to upgrade the Go version and dependencies in the Workbenches Operator project.

## Upgrading Go Version

Upgrading the Go version should be done in a separate PR to isolate the changes and make review easier.

> [!NOTE]
> **Patch versions are bumped automatically.** The `go-directive-updater` workflow
> (`.github/workflows/go-directive-updater.yaml`) runs weekly and opens a PR to
> bump the `go` directive in `go.mod` to the latest patch release, provided a
> matching `ubi9/go-toolset` image tag exists. The steps below are for **minor or
> major** version upgrades that require manual Dockerfile and CI changes.

> [!IMPORTANT]
> Images are built in the [ubi9/go-toolset](https://catalog.redhat.com/software/containers/ubi9/go-toolset/61e5c00b4ec9945c18787690) container.
> It contains a customized FIPS-compatible version of Go, that however lags behind the latest upstream Go version.
> Always use a Go version that has a supporting go-toolset image available.

1. Begin by reading [Go release notes](https://go.dev/doc/devel/release) to identify potential incompatibilities.

2. Update the Go version in the following files:
   - `go.mod`: Update the `go` directive at the top of the file.
   - `Dockerfile`: Update the Go version in the builder stage base image (`ubi9/go-toolset:<version>`).

3. Run the following commands to update and verify the build:

    ```shell
    go mod tidy
    make test
    ```

4. Review CI/CD configuration files that specify a Go version and update them accordingly:
   - `.github/workflows/` — GitHub Actions workflows (build, test, lint).
   - `.tekton/` — Konflux/Tekton pipeline definitions.

5. Commit these changes and create a pull request for the Go version upgrade.

   > [!WARNING]
   > Use the `Manifest List Digest` and not the `Image Digest` when locating sha256 in the Red Hat Image Catalog entry.

## Upgrading Dependencies

Upgrading dependencies can be done separately from the Go version upgrade. However, some dependency upgrades may require a newer Go version.

1. To update all dependencies to their latest minor or patch versions:

    ```shell
    go get -u ./...
    ```

    To update to major versions, you'll need to update import paths manually and run `go get` for each updated package.

2. Run `go mod tidy` to clean up the `go.mod` and `go.sum` files, pay attention to not increasing the required Go version, e.g.:

    ```shell
    go mod tidy -go=1.25.0
    go: example.com/pkg@v2.0.0 requires go@1.26.0, but 1.25.0 is requested
    ```

   (The above suggests to either bump the required Go version or to use an older version of the dependency.)

3. Verify that the project still builds and tests pass:

    ```shell
    make test
    ```

4. Review the changes in `go.mod` and `go.sum`. Pay special attention to major version upgrades, as they may include breaking changes.

5. If any dependencies require a newer Go version, you may need to upgrade Go first following the steps in the "Upgrading Go Version" section.

6. Commit the changes to `go.mod` and `go.sum`, and create a pull request for the dependency upgrades.

## Upgrading Build Tools

Build tool versions are pinned in the `Makefile`. Update the corresponding version variables when upgrading:

| Tool | Makefile Variable | Current Version |
|------|-------------------|-----------------|
| kustomize | `KUSTOMIZE_VERSION` | v5.6.0 |
| controller-gen | `CONTROLLER_TOOLS_VERSION` | v0.18.0 |
| setup-envtest | `ENVTEST_VERSION` | release-0.23 |
| golangci-lint | `GOLANGCI_LINT_VERSION` | v2.12.2 |

After updating a version, remove the old binary from `bin/` to trigger a fresh download:

```shell
rm -f bin/<tool>*
make <tool>
```

## Upgrading Upstream Manifests

Upstream component manifests are stored in `opt/manifests/` and committed to the repository. They are refreshed by the scheduled `.github/workflows/manifest-sync.yaml` workflow, which runs `get_all_manifests.sh` daily and opens a PR when content changes. This keeps Konflux container builds hermetic and supports airgapped deployments that cannot reach GitHub at build or runtime.

The manifest-sync workflow needs permission to open PRs. Enable **Settings → Actions → General → Allow GitHub Actions to create and approve pull requests**, or configure a `MANIFEST_SYNC_PAT` repository secret (PAT with `repo` scope).

Manifest sources are defined in `get_all_manifests.sh` as two maps (same pattern as
opendatahub-operator / rhods-operator):

- `ODH_COMPONENT_MANIFESTS` — upstream `opendatahub-io` sources (default)
- `RHOAI_COMPONENT_MANIFESTS` — downstream `red-hat-data-services` sources

`ODH_PLATFORM_TYPE` selects which map is used (`OpenDataHub` by default; `rhoai`
selects RHOAI). Unsupported values exit with an error. Upstream CI and the daily
manifest-sync workflow use the ODH map. Downstream
`red-hat-data-services/workbenches-operator` fetches with `ODH_PLATFORM_TYPE=rhoai`
so `opt/manifests/` matches the workbench entries in
[rhods-operator](https://github.com/red-hat-data-services/rhods-operator)
prefetched manifests for that release branch.

### ODH (upstream) sources

| Target | Source Repository | Branch | Source Path |
|--------|-------------------|--------|-------------|
| `workbenches/kf-notebook-controller` | `opendatahub-io/kubeflow` | `main` | `components/notebook-controller/config` |
| `workbenches/odh-notebook-controller` | `opendatahub-io/kubeflow` | `main` | `components/odh-notebook-controller/config` |
| `workbenches/notebooks` | `opendatahub-io/notebooks` | `main` | `manifests` |
| `workbenches/workspaces-controller` | `opendatahub-io/workbenches` | `main` | `workspaces/controller/manifests/kustomize` |

### RHOAI (downstream) sources

| Target | Source Repository | Branch | Source Path |
|--------|-------------------|--------|-------------|
| `workbenches/kf-notebook-controller` | `red-hat-data-services/kubeflow` | `main` | `components/notebook-controller/config` |
| `workbenches/odh-notebook-controller` | `red-hat-data-services/kubeflow` | `main` | `components/odh-notebook-controller/config` |
| `workbenches/notebooks` | `red-hat-data-services/notebooks` | `main` | `manifests` |
| `workbenches/workspaces-controller` | `red-hat-data-services/workbenches` | `main` | `workspaces/controller/manifests/kustomize` |

To pin manifests to a specific commit, update the ref field to include a SHA:

```shell
# Format: org:repo:branch@sha:source_path
["workbenches/kf-notebook-controller"]="opendatahub-io:kubeflow:main@abc123def:components/notebook-controller/config"
```

After modifying manifest sources:

1. Run `make manifests-fetch` (ODH) or `make manifests-fetch ODH_PLATFORM_TYPE=rhoai` (RHOAI).
2. Inspect the resulting files in `opt/manifests/` for expected changes.
3. Run `make test` to ensure the controller still renders manifests correctly.
4. Commit changes to `get_all_manifests.sh` and `opt/manifests/`.
5. If upstream ClusterRoles changed, sync `config/rbac/rbac_escalate_role.yaml` and `charts/operator/templates/clusterrole-escalate.yaml`.
6. If `params.env` / `params-latest.env` gained or renamed image keys (common for
   workbench/runtime ImageStreams when Python or UBI versions change), update
   `imageParamMap` in `internal/controller/imageparams.go` so
   `RELATED_IMAGE_*` overrides still apply. Keep that map aligned with the
   platform module handler's related-image list in
   [opendatahub-operator `internal/controller/modules/workbenches`](https://github.com/opendatahub-io/opendatahub-operator/tree/main/internal/controller/modules/workbenches)
   (`relatedImages` / `injectModuleEnv`).
