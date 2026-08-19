/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package controller contains the Workbenches reconciler.
package controller

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	componentsv1alpha1 "github.com/opendatahub-io/workbenches-operator/api/v1alpha1"
	"github.com/opendatahub-io/workbenches-operator/internal/gvk"
	"github.com/opendatahub-io/workbenches-operator/internal/metadata"
	"github.com/opendatahub-io/workbenches-operator/internal/platform"
	"github.com/opendatahub-io/workbenches-operator/internal/platformconfig"
	"github.com/opendatahub-io/workbenches-operator/internal/releases"
	statusutil "github.com/opendatahub-io/workbenches-operator/internal/status"
)

const (
	conditionTypeReady                    = "Ready"
	conditionTypeProvisioningSucceeded    = "ProvisioningSucceeded"
	conditionTypeDegraded                 = "Degraded"
	conditionTypeDeploymentsAvailable     = "DeploymentsAvailable"
	conditionTypeReleaseMetadataAvailable = "ReleaseMetadataAvailable"
	// ImageStreamsAvailable is informational only (matches ODH); it does not gate Ready.
	conditionTypeImageStreamsAvailable = "ImageStreamsAvailable"
	// WorkbenchesV2Ready is informational only; it does not gate Ready.
	conditionTypeWorkbenchesV2Ready     = "WorkbenchesV2Ready"
	conditionReasonImageStreamsNotReady = "ImageStreamsNotReady"
	conditionReasonUnknown              = "Unknown"
	conditionReasonAvailable            = "Available"
	conditionReasonReconcileSuccess     = "ReconcileSuccess"
	requeueDelay                        = 30 * time.Second

	rateLimiterBaseDelay = 5 * time.Second
	rateLimiterMaxDelay  = 5 * time.Minute

	workbenchesFinalizer = "components.platform.opendatahub.io/workbenches-cleanup"

	workspacesControllerDeploymentName = "workspaces-controller"

	paramGatewayURL    = "gateway-url"
	paramMLflowEnabled = "mlflow-enabled"
	paramSectionTitle  = "section-title"
)

// WorkbenchesReconciler reconciles a Workbenches object.
type WorkbenchesReconciler struct {
	client.Client

	Scheme                *runtime.Scheme
	ManifestsBasePath     string
	ApplicationsNamespace string
}

// +kubebuilder:rbac:groups=components.platform.opendatahub.io,resources=workbenches,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=components.platform.opendatahub.io,resources=workbenches/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=components.platform.opendatahub.io,resources=workbenches/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps;secrets;services;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// escalate and bind for RBAC resources are granted in a separate hand-maintained ClusterRole
// (config/rbac/rbac_escalate_role.yaml) scoped to specific resourceNames from upstream manifests.
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings;clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;create;update;patch
// Write verbs are required because the operator creates/patches webhook configs from upstream manifests via SSA
// and deletes them during component removal.
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations;validatingwebhookconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubeflow.org,resources=notebooks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:groups=image.openshift.io,resources=imagestreams,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=config.openshift.io,resources=apiservers,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile handles the reconciliation loop for Workbenches resources.
func (r *WorkbenchesReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	workbenches := &componentsv1alpha1.Workbenches{}

	err := r.Get(ctx, req.NamespacedName, workbenches)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	l.Info("reconciling Workbenches", "name", workbenches.Name, "generation", workbenches.Generation)

	if !workbenches.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, workbenches)
	}

	if !controllerutil.ContainsFinalizer(workbenches, workbenchesFinalizer) {
		controllerutil.AddFinalizer(workbenches, workbenchesFinalizer)

		if err := r.Update(ctx, workbenches); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
	}

	if workbenches.Spec.ManagementState == componentsv1alpha1.ManagementStateRemoved {
		return r.reconcileRemoved(ctx, workbenches)
	}

	return r.reconcileManaged(ctx, workbenches)
}

