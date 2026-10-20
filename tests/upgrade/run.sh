#!/usr/bin/env bash
# Upgrade smoke: baseline chart+image, seed workloads, roll the current checkout.
#
# Kind is the default. OpenShift mode uses the current kube context and refuses
# to install unless UPGRADE_ALLOW_EXISTING_CLUSTER=1.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${ROOT}"

MODE="kind"
BASELINE_IMAGE="${BASELINE_IMAGE:-${INPUT_BASELINE_IMAGE:-}}"
BASELINE_REF="${BASELINE_REF:-}"
BASELINE_ROOT="${BASELINE_ROOT:-}"
TARGET_IMAGE="${TARGET_IMAGE:-}"
BUILD_TARGET=0
TARGET_IMAGE_REGISTRY="${TARGET_IMAGE_REGISTRY:-}"
TARGET_IMAGE_TAG="${TARGET_IMAGE_TAG:-upgrade-test}"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-workbenches-upgrade}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:v1.32.0}"
OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-workbenches-operator-system}"
APPLICATIONS_NAMESPACE="${APPLICATIONS_NAMESPACE:-opendatahub}"
WORKLOAD_NAMESPACE="${WORKLOAD_NAMESPACE:-upgrade-workbenches}"
NOTEBOOK_IMAGE="${NOTEBOOK_IMAGE:-registry.k8s.io/pause:3.10}"
HELM_RELEASE="${HELM_RELEASE:-workbenches-operator}"
ARTIFACTS_DIR="${ROOT}/tests/upgrade/artifacts"
WORKTREE=""

usage() {
	cat <<'EOF'
Usage: tests/upgrade/run.sh [options]

  --mode kind|openshift|auto          Cluster mode (default kind)
  --baseline-image IMAGE              Baseline operator image
                                      (default: published tag for --baseline-ref)
  --baseline-ref REF                  Git ref whose chart is the baseline
                                      (default: BASELINE_REF, else upstream, else main)
  --baseline-root DIR                 Checkout to helm-install (skips worktree)
  --target-image IMAGE                Target operator image (skips the build)
  --build-target                      Build the target image from this checkout
  --target-image-registry REGISTRY    Registry prefix used when --build-target
                                      pushes for OpenShift
  --target-image-tag TAG              Tag used with --target-image-registry
                                      (default upgrade-test)

Environment:
  UPGRADE_ALLOW_EXISTING_CLUSTER=1    Required for openshift mode
  NOTEBOOK_IMAGE                      Notebook container image (default pause:3.10)
  CONTAINER_ENGINE                    docker or podman
  INPUT_BASELINE_IMAGE                workflow_dispatch override
EOF
}

die() {
	echo "error: $*" >&2
	exit 1
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--mode)
		MODE="${2:-}"
		shift 2
		;;
	--baseline-image)
		BASELINE_IMAGE="${2:-}"
		shift 2
		;;
	--baseline-ref)
		BASELINE_REF="${2:-}"
		shift 2
		;;
	--baseline-root)
		BASELINE_ROOT="${2:-}"
		shift 2
		;;
	--target-image)
		TARGET_IMAGE="${2:-}"
		shift 2
		;;
	--build-target)
		BUILD_TARGET=1
		shift
		;;
	--target-image-registry)
		TARGET_IMAGE_REGISTRY="${2:-}"
		shift 2
		;;
	--target-image-tag)
		TARGET_IMAGE_TAG="${2:-}"
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		die "unknown argument: $1"
		;;
	esac
done

redact() {
	sed -E \
		-e 's/[Bb]earer[[:space:]]+[A-Za-z0-9._~+/=-]+/[redacted]/g' \
		-e 's/(token|password|authorization|secret|api_key|cookie)[[:space:]]*[:=][[:space:]]*[^[:space:]]+/\1=[redacted]/Ig'
}

