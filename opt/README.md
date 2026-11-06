# `opt/manifests`

This directory contains component manifests fetched by `get_all_manifests.sh`.

**Do not edit fetched files under `opt/manifests/` manually.**

Exception: `opt/manifests/workbenches/workspacekinds/` is operator-owned (not fetched from upstream) and is preserved when `get_all_manifests.sh` runs. Edit those WorkspaceKind manifests in this repository.

Manifests are refreshed automatically by `.github/workflows/manifests-sync-main.yaml` (daily PR on `main`) and `.github/workflows/manifests-sync-stable.yaml` (direct commit on push to `stable`/`v1.x` and daily on `stable`; SHA pin bump on `stable` only), or locally via:

```shell
bash ci/bump-odh-manifest-shas.sh                 # optional: refresh ODH branch@sha pins in opt/manifest-sources.sh
make manifests-fetch                              # ODH (default)
make manifests-fetch ODH_PLATFORM_TYPE=rhoai      # RHOAI / downstream
```

Operand org/repo/ref maps live in `opt/manifest-sources.sh` (not under `opt/manifests/`). Branch sync keeps the target copy of that file so pins are not overwritten.

`ODH_PLATFORM_TYPE` must be `OpenDataHub` (default) or `rhoai` and selects ODH (`opendatahub-io`) or RHOAI (`red-hat-data-services`) sources. Upstream commits ODH manifests. `make manifests-fetch ODH_PLATFORM_TYPE=rhoai` fetches downstream sources locally.

Downstream (`red-hat-data-services/workbenches-operator`) release branches are refreshed separately by the [rhods-devops-infra](https://github.com/red-hat-data-services/rhods-devops-infra) [modular operators manifest sync](https://github.com/red-hat-data-services/rhods-devops-infra/tree/main/tools/modular-operators-manifest-validator#4-enable-automatic-synchronization). With `auto-sync` enabled, that job copies operand manifests into `opt/manifests/` from the per-release maps under `tools/modular-operators-manifest-validator/config/` (for example `rhoai-3.6.yaml`), for branches listed in `src/config/releases.yaml`. It does not write RHDS `main`. This repo has no `build/manifests-config.yaml`. `workspacekinds/` stays operator-owned.

Commit changes only after running the fetch script. See `DEPENDENCIES.md` for source configuration and upgrade steps.
