#!/bin/bash
set -euo pipefail

# Workbenches module operator manifest fetching script.
# Downloads manifests from component repositories into opt/manifests/.
# Manifests are committed to the repository for hermetic container builds.
# A scheduled GitHub Action (.github/workflows/manifest-sync.yaml) refreshes them daily.
#
# Platform selection (mirrors opendatahub-operator / rhods-operator):
#   ODH_PLATFORM_TYPE=OpenDataHub  (default) — opendatahub-io upstream sources
#   ODH_PLATFORM_TYPE=rhoai        — red-hat-data-services RHOAI/downstream sources
#
# Usage:
#   ./get_all_manifests.sh [--workbenches/kf-notebook-controller=org:repo:branch@sha:source_path]
#   ODH_PLATFORM_TYPE=rhoai ./get_all_manifests.sh
#
# The script clones from the specified org/repo at the given branch@sha,
# then copies source_path contents into opt/manifests/<target>.

MANIFEST_DIR="${MANIFEST_DIR:-opt/manifests}"

# {ODH,RHOAI}_COMPONENT_MANIFESTS are lists of component repositories to fetch.
# Format: "repo-org:repo-name:ref-name:source-folder"
# Key is the target folder under opt/manifests/
# ref-name supports:
#   1. "branch"              — latest commit on branch (e.g., main)
#   2. "tag"                 — immutable reference (e.g., v1.0.0)
#   3. "branch@commit-sha"   — branch tracking pin (e.g., main@a1b2c3d4)

# ODH (upstream) Component Manifests
declare -A ODH_COMPONENT_MANIFESTS=(
    ["workbenches/kf-notebook-controller"]="opendatahub-io:kubeflow:v1.10.0-14@9945627f2bbc1fd37c36c528bf41b9d1589d0561:components/notebook-controller/config"
    ["workbenches/odh-notebook-controller"]="opendatahub-io:kubeflow:v1.10.0-14@9945627f2bbc1fd37c36c528bf41b9d1589d0561:components/odh-notebook-controller/config"
    ["workbenches/notebooks"]="opendatahub-io:notebooks:v1.47.0@fb31a5a15294d30dbd00043558d9fb3a637fd22a:manifests"
)

# RHOAI (downstream) Component Manifests
declare -A RHOAI_COMPONENT_MANIFESTS=(
    ["workbenches/kf-notebook-controller"]="red-hat-data-services:kubeflow:rhoai-3.5@1998656679e96a2d4244dddce885ce3af5885cd2:components/notebook-controller/config"
    ["workbenches/odh-notebook-controller"]="red-hat-data-services:kubeflow:rhoai-3.5@1998656679e96a2d4244dddce885ce3af5885cd2:components/odh-notebook-controller/config"
    ["workbenches/notebooks"]="red-hat-data-services:notebooks:rhoai-3.5@305fc9347bd74741ebf4d691cf6dd5e99e644b8e:manifests"
)

# Select manifests based on platform type (default: OpenDataHub / upstream).
# Only documented selectors are accepted — typos must not silently pick RHOAI.
platform_type="${ODH_PLATFORM_TYPE:-OpenDataHub}"
case "${platform_type}" in
    OpenDataHub)
        echo "Cloning manifests for ODH (upstream)"
        declare -A COMPONENT_MANIFESTS=()
        for key in "${!ODH_COMPONENT_MANIFESTS[@]}"; do
            COMPONENT_MANIFESTS["$key"]="${ODH_COMPONENT_MANIFESTS[$key]}"
        done
        ;;
    rhoai)
        echo "Cloning manifests for RHOAI (downstream)"
        declare -A COMPONENT_MANIFESTS=()
        for key in "${!RHOAI_COMPONENT_MANIFESTS[@]}"; do
            COMPONENT_MANIFESTS["$key"]="${RHOAI_COMPONENT_MANIFESTS[$key]}"
        done
        ;;
    *)
        echo "ERROR: unsupported ODH_PLATFORM_TYPE='${platform_type}' (expected OpenDataHub or rhoai)"
        exit 1
        ;;
esac

if ! command -v python3 >/dev/null 2>&1; then
    echo "ERROR: python3 is required to canonicalize paths (macOS/BSD lack GNU realpath -m)"
    exit 1
fi