// SetupWithManager sets up the controller with the Manager.
// A custom rate limiter is configured with exponential backoff (5s base, 5m max)
// to avoid tight retry loops on persistent failures like missing manifests.
func (r *WorkbenchesReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctrlBuilder := ctrl.NewControllerManagedBy(mgr).
		For(&componentsv1alpha1.Workbenches{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Owned operands — OwnerReferences are set in applyObjects via SetControllerReference.
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&rbacv1.ClusterRole{}).
		Owns(&rbacv1.ClusterRoleBinding{}).
		Owns(&admissionregistrationv1.MutatingWebhookConfiguration{}).
		Owns(&admissionregistrationv1.ValidatingWebhookConfiguration{}).
		// Deployments also need status (replica) updates, not only generation changes.
		Owns(&appsv1.Deployment{}, builder.WithPredicates(deploymentAvailabilityChangedPredicate{})).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.mapPlatformConfigToWorkbenches),
			builder.WithPredicates(newPlatformConfigChangedPredicate(r.platformConfigWatchNamespaces()...)),
		).
		Named("workbenches").
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](
				rateLimiterBaseDelay,
				rateLimiterMaxDelay,
			),
		})

	// Watch ImageStreams for drift reconcile + ImageStreamsAvailable status.
	// Do not Own them and do not list them in cleanupGVKs (ODH onlyOwned parity).
	// Filter to managed part-of labels so unrelated ImageStream churn does not
	// enqueue full reconciles (module-operator hardening vs ODH watch-all).
	watchIS, err := shouldWatchImageStreams(mgr.GetRESTMapper())
	if err != nil {
		return fmt.Errorf("failed to check ImageStream API availability: %w", err)
	}

	if watchIS {
		imageStream := &unstructured.Unstructured{}
		imageStream.SetGroupVersionKind(gvk.ImageStream)
		ctrlBuilder = ctrlBuilder.Watches(
			imageStream,
			handler.EnqueueRequestsFromMapFunc(r.mapImageStreamToWorkbenches),
			builder.WithPredicates(predicate.NewPredicateFuncs(isManagedPartOfLabel)),
		)
	}

	return ctrlBuilder.Complete(r)
}

// platformConfigWatchNamespaces returns namespaces whose odh-workbenches-config
// ConfigMap should trigger reconcile. When APPLICATIONS_NAMESPACE is set, watch
// only that namespace; otherwise watch both platform defaults until CR platform
// selects one.
func (r *WorkbenchesReconciler) platformConfigWatchNamespaces() []string {
	if ns := r.configuredApplicationsNamespace(); ns != "" {
		return []string{ns}
	}

	return []string{
		platform.DefaultApplicationsNamespaceODH,
		platform.DefaultApplicationsNamespaceRHOAI,
	}
}

// configuredApplicationsNamespace returns ApplicationsNamespace when it is a valid
// DNS-1123 label. Invalid values are ignored so reconcile and ConfigMap watches
// fall back to platform defaults consistently.
func (r *WorkbenchesReconciler) configuredApplicationsNamespace() string {
	return platform.ValidApplicationsNamespace(r.ApplicationsNamespace)
}

func shouldWatchImageStreams(mapper meta.RESTMapper) (bool, error) {
	_, err := mapper.RESTMapping(gvk.ImageStream.GroupKind(), gvk.ImageStream.Version)
	if err == nil {
		return true, nil
	}

	if meta.IsNoMatchError(err) {
		// Vanilla Kubernetes / envtest without OpenShift image API.
		return false, nil
	}

	return false, err
}

func (r *WorkbenchesReconciler) mapPlatformConfigToWorkbenches(ctx context.Context, obj client.Object) []reconcile.Request {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok || cm == nil {
		return nil
	}

	if cm.GetName() != platformconfig.ConfigMapName {
		return nil
	}

	wb := &componentsv1alpha1.Workbenches{}
	err := r.Get(ctx, types.NamespacedName{Name: componentsv1alpha1.WorkbenchesInstanceName}, wb)
	if err != nil {
		if !errors.IsNotFound(err) {
			log.FromContext(ctx).Error(err, "failed to get Workbenches for platform config watch")
		}

		// No CR yet: accept ConfigMaps in any watched default apps namespace.
		if slices.Contains(r.platformConfigWatchNamespaces(), cm.GetNamespace()) {
			return []reconcile.Request{{
				NamespacedName: types.NamespacedName{Name: componentsv1alpha1.WorkbenchesInstanceName},
			}}
		}

		return nil
	}

	if cm.GetNamespace() != r.resolveOperandNamespace(wb.Spec.Platform) {
		return nil
	}

	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: componentsv1alpha1.WorkbenchesInstanceName}}}
}

