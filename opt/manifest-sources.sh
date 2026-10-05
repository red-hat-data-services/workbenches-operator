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
    ["workbenches/kf-notebook-controller"]="opendatahub-io:kubeflow:v1.10.0-16@bda4ebbcf09d2a3961b5bf900250a5083f73058c:components/notebook-controller/config"
    ["workbenches/odh-notebook-controller"]="opendatahub-io:kubeflow:v1.10.0-16@bda4ebbcf09d2a3961b5bf900250a5083f73058c:components/odh-notebook-controller/config"
    ["workbenches/notebooks"]="opendatahub-io:notebooks:v1.49.0@7b87e8f1c76ea299aa66a3e72a42a5c108666b1e:manifests"
    ["workbenches/workspaces-controller"]="opendatahub-io:workbenches:v2.0.1@baf4bba731f6f3385adcbc8d7bdba2681bba1bb9:workspaces/controller/manifests/kustomize"
)

# RHOAI (downstream) Component Manifests
declare -A RHOAI_COMPONENT_MANIFESTS=(
    ["workbenches/kf-notebook-controller"]="red-hat-data-services:kubeflow:rhoai-3.6@2bb8164660cae82f8c99453196d5014a537f1546:components/notebook-controller/config"
    ["workbenches/odh-notebook-controller"]="red-hat-data-services:kubeflow:rhoai-3.6@2bb8164660cae82f8c99453196d5014a537f1546:components/odh-notebook-controller/config"
    ["workbenches/notebooks"]="red-hat-data-services:notebooks:rhoai-3.6@ac6bd5a306abb34362af6aa5895b6177a3d87949:manifests"
    ["workbenches/workspaces-controller"]="red-hat-data-services:workbenches:rhoai-3.6@13fb02054a8b1df1c5d06603da38d343c701705f:workspaces/controller/manifests/kustomize"
)
