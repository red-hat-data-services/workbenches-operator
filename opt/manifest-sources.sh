#!/usr/bin/env bash
# Branch-specific operand source map used by get_all_manifests.sh.
# Format: "repo-org:repo-name:ref-name:source-folder"
# Key is the target folder under opt/manifests/
# ref-name supports:
#   1. "branch"              — latest commit on branch (e.g., main)
#   2. "tag"                 — immutable reference (e.g., v1.0.0)
#   3. "branch@commit-sha"   — branch tracking pin (e.g., stable@a1b2c3d4)
#
# This file is not overwritten by main→stable / stable→v1.x sync-branches
# (see .github/workflows/sync-branches.yaml). Script changes in
# get_all_manifests.sh still propagate; pins and refs stay on the target branch.
# shellcheck disable=SC2034

# ODH (upstream) Component Manifests
declare -A ODH_COMPONENT_MANIFESTS=(
    ["workbenches/kf-notebook-controller"]="opendatahub-io:kubeflow:v1.10.0-15@9945627f2bbc1fd37c36c528bf41b9d1589d0561:components/notebook-controller/config"
    ["workbenches/odh-notebook-controller"]="opendatahub-io:kubeflow:v1.10.0-15@9945627f2bbc1fd37c36c528bf41b9d1589d0561:components/odh-notebook-controller/config"
    ["workbenches/notebooks"]="opendatahub-io:notebooks:v1.48.0@fb31a5a15294d30dbd00043558d9fb3a637fd22a:manifests"
    ["workbenches/workspaces-controller"]="opendatahub-io:workbenches:v2.0.0@c068896fd61b55eedd13ae8daa869a1bf12024f8:workspaces/controller/manifests/kustomize"
)

# RHOAI (downstream) Component Manifests
declare -A RHOAI_COMPONENT_MANIFESTS=(
    ["workbenches/kf-notebook-controller"]="red-hat-data-services:kubeflow:rhoai-3.6-ea.2@4c3a7ee2d1a7a9c0e55ed5c9d7e7fe3fead563f8:components/notebook-controller/config"
    ["workbenches/odh-notebook-controller"]="red-hat-data-services:kubeflow:rhoai-3.6-ea.2@4c3a7ee2d1a7a9c0e55ed5c9d7e7fe3fead563f8:components/odh-notebook-controller/config"
    ["workbenches/notebooks"]="red-hat-data-services:notebooks:rhoai-3.6-ea.2@ca3d14d9aa77289b32bda42ac82807b89b482e34:manifests"
    ["workbenches/workspaces-controller"]="red-hat-data-services:workbenches:rhoai-3.6-ea.2@a9068f7c894055eb28b00bef551b188b60156744:workspaces/controller/manifests/kustomize"
)