// mapImageStreamToWorkbenches enqueues the singleton Workbenches CR for managed
// ImageStream events (predicate already filters to part-of=workbenches).
func (r *WorkbenchesReconciler) mapImageStreamToWorkbenches(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: componentsv1alpha1.WorkbenchesInstanceName}}}
}

func isManagedPartOfLabel(obj client.Object) bool {
	return obj != nil && obj.GetLabels()[metadata.PartOfLabelKey] == metadata.ComponentLabelValue
}

type deploymentAvailabilityChangedPredicate struct{}

func (deploymentAvailabilityChangedPredicate) Create(e event.CreateEvent) bool {
	return hasComponentLabel(e.Object)
}

func (deploymentAvailabilityChangedPredicate) Update(e event.UpdateEvent) bool {
	oldHasLabel := hasComponentLabel(e.ObjectOld)
	newHasLabel := hasComponentLabel(e.ObjectNew)
	if oldHasLabel != newHasLabel {
		return true
	}

	if !newHasLabel {
		return false
	}

	oldDeploy, oldOK := e.ObjectOld.(*appsv1.Deployment)
	newDeploy, newOK := e.ObjectNew.(*appsv1.Deployment)
	if !oldOK || !newOK {
		return true
	}

	oldDesired := deploymentDesiredReplicas(oldDeploy)
	newDesired := deploymentDesiredReplicas(newDeploy)

	return oldDeploy.Status.ReadyReplicas != newDeploy.Status.ReadyReplicas ||
		oldDeploy.Status.AvailableReplicas != newDeploy.Status.AvailableReplicas ||
		oldDesired != newDesired
}

func (deploymentAvailabilityChangedPredicate) Delete(e event.DeleteEvent) bool {
	return hasComponentLabel(e.Object)
}

func (deploymentAvailabilityChangedPredicate) Generic(_ event.GenericEvent) bool {
	return false
}

func hasComponentLabel(obj client.Object) bool {
	return obj.GetLabels()[metadata.ComponentLabelKey] == metadata.LabelTrue
}

func deploymentDesiredReplicas(deploy *appsv1.Deployment) int32 {
	if deploy.Spec.Replicas == nil {
		return 1
	}

	return *deploy.Spec.Replicas
}

func (r *WorkbenchesReconciler) reconcileDelete(ctx context.Context, wb *componentsv1alpha1.Workbenches) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	l.Info("workbenches CR is being deleted, cleaning up managed resources")

	if controllerutil.ContainsFinalizer(wb, workbenchesFinalizer) {
		nsName := r.resolveOperandNamespace(wb.Spec.Platform)

		if err := r.cleanupManagedResources(ctx, nsName); err != nil {
			l.Error(err, "failed to cleanup managed resources")

			return ctrl.Result{}, err
		}

		controllerutil.RemoveFinalizer(wb, workbenchesFinalizer)

		if err := r.Update(ctx, wb); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

func (r *WorkbenchesReconciler) reconcileRemoved(ctx context.Context, wb *componentsv1alpha1.Workbenches) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	l.Info("workbenches management state is Removed")

	nsName := r.resolveOperandNamespace(wb.Spec.Platform)
	r.setStatusNamespaces(wb, nsName)

	if err := r.cleanupManagedResources(ctx, nsName); err != nil {
		return r.setErrorStatus(ctx, wb, "CleanupFailed", err)
	}

	meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             componentsv1alpha1.ManagementStateRemoved,
		Message:            "Workbenches component has been removed",
		ObservedGeneration: wb.Generation,
	})

	meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
		Type:               conditionTypeProvisioningSucceeded,
		Status:             metav1.ConditionFalse,
		Reason:             componentsv1alpha1.ManagementStateRemoved,
		Message:            "Workbenches component has been removed",
		ObservedGeneration: wb.Generation,
	})

	meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
		Type:               conditionTypeWorkbenchesV2Ready,
		Status:             metav1.ConditionFalse,
		Reason:             componentsv1alpha1.ManagementStateRemoved,
		Message:            "Workbenches component has been removed",
		ObservedGeneration: wb.Generation,
	})

	wb.Status.Phase = statusutil.ComputePhase(statusutil.PhaseContext{Removed: true})
	wb.Status.Releases = nil
	wb.Status.Distribution = componentsv1alpha1.Distribution{}
	wb.Status.ObservedGeneration = wb.Generation

	sanitizeConditions(wb.Status.Conditions)

	err := r.Status().Update(ctx, wb)

	return ctrl.Result{}, err
}

