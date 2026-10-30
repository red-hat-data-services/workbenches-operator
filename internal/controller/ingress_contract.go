package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	componentsv1alpha1 "github.com/opendatahub-io/workbenches-operator/api/v1alpha1"
	"github.com/opendatahub-io/workbenches-operator/internal/metadata"
)

// notebookControllerComponentNames lists the component names of the
// deployments stamped and gated for the ingress contract. Only the odh
// controller reads odh-notebook-controller-config today (the kf operand
// consumes its own config and culler ConfigMaps, see
// opt/manifests/workbenches/kf-notebook-controller/manager/manager.yaml),
// but the kf deployment is stamped and gated conservatively so the rollout
// evidence stays complete if the notebooks controller unification makes it
// a contract consumer.
var notebookControllerComponentNames = []string{"odh-notebook-controller", "kf-notebook-controller"}

// ingressContractStamp returns the canonical annotation value for a contract.
func ingressContractStamp(contract *componentsv1alpha1.IngressContractSpec) string {
	return fmt.Sprintf("%s:%d", contract.Version, contract.SourceGeneration)
}

// isNotebookControllerDeployment reports whether object labels identify a
// notebook controller deployment (the ingress contract consumer).
func isNotebookControllerDeployment(labels map[string]string) bool {
	name, found := labels[metadata.ComponentNameLabelKey]
	return found && slices.Contains(notebookControllerComponentNames, name)
}

// stampIngressContract records the projected contract on notebook controller
// deployment pod templates. A nil contract leaves annotations untouched: the
// stale stamp is inert (the rollout gate only evaluates it while a contract
// exists and requires an exact match), and writing null would coerce to ""
// on the typed map[string]string annotations, bumping the deployment
// generation and restarting pods for no reason.
func stampIngressContract(
	objects []*unstructured.Unstructured,
	contract *componentsv1alpha1.IngressContractSpec,
) error {
	if contract == nil {
		return nil
	}

	value := ingressContractStamp(contract)

	for _, obj := range objects {
		if obj.GetKind() != "Deployment" || !isNotebookControllerDeployment(obj.GetLabels()) {
			continue
		}

		annotations, _, err := unstructured.NestedMap(obj.Object, "spec", "template", "metadata", "annotations")
		if err != nil {
			return fmt.Errorf("failed to read pod template annotations for deployment %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
		}

		if annotations == nil {
			annotations = map[string]any{}
		}

		annotations[metadata.IngressContractAnnotationKey] = value

		if err := unstructured.SetNestedMap(obj.Object, annotations, "spec", "template", "metadata", "annotations"); err != nil {
			return fmt.Errorf("failed to set pod template annotations for deployment %s/%s: %w", obj.GetNamespace(), obj.GetName(), err)
		}
	}

	return nil
}

// syncIngressContractStatus publishes the IngressContractReady condition and
// status.ingressContract evidence so the platform orchestrator can gate
// assigning ingresses on observed capability rather than spec intent:
//
//   - spec.ingressContract unset (legacy tuple 000): condition False, status cleared.
//   - projected ConfigMap contract disagrees with the spec: condition False
//     (projection pending; the ConfigMap apply heals drift on reconcile).
//   - operand deployment rollout incomplete for the projected contract:
//     condition False (rollout pending).
//   - otherwise: condition True with the observed contract and the deployment
//     generation that completed the rollout.
//
// The condition is independent of the overall Ready condition: it never gates
// deployment success, it only reports capability evidence.
func (r *WorkbenchesReconciler) syncIngressContractStatus(ctx context.Context, wb *componentsv1alpha1.Workbenches) {
	l := log.FromContext(ctx)

	if wb.Spec.IngressContract == nil {
		wb.Status.IngressContract = componentsv1alpha1.IngressContractStatus{}
		meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
			Type:               conditionTypeIngressContractReady,
			Status:             metav1.ConditionFalse,
			Reason:             "IngressContractNotProjected",
			Message:            "spec.ingressContract is not set",
			ObservedGeneration: wb.Generation,
		})
		return
	}

	observed, projectionMsg := r.observeIngressContract(ctx, wb)
	if projectionMsg != "" {
		wb.Status.IngressContract = componentsv1alpha1.IngressContractStatus{}
		meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
			Type:               conditionTypeIngressContractReady,
			Status:             metav1.ConditionFalse,
			Reason:             "IngressContractProjectionPending",
			Message:            projectionMsg,
			ObservedGeneration: wb.Generation,
		})
		return
	}

	rolloutGeneration, rolloutMsg := r.ingressContractRolloutGeneration(ctx, wb)
	if rolloutMsg != "" {
		wb.Status.IngressContract = componentsv1alpha1.IngressContractStatus{
			Version:          observed.Version,
			SourceGeneration: observed.SourceGeneration,
		}
		meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
			Type:               conditionTypeIngressContractReady,
			Status:             metav1.ConditionFalse,
			Reason:             "IngressContractRolloutPending",
			Message:            rolloutMsg,
			ObservedGeneration: wb.Generation,
		})
		return
	}

	wb.Status.IngressContract = componentsv1alpha1.IngressContractStatus{
		Version:              observed.Version,
		SourceGeneration:     observed.SourceGeneration,
		DeploymentGeneration: rolloutGeneration,
	}
	meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
		Type:   conditionTypeIngressContractReady,
		Status: metav1.ConditionTrue,
		Reason: "IngressContractReady",
		Message: fmt.Sprintf("ingress contract %s (sourceGeneration %d) projected and operand rollout complete",
			observed.Version, observed.SourceGeneration),
		ObservedGeneration: wb.Generation,
	})
	l.V(1).Info("ingress contract capability evaluated",
		"version", observed.Version,
		"sourceGeneration", observed.SourceGeneration,
		"deploymentGeneration", rolloutGeneration)
}

