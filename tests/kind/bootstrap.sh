#!/usr/bin/env bash
# Install the Kind prerequisites shared by e2e and the upgrade smoke.
# The current kube context must already point at the Kind cluster.
# opt/manifests must be present (committed, or refreshed with make manifests-fetch).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${ROOT}"

# Kind nodes use the host inotify instance limit. Ubuntu and Fedora default to
# 128, and the operator then exits with "too many open files".
instances="$(sysctl -n fs.inotify.max_user_instances 2>/dev/null || cat /proc/sys/fs/inotify/max_user_instances 2>/dev/null || true)"
if [[ "${instances}" =~ ^[0-9]+$ && "${instances}" -lt 512 ]]; then
	echo "error: fs.inotify.max_user_instances is ${instances}. Kind inherits this host limit, and the operator exits with \"too many open files\"." >&2
	echo "Raise it and re-run: sudo sysctl -w fs.inotify.max_user_instances=1024" >&2
	echo "If the operator pod is already crash-looping, delete it after raising the limit so Kubernetes retries immediately." >&2
	exit 1
fi

if [[ ! -f opt/manifests/workbenches/kf-notebook-controller/crd/bases/kubeflow.org_notebooks.yaml ]]; then
	echo "opt/manifests is missing the Notebook CRD. Run 'make manifests-fetch' or use a checkout that contains opt/manifests." >&2
	exit 1
fi

echo "[kind] Installing cert-manager"
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.21.0/cert-manager.yaml
kubectl wait --for=condition=Available --timeout=120s \
	deployment/cert-manager-webhook -n cert-manager

# Adapted from https://github.com/jiridanek/rhoai-in-kind/tree/main/components/05-ca-operator
echo "[kind] Installing OpenShift service-ca-operator"
kubectl label node --all node-role.kubernetes.io/master="" --overwrite
kubectl create namespace openshift-config-managed --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f https://raw.githubusercontent.com/openshift/api/72066cc9718bcab23b025ead4925db0b823adaf0/operator/v1/zz_generated.crd-manifests/0000_50_service-ca_02_servicecas.crd.yaml
kubectl apply -f https://raw.githubusercontent.com/openshift/api/72066cc9718bcab23b025ead4925db0b823adaf0/config/v1/zz_generated.crd-manifests/0000_00_cluster-version-operator_01_clusteroperators.crd.yaml
kubectl apply -f https://raw.githubusercontent.com/openshift/api/72066cc9718bcab23b025ead4925db0b823adaf0/payload-manifests/crds/0000_10_config-operator_01_infrastructures-Default.crd.yaml
kubectl apply -f https://raw.githubusercontent.com/openshift/api/72066cc9718bcab23b025ead4925db0b823adaf0/payload-command/empty-resources/0000_05_config-operator_02_infrastructure.cr.yaml
kubectl apply -f tests/e2e/kind/service-ca-operator.yaml
kubectl wait --for=condition=Available --timeout=120s \
	deployment/service-ca-operator -n openshift-service-ca-operator
echo "[kind] Waiting for service-ca child deployment to appear..."
timeout 120s bash -c \
	'until kubectl get deployment/service-ca -n openshift-service-ca 2>/dev/null; do sleep 2; done'
kubectl scale deployment/service-ca-operator -n openshift-service-ca-operator --replicas=0
kubectl wait --for=jsonpath='{.spec.replicas}'=0 --timeout=60s \
	deployment/service-ca-operator -n openshift-service-ca-operator
kubectl patch deployment/service-ca -n openshift-service-ca \
	-p '{"spec":{"template":{"spec":{"securityContext":{"runAsUser":1001}}}}}'
kubectl wait --for=condition=Available --timeout=120s \
	deployment/service-ca -n openshift-service-ca

echo "[kind] Installing ServiceMonitor CRD"
kubectl apply -f https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/v0.80.1/example/prometheus-operator-crd/monitoring.coreos.com_servicemonitors.yaml
kubectl wait --for=condition=Established --timeout=60s \
	crd/servicemonitors.monitoring.coreos.com

echo "[kind] Installing Notebook, ImageStream, HardwareProfile, and Gateway API CRDs"
kubectl apply -f opt/manifests/workbenches/kf-notebook-controller/crd/bases/kubeflow.org_notebooks.yaml
kubectl apply -f opt/manifests/workbenches/odh-notebook-controller/crd/external/image.openshift.io_imagestream.yaml
kubectl apply -f tests/e2e/kind/infrastructure.opendatahub.io_hardwareprofiles.yaml
# The odh-notebook-controller watches these. Without them it exits during cache
# sync and leaves kubeflow-resource-stopped=odh-notebook-controller-lock set,
# so the notebook StatefulSet stays at 0 replicas.
kubectl apply -f opt/manifests/workbenches/odh-notebook-controller/crd/external/gateway.networking.k8s.io_gateways.yaml
kubectl apply -f opt/manifests/workbenches/odh-notebook-controller/crd/external/gateway.networking.k8s.io_httproutes.yaml
kubectl apply -f opt/manifests/workbenches/odh-notebook-controller/crd/external/gateway.networking.k8s.io_referencegrants.yaml
kubectl wait --for=condition=Established --timeout=60s \
	crd/gateways.gateway.networking.k8s.io \
	crd/httproutes.gateway.networking.k8s.io \
	crd/referencegrants.gateway.networking.k8s.io