func (r *WorkbenchesReconciler) populateReleases(wb *componentsv1alpha1.Workbenches) error {
	platformRelease := platformconfig.GetPlatformRelease(wb.Status.Releases)

	componentReleases, err := releases.CollectWorkbenchesReleases(r.ManifestsBasePath)
	if err != nil {
		return fmt.Errorf("collecting component releases: %w", err)
	}

	wb.Status.Releases = platformconfig.MergeComponentReleases(componentReleases, platformRelease)

	return nil
}

func (r *WorkbenchesReconciler) reconcileManaged(ctx context.Context, wb *componentsv1alpha1.Workbenches) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	phaseCtx := statusutil.PhaseContext{
		PreviousObservedGeneration: wb.Status.ObservedGeneration,
		Generation:                 wb.Generation,
		WasReady:                   meta.IsStatusConditionTrue(wb.Status.Conditions, conditionTypeReady),
	}

	if wb.Status.Phase == "" && wb.Status.ObservedGeneration == 0 {
		wb.Status.Phase = statusutil.PhasePending

		sanitizeConditions(wb.Status.Conditions)

		if err := r.Status().Update(ctx, wb); err != nil {
			l.Error(err, "failed to update Pending status")

			return ctrl.Result{RequeueAfter: requeueDelay}, err
		}

		// Requeue immediately to continue provisioning after the first Pending status write.
		return ctrl.Result{Requeue: true}, nil
	}

	// Resolve and record namespaces early so error paths still persist them.
	nsName := r.resolveOperandNamespace(wb.Spec.Platform)
	r.setStatusNamespaces(wb, nsName)

	if err := validateSpec(wb.Spec); err != nil {
		return r.setErrorStatus(ctx, wb, "InvalidSpec", err)
	}

	desiredDistribution, err := r.resolveDesiredDistribution(ctx, wb)
	if err != nil {
		return r.setErrorStatus(ctx, wb, "PlatformConfigReadFailed", err)
	}

	platformVersion, err := r.readPlatformVersion(ctx, wb)
	if err != nil {
		return r.setErrorStatus(ctx, wb, "PlatformVersionReadFailed", err)
	}

	if err = r.configureDependencies(ctx, wb); err != nil {
		return r.setErrorStatus(ctx, wb, "ConfigureDependenciesFailed", err)
	}

	params := r.computeKustomizeParams(wb)
	l.V(1).Info("computed kustomize params", "params", params)

	if err = r.renderAndApply(ctx, wb, params, nsName, wb.Spec.Platform); err != nil {
		return r.setErrorStatus(ctx, wb, "ManifestApplyFailed", err)
	}

	if err = r.populateReleases(wb); err != nil {
		// Release metadata is informational; a missing or malformed
		// component_metadata.yaml must not block a successful deploy.
		l.Error(err, "failed to populate release metadata; continuing with empty releases")
		platformRelease := platformconfig.GetPlatformRelease(wb.Status.Releases)
		wb.Status.Releases = platformconfig.MergeComponentReleases(nil, platformRelease)
		meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
			Type:               conditionTypeReleaseMetadataAvailable,
			Status:             metav1.ConditionFalse,
			Reason:             "ReleaseMetadataFailed",
			Message:            err.Error(),
			ObservedGeneration: wb.Generation,
		})
	} else {
		meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
			Type:               conditionTypeReleaseMetadataAvailable,
			Status:             metav1.ConditionTrue,
			Reason:             conditionReasonAvailable,
			Message:            "Component release metadata is available",
			ObservedGeneration: wb.Generation,
		})
	}

	meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
		Type:               conditionTypeProvisioningSucceeded,
		Status:             metav1.ConditionTrue,
		Reason:             "Provisioned",
		Message:            "Workbenches manifests have been provisioned",
		ObservedGeneration: wb.Generation,
	})

	deploymentsReady, deployMsg := r.checkDeployments(ctx, wb)
	r.setDeploymentCondition(wb, deploymentsReady, deployMsg)

	r.setWorkbenchesV2Condition(ctx, wb)

	if err = r.syncImageStreamsAvailable(ctx, wb, nsName); err != nil {
		return r.setErrorStatus(ctx, wb, "ImageStreamsStatusFailed", err)
	}

	provisioningSucceeded := meta.IsStatusConditionTrue(wb.Status.Conditions, conditionTypeProvisioningSucceeded)
	if deploymentsReady && provisioningSucceeded &&
		!platformconfig.DistributionAligned(desiredDistribution, wb.Status.Distribution) {
		wb.Status.Distribution = desiredDistribution
	}

	currentPlatformVersion := platformconfig.GetPlatformRelease(wb.Status.Releases).Version
	wasPlatformVersionPending := false
	if readyCond := meta.FindStatusCondition(wb.Status.Conditions, conditionTypeReady); readyCond != nil {
		wasPlatformVersionPending = readyCond.Reason == "PlatformVersionPending"
	}

	handshakeRequired := platformconfig.HandshakeRequired(desiredDistribution)

	reconciledPlatformVersion := currentPlatformVersion
	if deploymentsReady &&
		platformVersion != "" &&
		currentPlatformVersion != platformVersion &&
		(currentPlatformVersion == "" || wasPlatformVersionPending || !handshakeRequired) {
		platformconfig.SetPlatformRelease(&wb.Status.Releases, platformVersion)
		reconciledPlatformVersion = platformVersion
	}

	r.setReadyCondition(
		wb,
		deploymentsReady,
		deployMsg,
		phaseCtx.WasReady,
		desiredDistribution,
		platformVersion,
		reconciledPlatformVersion,
	)
	appendImageStreamWarningToReady(wb)

	wb.Status.ObservedGeneration = wb.Generation

	phaseCtx.Ready = meta.IsStatusConditionTrue(wb.Status.Conditions, conditionTypeReady)
	phaseCtx.Degraded = meta.IsStatusConditionTrue(wb.Status.Conditions, conditionTypeDegraded)
	phaseCtx.ProvisioningSucceeded = meta.IsStatusConditionTrue(wb.Status.Conditions, conditionTypeProvisioningSucceeded)
	wb.Status.Phase = statusutil.ComputePhase(phaseCtx)

	sanitizeConditions(wb.Status.Conditions)

	err = r.Status().Update(ctx, wb)
	if err != nil {
		l.Error(err, "failed to update Workbenches status")

		return ctrl.Result{RequeueAfter: requeueDelay}, err
	}

	l.Info("reconciliation complete", "phase", wb.Status.Phase)

	if !deploymentsReady ||
		!platformconfig.DistributionAligned(desiredDistribution, wb.Status.Distribution) ||
		(handshakeRequired && !platformconfig.HandshakeComplete(platformVersion, wb.Status.Releases)) {
		return ctrl.Result{RequeueAfter: requeueDelay}, nil
	}

	return ctrl.Result{}, nil
}