# GNU realpath -m canonicalizes paths that may not exist yet; macOS/BSD realpath lacks -m.
# Logical normalization only (like realpath -ms): resolves . and .. but not symlinks.
canonicalize_path() {
    python3 -c 'import os, sys
path = sys.argv[1]
if not os.path.isabs(path):
    path = os.path.join(os.getcwd(), path)
print(os.path.normpath(path))' "$1"
}

# Resolve MANIFEST_DIR once so destination jail checks use a stable absolute prefix.
MANIFEST_DIR="$(canonicalize_path "${MANIFEST_DIR}")"

# Parse command line overrides
for arg in "$@"; do
    if [[ "${arg}" != --* ]]; then
        echo "Warning: Argument '${arg}' does not follow the '--key=value' format."
        continue
    fi
    key="${arg%%=*}"
    key="${key#--}"
    value="${arg#*=}"
    if [[ -v "COMPONENT_MANIFESTS[${key}]" ]]; then
        COMPONENT_MANIFESTS["${key}"]="${value}"
    else
        echo "Unknown manifest key: ${key}"
        echo "Valid keys: ${!COMPONENT_MANIFESTS[*]}"
        exit 1
    fi
done

TMPDIR=$(mktemp -d)
trap 'rm -rf "${TMPDIR}"' EXIT

# Collision-safe directory name for a given org/repo/ref.
clone_dir_for() {
    local org="$1"
    local repo="$2"
    local branch_sha="$3"
    # Encode path separators and @ so refs like branch@sha stay unique and filesystem-safe.
    local encoded="${org}__${repo}__${branch_sha}"
    encoded="${encoded//\//_}"
    encoded="${encoded//@/_}"
    printf '%s\n' "${TMPDIR}/${encoded}"
}

fetch_manifests() {
    local target="$1"
    local spec="$2"

    IFS=':' read -r org repo branch_sha source_path <<< "${spec}"

    if [[ -z "${org}" || -z "${repo}" || -z "${branch_sha}" || -z "${source_path}" ]]; then
        echo "ERROR: invalid spec for ${target}: '${spec}' (expected org:repo:branch[@sha]:source_path)"
        exit 1
    fi

    local branch="${branch_sha}"
    local sha=""
    if [[ "${branch_sha}" == *"@"* ]]; then
        branch="${branch_sha%%@*}"
        sha="${branch_sha#*@}"
    fi

    local repo_url="https://github.com/${org}/${repo}.git"
    local clone_dir
    clone_dir="$(canonicalize_path "$(clone_dir_for "${org}" "${repo}" "${branch_sha}")")"

    echo "Fetching ${target} from ${repo_url} (branch: ${branch}, sha: ${sha:-HEAD})"

    if [[ ! -d "${clone_dir}" ]]; then
        if [[ -n "${sha}" ]]; then
            # Pin to commit: shallow-fetch the SHA directly (branch name is tracking metadata).
            mkdir -p "${clone_dir}"
            (
                cd "${clone_dir}"
                git init -q
                git remote add origin "${repo_url}"
                git fetch --depth 1 -q origin "${sha}"
                git reset -q --hard "${sha}"
            )
        else
            git clone --depth 1 --branch "${branch}" "${repo_url}" "${clone_dir}"
        fi
    fi

    local resolved
    resolved="$(canonicalize_path "${clone_dir}/${source_path}")"
    if [[ "${resolved}" != "${clone_dir}"/* ]]; then
        echo "ERROR: source_path '${source_path}' escapes clone directory"
        exit 1
    fi

    local dest
    dest="$(canonicalize_path "${MANIFEST_DIR}/${target}")"
    if [[ "${dest}" != "${MANIFEST_DIR}" && "${dest}" != "${MANIFEST_DIR}"/* ]]; then
        echo "ERROR: target '${target}' escapes manifest directory '${MANIFEST_DIR}'"
        exit 1
    fi

    mkdir -p "${dest}"
    cp -r "${resolved}/." "${dest}/"

    echo "  -> ${dest}"
}

echo "Cleaning up ${MANIFEST_DIR}..."
rm -rf "${MANIFEST_DIR:?}"
mkdir -p "${MANIFEST_DIR}"

for target in "${!COMPONENT_MANIFESTS[@]}"; do
    fetch_manifests "${target}" "${COMPONENT_MANIFESTS[${target}]}"
done

echo "All manifests fetched successfully."
