# `opt/manifests`

This directory contains component manifests fetched by `get_all_manifests.sh`.

**Do not edit fetched files under `opt/manifests/` manually.**

Exception: `opt/manifests/workbenches/workspacekinds/` is operator-owned (not fetched from upstream) and is preserved when `get_all_manifests.sh` runs. Edit those WorkspaceKind manifests in this repository.

Manifests are refreshed automatically by the scheduled GitHub Action (`.github/workflows/manifest-sync.yaml`) or locally via:

```shell
make manifests-fetch                              # ODH (default)
make manifests-fetch ODH_PLATFORM_TYPE=rhoai      # RHOAI / downstream
```

`ODH_PLATFORM_TYPE` must be `OpenDataHub` (default) or `rhoai` and selects ODH (`opendatahub-io`) or RHOAI (`red-hat-data-services`) sources. Upstream commits ODH manifests; downstream builds fetch with `ODH_PLATFORM_TYPE=rhoai`.

Commit changes only after running the fetch script. See `DEPENDENCIES.md` for source configuration and upgrade steps.