func (r *WorkbenchesReconciler) setDeploymentCondition(wb *componentsv1alpha1.Workbenches, ready bool, msg string) {
	if ready {
		meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDeploymentsAvailable,
			Status:             metav1.ConditionTrue,
			Reason:             conditionReasonAvailable,
			Message:            "All deployments are available",
			ObservedGeneration: wb.Generation,
		})
	} else {
		meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDeploymentsAvailable,
			Status:             metav1.ConditionFalse,
			Reason:             "Unavailable",
			Message:            msg,
			ObservedGeneration: wb.Generation,
		})
	}
}

func (r *WorkbenchesReconciler) setWorkbenchesV2Condition(ctx context.Context, wb *componentsv1alpha1.Workbenches) {
	if !wb.Spec.IsWorkbenchesV2Managed() {
		meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
			Type:               conditionTypeWorkbenchesV2Ready,
			Status:             metav1.ConditionFalse,
			Reason:             componentsv1alpha1.ManagementStateRemoved,
			Message:            "workbenches-v2 submodule is not enabled",
			ObservedGeneration: wb.Generation,
		})

		return
	}

	if !r.workbenchesV2ManifestsExist() {
		meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
			Type:               conditionTypeWorkbenchesV2Ready,
			Status:             metav1.ConditionFalse,
			Reason:             "ManifestsNotAvailable",
			Message:            "workbenches-v2 manifests are not available in this operator build",
			ObservedGeneration: wb.Generation,
		})

		return
	}

	ready, msg := r.checkWorkspacesControllerDeployment(ctx, wb)
	if !ready {
		meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
			Type:               conditionTypeWorkbenchesV2Ready,
			Status:             metav1.ConditionFalse,
			Reason:             "Unavailable",
			Message:            msg,
			ObservedGeneration: wb.Generation,
		})

		return
	}

	meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
		Type:               conditionTypeWorkbenchesV2Ready,
		Status:             metav1.ConditionTrue,
		Reason:             conditionReasonAvailable,
		Message:            "workspaces-controller deployment is available",
		ObservedGeneration: wb.Generation,
	})
}