// observeIngressContract reads the live notebook controller ConfigMap and
// returns the contract observed there. A non-empty message means the
// projection does not yet match the spec.
func (r *WorkbenchesReconciler) observeIngressContract(
	ctx context.Context, wb *componentsv1alpha1.Workbenches,
) (componentsv1alpha1.IngressContractSpec, string) {
	nsName := r.resolveOperandNamespace(wb.Spec.Platform)

	config := &corev1.ConfigMap{}
	key := client.ObjectKey{Name: NotebookControllerConfigMap, Namespace: nsName}
	if err := r.Get(ctx, key, config); err != nil {
		return componentsv1alpha1.IngressContractSpec{}, fmt.Sprintf("failed to read %s ConfigMap: %v", NotebookControllerConfigMap, err)
	}

	raw, found := config.Data[componentsv1alpha1.IngressContractConfigMapKey]
	if !found {
		return componentsv1alpha1.IngressContractSpec{}, fmt.Sprintf("contract key %q missing from %s ConfigMap",
			componentsv1alpha1.IngressContractConfigMapKey, NotebookControllerConfigMap)
	}

	var observed componentsv1alpha1.IngressContractSpec
	if err := json.Unmarshal([]byte(raw), &observed); err != nil {
		return componentsv1alpha1.IngressContractSpec{}, fmt.Sprintf("contract data in %s ConfigMap is malformed: %v", NotebookControllerConfigMap, err)
	}

	if observed != *wb.Spec.IngressContract {
		return componentsv1alpha1.IngressContractSpec{}, fmt.Sprintf("projected contract %+v does not match spec %+v", observed, *wb.Spec.IngressContract)
	}

	return observed, ""
}

// ingressContractRolloutGeneration reports the notebook controller deployment
// rollout evidence for the projected contract. A non-empty message means the
// rollout is not complete; otherwise it returns the deployment generation that
// completed the rollout. Only deployments identified as notebook controllers
// (the contract consumers) are evaluated, so unrelated component rollouts can
// neither block the condition nor pollute the reported generation. Every
// component in notebookControllerComponentNames must be present and rolled
// out: both controllers render on every platform, so a missing one means the
// evidence is incomplete and the condition fails closed.
func (r *WorkbenchesReconciler) ingressContractRolloutGeneration(
	ctx context.Context, wb *componentsv1alpha1.Workbenches,
) (int64, string) {
	nsName := r.resolveOperandNamespace(wb.Spec.Platform)

	deployments := &appsv1.DeploymentList{}
	err := r.List(ctx, deployments, client.InNamespace(nsName), client.MatchingLabels{
		metadata.ComponentLabelKey: metadata.LabelTrue,
	})
	if err != nil {
		return 0, fmt.Sprintf("failed to list deployments: %v", err)
	}

	expectedStamp := ingressContractStamp(wb.Spec.IngressContract)

	rolloutGeneration := int64(0)
	seen := make(map[string]bool, len(notebookControllerComponentNames))
	for i := range deployments.Items {
		d := &deployments.Items[i]

		if !isNotebookControllerDeployment(d.Labels) {
			continue
		}

		seen[d.Labels[metadata.ComponentNameLabelKey]] = true

		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}

		if desired < 1 {
			return 0, fmt.Sprintf("deployment %s/%s is scaled to zero", d.Namespace, d.Name)
		}

		stamp, stamped := d.Spec.Template.Annotations[metadata.IngressContractAnnotationKey]
		if !stamped || stamp != expectedStamp {
			return 0, fmt.Sprintf("deployment %s/%s is not yet carrying the projected ingress contract", d.Namespace, d.Name)
		}

		if d.Status.ObservedGeneration < d.Generation {
			return 0, fmt.Sprintf("deployment %s/%s rollout not observed by controller (generation %d, observed %d)",
				d.Namespace, d.Name, d.Generation, d.Status.ObservedGeneration)
		}

		// Requiring total replicas to equal the desired count rules out surge
		// rollouts where updated and ready replicas both reach the desired
		// count while old pods are still terminating.
		if d.Status.UpdatedReplicas < desired || d.Status.ReadyReplicas < desired || d.Status.Replicas != desired {
			return 0, fmt.Sprintf("deployment %s/%s rollout in progress (%d/%d updated, %d/%d ready, %d total)",
				d.Namespace, d.Name, d.Status.UpdatedReplicas, desired, d.Status.ReadyReplicas, desired, d.Status.Replicas)
		}

		if d.Generation > rolloutGeneration {
			rolloutGeneration = d.Generation
		}
	}

	for _, name := range notebookControllerComponentNames {
		if !seen[name] {
			return 0, "no notebook controller deployment found for component " + name
		}
	}

	return rolloutGeneration, ""
}