collect_diagnostics() {
	mkdir -p "${ARTIFACTS_DIR}"
	{
		echo "=== operator logs ==="
		kubectl logs -n "${OPERATOR_NAMESPACE}" "deployment/${HELM_RELEASE}" --tail=80 2>&1 || true
		echo "=== notebook-controller logs ==="
		kubectl logs -n "${APPLICATIONS_NAMESPACE}" deploy/notebook-controller-deployment --previous --tail=80 2>&1 || true
		kubectl logs -n "${APPLICATIONS_NAMESPACE}" deploy/notebook-controller-deployment --tail=40 2>&1 || true
		echo "=== odh-notebook-controller logs ==="
		kubectl logs -n "${APPLICATIONS_NAMESPACE}" deploy/odh-notebook-controller-manager --previous --tail=80 2>&1 || true
		kubectl logs -n "${APPLICATIONS_NAMESPACE}" deploy/odh-notebook-controller-manager --tail=40 2>&1 || true
		echo "=== pods ==="
		kubectl get pods -A 2>&1 || true
	} | redact >"${ARTIFACTS_DIR}/diagnostics.log" || true
	echo "[upgrade] diagnostics written to ${ARTIFACTS_DIR}/diagnostics.log" >&2
}

cleanup() {
	local status=$?
	if [[ "${status}" -ne 0 ]]; then
		collect_diagnostics || true
	fi
	if [[ -n "${WORKTREE}" && -d "${WORKTREE}" ]]; then
		git -C "${ROOT}" worktree remove --force "${WORKTREE}" >/dev/null 2>&1 || true
	fi
	exit "${status}"
}
trap cleanup EXIT

require_cmd() {
	command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

detect_engine() {
	if [[ -n "${CONTAINER_ENGINE:-}" ]]; then
		return
	fi
	if docker info >/dev/null 2>&1; then
		CONTAINER_ENGINE="docker"
		return
	fi
	if podman info >/dev/null 2>&1; then
		CONTAINER_ENGINE="podman"
		export KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-podman}"
		return
	fi
	die "docker or podman is required"
}

baseline_image_allowed() {
	local image="$1"
	[[ "${image}" =~ ^quay\.io/opendatahub/odh-workbenches-operator:[A-Za-z0-9._-]+$ ]] && return 0
	[[ "${image}" =~ ^quay\.io/opendatahub/odh-workbenches-operator@sha256:[a-f0-9]{64}$ ]] && return 0
	return 1
}

image_for_ref() {
	case "$1" in
	main) echo "quay.io/opendatahub/odh-workbenches-operator:main" ;;
	stable) echo "quay.io/opendatahub/odh-workbenches-operator:odh-stable" ;;
	*) return 1 ;;
	esac
}

resolve_baseline_ref() {
	if [[ -n "${BASELINE_REF}" ]]; then
		return
	fi
	local upstream short
	if upstream="$(git -C "${ROOT}" rev-parse --abbrev-ref '@{upstream}' 2>/dev/null)"; then
		short="${upstream#*/}"
		case "${short}" in
		main | stable)
			BASELINE_REF="${short}"
			return
			;;
		esac
	fi
	BASELINE_REF="main"
}

resolve_mode() {
	local context
	case "${MODE}" in
	kind | openshift) ;;
	auto)
		# Kind bootstrap installs config.openshift.io CRDs for service-ca.
		# A real cluster also has ClusterVersion; a kind-* context does not.
		context="$(kubectl config current-context 2>/dev/null || true)"
		if [[ "${context}" == kind-* ]]; then
			MODE="kind"
		elif kubectl get --raw /apis/config.openshift.io/v1/clusterversions >/dev/null 2>&1; then
			MODE="openshift"
		else
			MODE="kind"
		fi
		;;
	*) die "--mode must be kind, openshift, or auto" ;;
	esac
	echo "[upgrade] selected mode ${MODE}"
}