// checkWorkspacesControllerDeployment reports whether the workspaces-controller
// Deployment (rendered from the workbenches-v2 gateway overlay) exists and has
// its desired replicas ready.
func (r *WorkbenchesReconciler) checkWorkspacesControllerDeployment(
	ctx context.Context,
	wb *componentsv1alpha1.Workbenches,
) (bool, string) {
	l := log.FromContext(ctx)
	nsName := r.resolveOperandNamespace(wb.Spec.Platform)

	deployments := &appsv1.DeploymentList{}

	err := r.List(ctx, deployments, client.InNamespace(nsName), client.MatchingLabels{
		metadata.ComponentLabelKey: metadata.LabelTrue,
	})
	if err != nil {
		l.V(1).Info("failed to list deployments for workspaces-controller", "error", err)

		return false, fmt.Sprintf("failed to list deployments: %v", err)
	}

	var v2Deployments []appsv1.Deployment

	for _, d := range deployments.Items {
		if d.Name == workspacesControllerDeploymentName {
			v2Deployments = append(v2Deployments, d)
		}
	}

	if len(v2Deployments) == 0 {
		return false, "workspaces-controller deployment not found"
	}

	return deploymentsAvailability(v2Deployments)
}

func (r *WorkbenchesReconciler) resolveDesiredDistribution(
	ctx context.Context,
	wb *componentsv1alpha1.Workbenches,
) (componentsv1alpha1.Distribution, error) {
	nsName := r.resolveOperandNamespace(wb.Spec.Platform)

	desired, err := platformconfig.ReadDesiredDistribution(ctx, r.Client, nsName)
	if err != nil {
		return componentsv1alpha1.Distribution{}, err
	}

	return platformconfig.ResolveDesiredDistribution(desired, wb.Spec.Platform, ""), nil
}

func (r *WorkbenchesReconciler) readPlatformVersion(
	ctx context.Context,
	wb *componentsv1alpha1.Workbenches,
) (string, error) {
	nsName := r.resolveOperandNamespace(wb.Spec.Platform)

	return platformconfig.ReadPlatformVersion(ctx, r.Client, nsName)
}

func (r *WorkbenchesReconciler) setReadyCondition(
	wb *componentsv1alpha1.Workbenches,
	deploymentsReady bool,
	deployMsg string,
	wasReady bool,
	desiredDistribution componentsv1alpha1.Distribution,
	platformVersion string,
	reconciledPlatformVersion string,
) {
	distributionAligned := platformconfig.DistributionAligned(desiredDistribution, wb.Status.Distribution)
	handshakeRequired := platformconfig.HandshakeRequired(desiredDistribution)
	handshakeComplete := !handshakeRequired || platformconfig.HandshakeComplete(platformVersion, wb.Status.Releases)
	ready := deploymentsReady && distributionAligned && handshakeComplete

	if ready {
		meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
			Type:               conditionTypeReady,
			Status:             metav1.ConditionTrue,
			Reason:             conditionReasonReconcileSuccess,
			Message:            "Workbenches component is ready",
			ObservedGeneration: wb.Generation,
		})

		meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDegraded,
			Status:             metav1.ConditionFalse,
			Reason:             "NotDegraded",
			Message:            "Workbenches component is operating normally",
			ObservedGeneration: wb.Generation,
		})

		return
	}

	readyReason := "DeploymentsNotReady"
	readyMsg := deployMsg
	if readyMsg == "" {
		readyMsg = "Waiting for deployments to become available"
	}

	if deploymentsReady && !distributionAligned {
		readyReason = "DistributionNotAligned"
		readyMsg = fmt.Sprintf(
			"Waiting for distribution alignment: desired %s/%s, current %s/%s",
			desiredDistribution.Name,
			desiredDistribution.Version,
			wb.Status.Distribution.Name,
			wb.Status.Distribution.Version,
		)
	} else if deploymentsReady && distributionAligned && handshakeRequired && !handshakeComplete {
		readyReason = "PlatformVersionPending"
		if platformVersion == "" {
			readyMsg = "waiting for platform version from odh-workbenches-config"
		} else {
			readyMsg = fmt.Sprintf(
				"platform upgrade in progress: reconciled %q, target %q",
				reconciledPlatformVersion,
				platformVersion,
			)
		}

		meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDegraded,
			Status:             metav1.ConditionFalse,
			Reason:             "NotDegraded",
			Message:            "Workbenches component is operating normally",
			ObservedGeneration: wb.Generation,
		})
	}

	meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             readyReason,
		Message:            readyMsg,
		ObservedGeneration: wb.Generation,
	})

	if !deploymentsReady && wasReady {
		meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
			Type:               conditionTypeDegraded,
			Status:             metav1.ConditionTrue,
			Reason:             "DeploymentsNotReady",
			Message:            deployMsg,
			ObservedGeneration: wb.Generation,
		})
	}
}

