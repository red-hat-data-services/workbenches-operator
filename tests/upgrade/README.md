# Operator upgrade smoke

Installs the published baseline workbenches-operator with that ref's Helm chart, seeds Notebook workloads, rolls the current checkout, and checks that the upgrade leaves user pods alone.

Tracked by [RHOAIENG-96953](https://redhat.atlassian.net/browse/RHOAIENG-96953).

## What it checks

- The operator Deployment becomes Available on the target image.
- `default-workbenches` stays Ready, with the same `applicationsNamespace` and `workbenchNamespace`.
- `notebook-controller-deployment` and `odh-notebook-controller-manager` are Available. A crash-looping operator, notebook-controller, or Notebook pod fails the check immediately, with the container reason and a short log tail, instead of waiting out the timeout. Controller pod UIDs may change when the new image's manifests change those Deployments.
- The running Notebook's pod UID stays the same and its restart count does not increase.
- The stopped Notebook stays at 0 replicas, and `kubeflow-resource-stopped` stays equal to the value saved before the upgrade. It is stopped only after the first reconcile, because the odh controller uses that annotation as a lock while creating a Notebook and then clears it.
- A Notebook created after the upgrade still receives connection `envFrom` from the operator webhook.
- After the new operator is Ready, applied objects stay still for 20s. Aggregated admin ClusterRoles keep a non-empty `.rules` list owned by `clusterrole-aggregation-controller`, not by `workbenches-operator`, and their `resourceVersion` does not move. The static edit roles are the control for cluster-wide etcd churn. Every other labeled operand is checked by `workbenches-operator`'s managed-fields timestamp, which stays put when this operator is idle and moves when it writes again. Deployment status, ImageStream import, and a one-time `caBundle` injection may still change `resourceVersion`. This spec checks that those managed fields go quiet after an upgrade ([RHOAIENG-96364](https://redhat.atlassian.net/browse/RHOAIENG-96364)). The greenfield e2e spec is the one that also reads the operator log and rejects a reconcile loop. The Notebook checks are a separate spec, so a managed-fields failure does not hide them.

`workbenchesV2` stays `Removed`.

## Kind and OpenShift

Kind runs this smoke, including pause-image Notebook pods. The notebook controller overlay sets `USE_ISTIO=false` and creates a normal StatefulSet, so Kind's kubelet can start that pod.

Kind installs the Gateway API CRDs shipped with the notebook controller (`Gateway`, `HTTPRoute`, `ReferenceGrant`). The odh-notebook-controller watches those kinds and exits on startup when they are missing, which leaves `kubeflow-resource-stopped=odh-notebook-controller-lock` on the Notebook and the StatefulSet at 0 replicas.

Kind does not stand in for:

- ImageStream-backed workbench images. The CRD is installed, but there is no OpenShift image registry.
- Routes, OAuthClients, SCCs, and the oauth-proxy sidecar path.
- Service-CA behavior beyond the fake service-ca used for the operator webhook.

The default notebook image is `registry.k8s.io/pause:3.10`. On a shared OpenShift cluster, set `NOTEBOOK_IMAGE` to an immutable digest.

If a pause Notebook does not become Ready, the prepare phase fails. The pod checks are not skipped.

## Baseline images

The baseline chart comes from the base ref. The baseline image is the published tag for that branch, resolved to the digest that was pulled:

| Ref | Image |
| --- | --- |
| `main` | `quay.io/opendatahub/odh-workbenches-operator:main` |
| `stable` | `quay.io/opendatahub/odh-workbenches-operator:odh-stable` |

Other refs need `--baseline-image`. The image must be `quay.io/opendatahub/odh-workbenches-operator` with a tag or a `sha256` digest. `:main` can lag git `main` until Konflux publishes the next push.

Kind deploys the loaded tag with `imagePullPolicy: Never`. OpenShift deploys the digest.

Helm upgrades the Workbenches CRD because the chart renders it as a template. The harness also server-side applies `config/crd/bases/components.platform.opendatahub.io_workbenches.yaml` before the Helm upgrade.

## Local Kind

`make test-upgrade` uses Kind. Requires `kubectl`, `helm`, `kind`, `go`, and either `docker` or `podman`.

```sh
bash tests/upgrade/run.sh --mode kind
```

`--mode auto` selects OpenShift only when the current context has `ClusterVersion`. The Kind service-ca bootstrap installs other `config.openshift.io` CRDs, and a `kind-*` context stays on Kind.

That builds `workbenches-operator:upgrade-test` from the current checkout. Re-run without another build:

```sh
bash tests/upgrade/run.sh --mode kind \
  --target-image workbenches-operator:upgrade-test
```

The host `fs.inotify.max_user_instances` must be at least 512. The usual default of 128 makes the operator exit with `too many open files`.

```sh
sudo sysctl -w fs.inotify.max_user_instances=1024
```

Podman-only hosts (the script selects podman when `docker info` fails):

```sh
export CONTAINER_ENGINE=podman
export KIND_EXPERIMENTAL_PROVIDER=podman
```

`kind load docker-image` looks the image up with the docker CLI. The harness saves with the engine that pulled it and loads that archive instead. Podman stores an unqualified tag as `localhost/<name>`. The harness retags that to `docker.io/library/<name>` before loading, which is the name the kubelet looks up when `imagePullPolicy` is `Never`.

Delete the cluster with the same provider:

```sh
KIND_EXPERIMENTAL_PROVIDER=podman kind delete cluster --name workbenches-upgrade
```

## OpenShift

Uses the current kube context. This installs into that cluster.

```sh
export UPGRADE_ALLOW_EXISTING_CLUSTER=1
bash tests/upgrade/run.sh \
  --mode openshift \
  --build-target \
  --target-image-registry quay.io/<your-org>
```

## CI

[`.github/workflows/upgrade.yml`](../../.github/workflows/upgrade.yml) runs the Kind path on pull requests and pushes to `main`, `stable`, and `v1.x`. `v1.x` has no default image mapping, so that branch needs a `workflow_dispatch` `baseline_image` until one is added.

`workflow_dispatch` passes `baseline_image` through the step environment. The script accepts only `quay.io/opendatahub/odh-workbenches-operator` references.

Failure diagnostics are written to `tests/upgrade/artifacts/` (gitignored). Credential-like assignments are redacted. The upload does not include Notebook or Secret objects.

## Out of scope

N-1 release matrix, rollback, a kustomize upgrade path, and ImageStream or OAuth notebook profiles.