ensure_kind_cluster() {
	require_cmd kind
	require_cmd kubectl
	detect_engine
	if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
		echo "[kind] reusing cluster ${KIND_CLUSTER_NAME}"
	else
		echo "[kind] creating cluster ${KIND_CLUSTER_NAME}"
		kind create cluster --name "${KIND_CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}"
	fi
	kind export kubeconfig --name "${KIND_CLUSTER_NAME}"
	bash "${ROOT}/tests/kind/bootstrap.sh"
}

ensure_openshift_cluster() {
	require_cmd kubectl
	[[ "${UPGRADE_ALLOW_EXISTING_CLUSTER:-}" == "1" ]] ||
		die "openshift mode requires UPGRADE_ALLOW_EXISTING_CLUSTER=1"
	kubectl cluster-info >/dev/null 2>&1 || die "current kube context is not reachable"
	echo "[openshift] using the current kube context"
}

repo_digest() {
	local image="$1"
	local digest
	digest="$("${CONTAINER_ENGINE}" image inspect --format '{{index .RepoDigests 0}}' "${image}" 2>/dev/null || true)"
	[[ -n "${digest}" && "${digest}" != "<no value>" ]] || die "image ${image} has no repo digest"
	echo "${digest}"
}

# containerd treats an unqualified name as docker.io/library/<name>. Podman
# records the same build as localhost/<name>, and kind load keeps that prefix.
kind_load_name() {
	local image="$1"
	if [[ "${image}" != */* ]]; then
		echo "docker.io/library/${image}"
		return
	fi
	echo "${image}"
}

load_kind_image() {
	local image="$1"
	echo "[kind] loading ${image}"
	# kind load docker-image always inspects and saves through the docker CLI,
	# including when the cluster provider is podman. After a podman pull that
	# check reports the image as missing, so save with the engine that has it.
	if docker image inspect "${image}" >/dev/null 2>&1; then
		kind load docker-image "${image}" --name "${KIND_CLUSTER_NAME}"
		return
	fi
	local archive status=0 load_name
	load_name="$(kind_load_name "${image}")"
	if [[ "${load_name}" != "${image}" ]]; then
		"${CONTAINER_ENGINE}" tag "${image}" "${load_name}"
	fi
	archive="$(mktemp "${TMPDIR:-/tmp}/kind-image.XXXXXX.tar")"
	"${CONTAINER_ENGINE}" save --output "${archive}" "${load_name}"
	kind load image-archive "${archive}" --name "${KIND_CLUSTER_NAME}" || status=$?
	rm -f "${archive}"
	return "${status}"
}

prepare_baseline_root() {
	if [[ -n "${BASELINE_ROOT}" ]]; then
		[[ -f "${BASELINE_ROOT}/charts/operator/Chart.yaml" ]] ||
			die "baseline root ${BASELINE_ROOT} has no charts/operator/Chart.yaml"
		return
	fi
	if [[ -f "${ROOT}/baseline/charts/operator/Chart.yaml" ]]; then
		BASELINE_ROOT="${ROOT}/baseline"
		return
	fi
	resolve_baseline_ref
	# A leading "-" is a git option, not a ref. The rest of the name stays as before.
	[[ "${BASELINE_REF}" =~ ^[A-Za-z0-9][A-Za-z0-9._/-]*$ ]] || die "refusing baseline ref ${BASELINE_REF}"
	echo "[upgrade] fetching origin/${BASELINE_REF}"
	git -C "${ROOT}" fetch --depth=1 origin "${BASELINE_REF}"
	WORKTREE="$(mktemp -d "${TMPDIR:-/tmp}/workbenches-baseline.XXXXXX")"
	rmdir "${WORKTREE}"
	git -C "${ROOT}" worktree add --detach "${WORKTREE}" FETCH_HEAD
	BASELINE_ROOT="${WORKTREE}"
}

sync_chart_crd() {
	local root="$1"
	echo "[upgrade] syncing Helm CRD in ${root}"
	make -C "${root}" chart-sync-crd
}

helm_deploy() {
	local chart="$1"
	local image="$2"
	local pull_policy="$3"
	local tls_provider="$4"
	local -a digest_sets=()
	if [[ "${image}" =~ ^(.+)@sha256:([a-f0-9]{64})$ ]]; then
		digest_sets+=(--set-string "image.repository=${BASH_REMATCH[1]}")
		digest_sets+=(--set-string "image.digest=sha256:${BASH_REMATCH[2]}")
	fi
	helm upgrade --install "${HELM_RELEASE}" "${chart}" \
		--namespace "${OPERATOR_NAMESPACE}" \
		--create-namespace \
		--set-string "operatorNamespace=${OPERATOR_NAMESPACE}" \
		--set-string "applicationsNamespace=${APPLICATIONS_NAMESPACE}" \
		--set leaderElection.enabled=false \
		--set-string "params.workbenchesOperatorImage=${image}" \
		--set-string "image.pullPolicy=${pull_policy}" \
		--set "webhooks.tlsProvider=${tls_provider}" \
		${digest_sets[@]+"${digest_sets[@]}"}
}

wait_for_operator() {
	local deadline=$((SECONDS + 180))
	local reason
	while (( SECONDS < deadline )); do
		if kubectl rollout status "deployment/${HELM_RELEASE}" \
			-n "${OPERATOR_NAMESPACE}" --timeout=15s; then
			return 0
		fi
		# rollout status waits out CrashLoopBackOff. Stop when the pod has already exited.
		reason="$(kubectl get pods -n "${OPERATOR_NAMESPACE}" \
			-l app.kubernetes.io/name=workbenches-operator \
			-o jsonpath='{range .items[*].status.containerStatuses[*]}{.state.waiting.reason}{" "}{end}' 2>/dev/null || true)"
		case "${reason}" in
		*CrashLoopBackOff* | *OOMKilled* | *RunContainerError* | *ErrImageNeverPull* | *Error*)
			kubectl logs -n "${OPERATOR_NAMESPACE}" "deployment/${HELM_RELEASE}" --tail=40 >&2 || true
			die "operator pod is not starting: ${reason}"
			;;
		esac
	done
	die "timed out waiting for deployment/${HELM_RELEASE} to roll out"
}

run_phase() {
	local phase="$1"
	local expected="${2:-}"
	UPGRADE_PHASE="${phase}" \
		EXPECTED_OPERATOR_IMAGE="${expected}" \
		OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE}" \
		APPLICATIONS_NAMESPACE="${APPLICATIONS_NAMESPACE}" \
		WORKLOAD_NAMESPACE="${WORKLOAD_NAMESPACE}" \
		NOTEBOOK_IMAGE="${NOTEBOOK_IMAGE}" \
		go test ./tests/upgrade/ -count=1 -timeout 25m
}

require_cmd helm
require_cmd kubectl
require_cmd go

resolve_mode

case "${MODE}" in
kind)
	ensure_kind_cluster
	PULL_POLICY="Never"
	TLS_PROVIDER="certmanager"
	;;
openshift)
	ensure_openshift_cluster
	detect_engine
	PULL_POLICY="IfNotPresent"
	TLS_PROVIDER="openshift"
	;;
*)
	die "unhandled mode ${MODE}"
	;;
esac

mkdir -p "${ARTIFACTS_DIR}"
kubectl create namespace "${APPLICATIONS_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

resolve_baseline_ref
if [[ -z "${BASELINE_IMAGE}" ]]; then
	BASELINE_IMAGE="$(image_for_ref "${BASELINE_REF}")" ||
		die "no published baseline image for ref ${BASELINE_REF}; pass --baseline-image"
fi
baseline_image_allowed "${BASELINE_IMAGE}" ||
	die "baseline image must be quay.io/opendatahub/odh-workbenches-operator:<tag> or @sha256:<digest>"

detect_engine
echo "[upgrade] pulling baseline ${BASELINE_IMAGE}"
"${CONTAINER_ENGINE}" pull "${BASELINE_IMAGE}"
BASELINE_DIGEST="$(repo_digest "${BASELINE_IMAGE}")"
echo "[upgrade] baseline digest ${BASELINE_DIGEST}"
# Kind deploys the loaded tag with imagePullPolicy Never so the node cannot
# re-pull a floating tag. OpenShift deploys the immutable digest.
if [[ "${MODE}" == "kind" ]]; then
	load_kind_image "${BASELINE_IMAGE}"
	BASELINE_DEPLOY="${BASELINE_IMAGE}"
else
	BASELINE_DEPLOY="${BASELINE_DIGEST}"
fi

prepare_baseline_root
sync_chart_crd "${BASELINE_ROOT}"

echo "[upgrade] installing baseline"
helm_deploy "${BASELINE_ROOT}/charts/operator" "${BASELINE_DEPLOY}" "${PULL_POLICY}" "${TLS_PROVIDER}"
wait_for_operator

echo "[upgrade] seeding workloads"
run_phase prepare

sync_chart_crd "${ROOT}"
echo "[upgrade] applying target Workbenches CRD"
kubectl apply --server-side --force-conflicts --field-manager=workbenches-upgrade-test \
	-f "${ROOT}/config/crd/bases/components.platform.opendatahub.io_workbenches.yaml"

if [[ -z "${TARGET_IMAGE}" || "${BUILD_TARGET}" -eq 1 ]]; then
	if [[ "${MODE}" == "openshift" ]]; then
		[[ -n "${TARGET_IMAGE_REGISTRY}" ]] ||
			die "openshift --build-target requires --target-image-registry"
		TARGET_IMAGE="${TARGET_IMAGE_REGISTRY%/}/odh-workbenches-operator:${TARGET_IMAGE_TAG}"
	elif [[ -z "${TARGET_IMAGE}" ]]; then
		TARGET_IMAGE="workbenches-operator:${TARGET_IMAGE_TAG}"
	fi
	echo "[upgrade] building ${TARGET_IMAGE}"
	make image-build "IMG=${TARGET_IMAGE}" "CONTAINER_ENGINE=${CONTAINER_ENGINE}"
	if [[ "${MODE}" == "openshift" ]]; then
		echo "[upgrade] pushing ${TARGET_IMAGE}"
		make image-push "IMG=${TARGET_IMAGE}" "CONTAINER_ENGINE=${CONTAINER_ENGINE}"
		TARGET_DEPLOY="$(repo_digest "${TARGET_IMAGE}")"
	else
		TARGET_DEPLOY="${TARGET_IMAGE}"
	fi
else
	if [[ "${MODE}" == "openshift" ]]; then
		echo "[upgrade] pulling target ${TARGET_IMAGE}"
		"${CONTAINER_ENGINE}" pull "${TARGET_IMAGE}"
		TARGET_DEPLOY="$(repo_digest "${TARGET_IMAGE}")"
	else
		if ! "${CONTAINER_ENGINE}" image inspect "${TARGET_IMAGE}" >/dev/null 2>&1; then
			echo "[upgrade] pulling target ${TARGET_IMAGE}"
			"${CONTAINER_ENGINE}" pull "${TARGET_IMAGE}"
		fi
		TARGET_DEPLOY="${TARGET_IMAGE}"
	fi
fi

if [[ "${MODE}" == "kind" ]]; then
	load_kind_image "${TARGET_IMAGE}"
fi

echo "[upgrade] upgrading to ${TARGET_DEPLOY}"
helm_deploy "${ROOT}/charts/operator" "${TARGET_DEPLOY}" "${PULL_POLICY}" "${TLS_PROVIDER}"
wait_for_operator

echo "[upgrade] verifying invariants"
run_phase verify "${TARGET_DEPLOY}"
echo "[upgrade] passed"