func (r *WorkbenchesReconciler) configureDependencies(ctx context.Context, wb *componentsv1alpha1.Workbenches) error {
	appsNS := r.resolveOperandNamespace(wb.Spec.Platform)
	if err := r.ensureGeneratedNamespace(ctx, appsNS, "applications"); err != nil {
		return fmt.Errorf("applications namespace: %w", err)
	}

	legacyNS := r.resolveLegacyWorkbenchNamespace(wb)
	if legacyNS != appsNS {
		if err := r.ensureGeneratedNamespace(ctx, legacyNS, "legacy workbench"); err != nil {
			return fmt.Errorf("legacy workbench namespace: %w", err)
		}
	}

	return nil
}

func (r *WorkbenchesReconciler) ensureGeneratedNamespace(
	ctx context.Context,
	nsName, purpose string,
) error {
	l := log.FromContext(ctx)

	ns := &corev1.Namespace{}

	err := r.Get(ctx, client.ObjectKey{Name: nsName}, ns)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get namespace %s: %w", nsName, err)
		}

		l.Info("creating namespace for workbenches", "namespace", nsName, "purpose", purpose)

		// Label only — do not set controller ownerReferences on Namespaces. Legacy
		// workbench namespaces may hold user notebooks that must survive CR deletion.
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: nsName,
				Labels: map[string]string{
					metadata.OwnedNamespaceLabel: metadata.LabelTrue,
				},
			},
		}

		if createErr := r.Create(ctx, ns); createErr != nil {
			return fmt.Errorf("failed to create namespace %s: %w", nsName, createErr)
		}

		return nil
	}

	if ns.Labels == nil {
		ns.Labels = map[string]string{}
	}

	if ns.Labels[metadata.OwnedNamespaceLabel] != metadata.LabelTrue {
		ns.Labels[metadata.OwnedNamespaceLabel] = metadata.LabelTrue

		if updateErr := r.Update(ctx, ns); updateErr != nil {
			return fmt.Errorf("failed to update namespace %s labels: %w", nsName, updateErr)
		}
	}

	return nil
}

func validateSpec(spec componentsv1alpha1.WorkbenchesSpec) error {
	if spec.Platform != "" && !platform.IsValid(spec.Platform) {
		return fmt.Errorf("unsupported platform %q, must be one of: %s, %s",
			spec.Platform, platform.OpenDataHub, platform.SelfManagedRhoai)
	}

	return nil
}

// setStatusNamespaces records the active operand namespace and echoes the
// legacy DSC/spec workbenchNamespace onto status for observability.
func (r *WorkbenchesReconciler) setStatusNamespaces(wb *componentsv1alpha1.Workbenches, appsNS string) {
	wb.Status.ApplicationsNamespace = appsNS
	wb.Status.WorkbenchNamespace = wb.Spec.WorkbenchNamespace
}

// resolveOperandNamespace returns the namespace where notebook-controller
// operands are deployed and cleaned up.
//
// Prefer APPLICATIONS_NAMESPACE when configured on the reconciler. Otherwise
// fall back by platform: opendatahub (ODH/default) or redhat-ods-applications
// (SelfManagedRhoai). Spec.WorkbenchNamespace is not used for operand deploy.
func (r *WorkbenchesReconciler) resolveOperandNamespace(platformType string) string {
	if ns := r.configuredApplicationsNamespace(); ns != "" {
		return ns
	}

	return platform.DefaultApplicationsNamespace(platformType)
}

// resolveLegacyWorkbenchNamespace returns the JupyterHub-era notebooks namespace
// ensured for legacy Notebook CR placement (dashboard / upgraded clusters).
// Uses spec.workbenchNamespace when set; otherwise platform defaults apply.
func (r *WorkbenchesReconciler) resolveLegacyWorkbenchNamespace(wb *componentsv1alpha1.Workbenches) string {
	if wb.Spec.WorkbenchNamespace != "" {
		return wb.Spec.WorkbenchNamespace
	}

	return platform.DefaultLegacyWorkbenchNamespace(wb.Spec.Platform)
}

func (r *WorkbenchesReconciler) computeKustomizeParams(wb *componentsv1alpha1.Workbenches) map[string]string {
	gatewayURL := ""
	if wb.Spec.GatewayDomain != "" {
		gatewayURL = wb.Spec.GatewayDomain
	}

	return map[string]string{
		paramSectionTitle:  platform.SectionTitle(wb.Spec.Platform),
		paramMLflowEnabled: strconv.FormatBool(wb.Spec.MLflowEnabled),
		paramGatewayURL:    gatewayURL,
	}
}

func (r *WorkbenchesReconciler) checkDeployments(ctx context.Context, wb *componentsv1alpha1.Workbenches) (bool, string) {
	l := log.FromContext(ctx)
	nsName := r.resolveOperandNamespace(wb.Spec.Platform)

	deployments := &appsv1.DeploymentList{}

	err := r.List(ctx, deployments, client.InNamespace(nsName), client.MatchingLabels{
		metadata.ComponentLabelKey: metadata.LabelTrue,
	})
	if err != nil {
		l.V(1).Info("failed to list deployments, treating as not ready", "error", err)

		return false, fmt.Sprintf("failed to list deployments: %v", err)
	}

	if len(deployments.Items) == 0 {
		return false, "no notebook controller deployments found"
	}

	return deploymentsAvailability(deployments.Items)
}

func (r *WorkbenchesReconciler) setErrorStatus(
	ctx context.Context,
	wb *componentsv1alpha1.Workbenches,
	reason string,
	reconcileErr error,
) (ctrl.Result, error) {
	meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
		Type:               conditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            reconcileErr.Error(),
		ObservedGeneration: wb.Generation,
	})

	wb.Status.Phase = statusutil.ComputePhase(statusutil.PhaseContext{Failed: true})
	wb.Status.ObservedGeneration = wb.Generation

	sanitizeConditions(wb.Status.Conditions)

	if err := r.Status().Update(ctx, wb); err != nil {
		log.FromContext(ctx).Error(err, "failed to update error status")
	}

	return ctrl.Result{}, reconcileErr
}

// sanitizeConditions ensures every condition has a non-empty Reason.
// Foreign conditions (set by other controllers or the platform orchestrator) may
// violate the Kubernetes validation rule that reason must be >= 1 character.
func sanitizeConditions(conditions []metav1.Condition) {
	for i := range conditions {
		if conditions[i].Reason == "" {
			conditions[i].Reason = conditionReasonUnknown
		}
	}
}

// deploymentsAvailability reports whether all component deployments have the desired
// number of ready replicas. A deployment scaled to zero is treated as unavailable.
func deploymentsAvailability(deployments []appsv1.Deployment) (bool, string) {
	if len(deployments) == 0 {
		return false, "no notebook controller deployments found"
	}

	for i := range deployments {
		d := &deployments[i]
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}

		if desired < 1 {
			return false, fmt.Sprintf("deployment %s/%s is scaled to zero", d.Namespace, d.Name)
		}

		if d.Status.ReadyReplicas < desired {
			return false, fmt.Sprintf("deployment %s/%s has %d/%d ready replicas",
				d.Namespace, d.Name, d.Status.ReadyReplicas, desired)
		}
	}

	return true, ""
}
