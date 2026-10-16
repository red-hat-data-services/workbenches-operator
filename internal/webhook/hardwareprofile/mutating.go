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

// Package hardwareprofile implements the mutating webhook for HardwareProfile injection into Notebooks.
// This is a faithful port of the opendatahub-operator webhook, scoped to Notebooks only
// (InferenceService/LLMInferenceService are managed by the KServe component).
// The HardwareProfile CR is accessed as unstructured to avoid importing the opendatahub-operator API.
package hardwareprofile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/opendatahub-io/workbenches-operator/internal/gvk"
	"github.com/opendatahub-io/workbenches-operator/internal/metadata"
)

const (
	specField = "spec"

	kueueQueueNameLabel = "kueue.x-k8s.io/queue-name"

	// draClaimName is the deterministic name used for the pod-level ResourceClaim
	// entry managed by the hardware profile webhook. Using a constant name makes
	// it easy to identify and clean up webhook-managed claims.
	draClaimName = "hwp-dra"
)

var (
	notebookContainersPath     = []string{specField, "template", specField, "containers"}
	notebookNodeSelectorPath   = []string{specField, "template", specField, "nodeSelector"}
	notebookTolerationsPath    = []string{specField, "template", specField, "tolerations"}
	notebookResourceClaimsPath = []string{specField, "template", specField, "resourceClaims"}

	errNoMatchingContainer = errors.New("no matching main container found")
)

//+kubebuilder:rbac:groups=infrastructure.opendatahub.io,resources=hardwareprofiles,verbs=get
//+kubebuilder:rbac:groups=resource.k8s.io,resources=resourceclaimtemplates,verbs=get
//+kubebuilder:rbac:groups="",resources=events,verbs=create
//+kubebuilder:webhook:path=/workbenches-hardware-profile,mutating=true,failurePolicy=fail,timeoutSeconds=5,groups=kubeflow.org,resources=notebooks,verbs=create;update,versions=v1,name=hardwareprofile-notebook-injector.opendatahub.io,sideEffects=NoneOnDryRun,admissionReviewVersions=v1

// Injector implements a mutating admission webhook for hardware profile injection
// into Notebook resources.
type Injector struct {
	Client      client.Reader
	EventWriter client.Writer
	Decoder     admission.Decoder
	Name        string
}

var _ admission.Handler = &Injector{}

// SetupWithManager registers the webhook with the manager.
func (i *Injector) SetupWithManager(mgr ctrl.Manager) error {
	hookServer := mgr.GetWebhookServer()
	hookServer.Register("/workbenches-hardware-profile", &webhook.Admission{
		Handler: i,
	})

	return nil
}

// Handle processes admission requests for Notebook create/update operations.
func (i *Injector) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx)

	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return admission.Allowed(fmt.Sprintf("Operation %s on %s allowed", req.Operation, req.Kind.Kind))
	}

	if i.Decoder == nil {
		log.Error(nil, "Decoder is nil - webhook not properly initialized")

		return admission.Errored(http.StatusInternalServerError, errors.New("webhook decoder not initialized"))
	}

	notebook := &unstructured.Unstructured{}
	if err := i.Decoder.Decode(req, notebook); err != nil {
		log.Error(err, "failed to decode object")

		return admission.Errored(http.StatusBadRequest, fmt.Errorf("failed to decode object: %w", err))
	}

	if !notebook.GetDeletionTimestamp().IsZero() {
		return admission.Allowed("Object marked for deletion, skipping hardware profile injection")
	}

	return i.performHardwareProfileInjection(ctx, &req, notebook)
}

func (i *Injector) performHardwareProfileInjection(
	ctx context.Context,
	req *admission.Request,
	obj *unstructured.Unstructured,
) admission.Response {
	log := logf.FromContext(ctx)

	profileName := getAnnotation(obj, metadata.HardwareProfileNameAnnotation)
	if profileName == "" {
		if req.Operation == admissionv1.Update {
			if resp := i.handleHWPRemoval(ctx, req, obj); resp != nil {
				return *resp
			}
		}

		return admission.Allowed("No hardware profile annotation found")
	}

	profileNamespace := getAnnotation(obj, metadata.HardwareProfileNamespaceAnnotation)
	if profileNamespace == "" {
		profileNamespace = obj.GetNamespace()
	}

	if profileNamespace == "" {
		return admission.Errored(http.StatusBadRequest, errors.New("unable to determine hardware profile namespace"))
	}

	hwp, err := i.fetchHardwareProfile(ctx, profileNamespace, profileName)
	if err != nil {
		return i.handleFetchError(ctx, req, err, profileName, profileNamespace)
	}

	if getAnnotation(obj, metadata.HardwareProfileNamespaceAnnotation) == "" {
		setAnnotation(obj, metadata.HardwareProfileNamespaceAnnotation, profileNamespace)
	}

	if validateErr := i.validateContainerNames(obj); validateErr != nil {
		expectedName := obj.GetName()
		isDryRun := req.DryRun != nil && *req.DryRun

		var warningMsg string
		if isContainerNameMismatch(validateErr) {
			warningMsg = fmt.Sprintf("Hardware profile '%s' was not applied: %s. "+
				"To use this hardware profile, rename your container to '%s'. "+
				"All hardware profile settings (identifiers, scheduling, etc.) are skipped.",
				profileName, validateErr.Error(), expectedName)
			i.maybeEmitContainerNameWarningEvent(ctx, isDryRun, obj, validateErr, profileName, expectedName)
		} else {
			warningMsg = fmt.Sprintf("Hardware profile '%s' was not applied due to validation error: %s. "+
				"All hardware profile settings are skipped. Please check your workload structure.",
				profileName, validateErr.Error())
			i.maybeEmitValidationErrorEvent(ctx, isDryRun, obj, validateErr, profileName)
		}

		log.Info("skipping all hardware profile application due to validation failure",
			"workload", obj.GetName(), "namespace", obj.GetNamespace(),
			"hardwareProfile", profileName, "error", validateErr)

		marshaledObj, marshalErr := json.Marshal(obj)
		if marshalErr != nil {
			log.Error(marshalErr, "Failed to marshal object after container validation, admitting anyway")
			resp := admission.Allowed("Admitted but marshal failed after validation")
			resp.Warnings = []string{warningMsg, "Internal error: failed to marshal object"}

			return resp
		}

		resp := admission.PatchResponseFromRaw(req.Object.Raw, marshaledObj)
		resp.Warnings = []string{warningMsg}

		return resp
	}

	isCreate := req.Operation == admissionv1.Create
	profileChanged := i.detectProfileChange(req, profileName, profileNamespace)
	profileNewlyAdded := i.isProfileNewlyAdded(req)

	if profileChanged {
		log.V(1).Info("hardware profile changed, will clear existing scheduling settings",
			"workload", obj.GetName(), "newProfile", profileName, "newNamespace", profileNamespace)
	}

	warnings, err := i.applyHardwareProfileToNotebook(ctx, obj, hwp, profileChanged, isCreate || profileNewlyAdded)
	if err != nil {
		log.Error(err, "Failed to apply hardware profile", "profile", profileName)

		return admission.Errored(http.StatusInternalServerError, err)
	}

	marshaledObj, err := json.Marshal(obj)
	if err != nil {
		log.Error(err, "Failed to marshal modified object")

		return admission.Errored(http.StatusInternalServerError, err)
	}

	resp := admission.PatchResponseFromRaw(req.Object.Raw, marshaledObj)
	if len(warnings) > 0 {
		resp.Warnings = warnings
		log.V(1).Info("admission response includes warnings", "warnings", warnings)
	}

	return resp
}

func (i *Injector) handleFetchError(
	ctx context.Context,
	req *admission.Request,
	err error,
	profileName, profileNamespace string,
) admission.Response {
	log := logf.FromContext(ctx)

	if k8serr.IsNotFound(err) {
		log.V(1).Info("Hardware profile not found", "profile", profileName, "namespace", profileNamespace, "request", req.Name)

		if req.Operation == admissionv1.Update && i.isStaleProfileReference(req, profileName) {
			return admission.Allowed(
				fmt.Sprintf("hardware profile '%s' not found in namespace '%s'; allowing update with warning",
					profileName, profileNamespace)).
				WithWarnings(
					fmt.Sprintf("Referenced hardware profile '%s' in namespace '%s' no longer exists. "+
						"Profile settings were not applied. Remove or update the annotation to clear this warning.",
						profileName, profileNamespace))
		}

		return admission.Errored(http.StatusBadRequest,
			fmt.Errorf("hardware profile '%s' not found in namespace '%s'", profileName, profileNamespace))
	}

	log.Error(err, "Failed to get hardware profile", "profile", profileName, "namespace", profileNamespace)

	if req.Operation == admissionv1.Update && i.isStaleProfileReference(req, profileName) {
		return admission.Allowed(
			fmt.Sprintf("failed to fetch hardware profile '%s'; allowing update with warning",
				profileName)).
			WithWarnings(
				fmt.Sprintf("Failed to fetch hardware profile '%s' from namespace '%s': %v. "+
					"Profile settings were not applied.", profileName, profileNamespace, err))
	}

	return admission.Errored(http.StatusInternalServerError,
		fmt.Errorf("failed to get hardware profile '%s' from namespace '%s': %w", profileName, profileNamespace, err))
}

// isProfileNewlyAdded returns true when an update adds a hardware profile
// annotation to a Notebook that previously had none. This is distinct from
// detectProfileChange (which compares two non-empty profile names) and from
// isCreate. Used to gate first-time DRA application on existing notebooks.
func (i *Injector) isProfileNewlyAdded(req *admission.Request) bool {
	if req.Operation == admissionv1.Create || req.OldObject.Raw == nil {
		return false
	}

	oldObj := &unstructured.Unstructured{Object: map[string]any{}}
	if err := json.Unmarshal(req.OldObject.Raw, oldObj); err != nil {
		return false
	}

	return getAnnotation(oldObj, metadata.HardwareProfileNameAnnotation) == ""
}

func (i *Injector) detectProfileChange(req *admission.Request, newProfileName, newProfileNamespace string) bool {
	if req.Operation == admissionv1.Create {
		return false
	}

	if req.OldObject.Raw == nil {
		return true
	}

	oldObj := &unstructured.Unstructured{}
	if err := json.Unmarshal(req.OldObject.Raw, oldObj); err != nil {
		return true
	}

	oldProfileName := getAnnotation(oldObj, metadata.HardwareProfileNameAnnotation)
	if oldProfileName == "" {
		return false
	}

	oldProfileNamespace := getAnnotation(oldObj, metadata.HardwareProfileNamespaceAnnotation)
	if oldProfileNamespace == "" {
		oldProfileNamespace = oldObj.GetNamespace()
	}

	return oldProfileName != newProfileName || oldProfileNamespace != newProfileNamespace
}

func (i *Injector) validateContainerNames(obj *unstructured.Unstructured) error {
	containers, found, err := unstructured.NestedSlice(obj.Object, notebookContainersPath...)
	if err != nil {
		return fmt.Errorf("failed to access containers: %w", err)
	}

	if !found || len(containers) == 0 {
		return nil
	}

	if len(containers) == 1 {
		return nil
	}

	expectedName := obj.GetName()
	for _, c := range containers {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}

		name, _ := m["name"].(string)
		if name == expectedName {
			return nil
		}
	}

	return fmt.Errorf("%w in Notebook '%s/%s': expected container name '%s'",
		errNoMatchingContainer, obj.GetNamespace(), obj.GetName(), expectedName)
}

func isContainerNameMismatch(err error) bool {
	return errors.Is(err, errNoMatchingContainer)
}

// maybeEmitContainerNameWarningEvent creates a Warning event on the workload to indicate
// that hardware profile settings were not applied due to container name mismatch.
// Skipped during dry-run requests. Non-blocking — failures are logged but never fail admission.
func (i *Injector) maybeEmitContainerNameWarningEvent(
	ctx context.Context, isDryRun bool, obj *unstructured.Unstructured,
	validationErr error, hwpName, expectedName string,
) {
	if isDryRun {
		return
	}

	log := logf.FromContext(ctx)

	if i.EventWriter == nil {
		log.V(1).Info("no event writer configured, skipping event creation")

		return
	}

	eventMessage := fmt.Sprintf("Hardware profile '%s' settings not applied to this workload: %s. "+
		"Rename container to '%s' to enable hardware profile injection.",
		hwpName, validationErr.Error(), expectedName)

	now := metav1.NewTime(time.Now())
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: obj.GetName() + ".hwp-",
			Namespace:    obj.GetNamespace(),
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       obj.GetKind(),
			Namespace:  obj.GetNamespace(),
			Name:       obj.GetName(),
			UID:        obj.GetUID(),
			APIVersion: obj.GetAPIVersion(),
		},
		Reason:         "ContainerNameMismatch",
		Message:        eventMessage,
		Source:         corev1.EventSource{Component: "hardwareprofile-webhook"},
		Type:           corev1.EventTypeWarning,
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}

	if createErr := i.EventWriter.Create(ctx, event); createErr != nil {
		log.V(1).Info("failed to create warning event for container name mismatch (non-blocking)",
			"error", createErr, "workload", obj.GetName())
	}
}

// maybeEmitValidationErrorEvent creates a Warning event on the workload to indicate
// that hardware profile settings were not applied due to a structural validation error.
// Skipped during dry-run requests. Non-blocking — failures are logged but never fail admission.
func (i *Injector) maybeEmitValidationErrorEvent(
	ctx context.Context, isDryRun bool, obj *unstructured.Unstructured,
	validationErr error, hwpName string,
) {
	if isDryRun {
		return
	}

	log := logf.FromContext(ctx)

	if i.EventWriter == nil {
		log.V(1).Info("no event writer configured, skipping event creation")

		return
	}

	eventMessage := fmt.Sprintf("Hardware profile '%s' was not applied due to validation error: %v. "+
		"Check your workload structure.",
		hwpName, validationErr)

	now := metav1.NewTime(time.Now())
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: obj.GetName() + ".hwp-",
			Namespace:    obj.GetNamespace(),
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       obj.GetKind(),
			Namespace:  obj.GetNamespace(),
			Name:       obj.GetName(),
			UID:        obj.GetUID(),
			APIVersion: obj.GetAPIVersion(),
		},
		Reason:         "ValidationError",
		Message:        eventMessage,
		Source:         corev1.EventSource{Component: "hardwareprofile-webhook"},
		Type:           corev1.EventTypeWarning,
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}

	if createErr := i.EventWriter.Create(ctx, event); createErr != nil {
		log.V(1).Info("failed to create warning event for validation error (non-blocking)",
			"error", createErr, "workload", obj.GetName())
	}
}

func (i *Injector) handleHWPRemoval(
	ctx context.Context,
	req *admission.Request,
	obj *unstructured.Unstructured,
) *admission.Response {
	log := logf.FromContext(ctx)

	if req.OldObject.Raw == nil {
		return nil
	}

	oldObj := &unstructured.Unstructured{}
	if err := json.Unmarshal(req.OldObject.Raw, oldObj); err != nil {
		return nil
	}

	oldProfileName := getAnnotation(oldObj, metadata.HardwareProfileNameAnnotation)
	if oldProfileName == "" {
		return nil
	}

	log.V(1).Info("HWP annotation removed, cleaning up HWP-applied settings",
		"workload", obj.GetName(), "oldProfile", oldProfileName)

	oldProfileNamespace := getAnnotation(oldObj, metadata.HardwareProfileNamespaceAnnotation)
	if oldProfileNamespace == "" {
		oldProfileNamespace = oldObj.GetNamespace()
	}

	oldHWP, err := i.fetchHardwareProfile(ctx, oldProfileNamespace, oldProfileName)
	if err != nil {
		log.V(1).Info("Could not fetch old HWP for cleanup, HWP-applied settings may remain",
			"error", err, "oldProfile", oldProfileName, "oldNamespace", oldProfileNamespace)

		// DRA cleanup does not need the old HWP — the claim name is the
		// constant draClaimName. Clean it up now; the name annotation is
		// already gone so future updates will never re-enter handleHWPRemoval.
		removeDRAFromNotebook(obj)

		removeAnnotation(obj, metadata.HardwareProfileNamespaceAnnotation)

		marshaledObj, marshalErr := json.Marshal(obj)
		if marshalErr != nil {
			return nil
		}

		resp := admission.PatchResponseFromRaw(req.Object.Raw, marshaledObj)

		return &resp
	}

	if removeErr := i.removeHWPSettings(obj, oldHWP); removeErr != nil {
		log.Error(removeErr, "Failed to remove HWP settings")
		resp := admission.Errored(http.StatusInternalServerError, removeErr)

		return &resp
	}

	removeAnnotation(obj, metadata.HardwareProfileNamespaceAnnotation)

	marshaledObj, err := json.Marshal(obj)
	if err != nil {
		log.Error(err, "Failed to marshal modified object")
		resp := admission.Errored(http.StatusInternalServerError, err)

		return &resp
	}

	resp := admission.PatchResponseFromRaw(req.Object.Raw, marshaledObj)

	return &resp
}

func (i *Injector) removeHWPSettings(obj, hwp *unstructured.Unstructured) error {
	// Remove Kueue label if the old HWP had Kueue scheduling.
	// Errors from NestedString indicate a malformed field (non-string value) — safe to skip.
	kueueQueueName, _, _ := unstructured.NestedString(hwp.Object, "spec", "scheduling", "kueue", "localQueueName")
	if kueueQueueName != "" {
		removeLabel(obj, kueueQueueNameLabel)
	}

	nodeSelector, nsFound, _ := unstructured.NestedStringMap(hwp.Object, "spec", "scheduling", "node", "nodeSelector")
	if nsFound && len(nodeSelector) > 0 {
		if err := removeHWPNodeSelector(obj, notebookNodeSelectorPath, nodeSelector); err != nil {
			return fmt.Errorf("failed to remove HWP nodeSelector: %w", err)
		}
	}

	tolerations, tolFound, _ := unstructured.NestedSlice(hwp.Object, "spec", "scheduling", "node", "tolerations")
	if tolFound && len(tolerations) > 0 {
		if err := removeHWPTolerations(obj, notebookTolerationsPath, tolerations); err != nil {
			return fmt.Errorf("failed to remove HWP tolerations: %w", err)
		}
	}

	// Remove webhook-managed DRA resourceClaims unconditionally. The fetched
	// HWP may have been edited to remove DRA before the annotation was removed
	// from the Notebook, so we cannot gate cleanup on the current HWP state.
	// removeDRAFromNotebook is a no-op when no managed claims are present.
	removeDRAFromNotebook(obj)

	return nil
}

func removeHWPNodeSelector(obj *unstructured.Unstructured, nodeSelectorPath []string, hwpNodeSelector map[string]string) error {
	existingNodeSelector, found, err := unstructured.NestedStringMap(obj.Object, nodeSelectorPath...)
	if err != nil {
		return err
	}

	if !found || len(existingNodeSelector) == 0 {
		return nil
	}

	for key, value := range hwpNodeSelector {
		if existingValue, exists := existingNodeSelector[key]; exists && existingValue == value {
			delete(existingNodeSelector, key)
		}
	}

	if len(existingNodeSelector) == 0 {
		unstructured.RemoveNestedField(obj.Object, nodeSelectorPath...)
	} else {
		if err := unstructured.SetNestedStringMap(obj.Object, existingNodeSelector, nodeSelectorPath...); err != nil {
			return err
		}
	}

	return nil
}

func removeHWPTolerations(obj *unstructured.Unstructured, tolerationsPath []string, hwpTolerations []any) error {
	existingTolerations, found, err := unstructured.NestedSlice(obj.Object, tolerationsPath...)
	if err != nil {
		return err
	}

	if !found || len(existingTolerations) == 0 {
		return nil
	}

	hwpTolKeys := make(map[string]bool)
	for _, tol := range hwpTolerations {
		if tolMap, ok := tol.(map[string]any); ok {
			hwpTolKeys[TolerationKey(tolMap)] = true
		}
	}

	remaining := make([]any, 0, len(existingTolerations))
	for _, existing := range existingTolerations {
		if existingMap, ok := existing.(map[string]any); ok {
			if !hwpTolKeys[TolerationKey(existingMap)] {
				remaining = append(remaining, existing)
			}
		}
	}

	if len(remaining) == 0 {
		unstructured.RemoveNestedField(obj.Object, tolerationsPath...)
	} else {
		if err := unstructured.SetNestedSlice(obj.Object, remaining, tolerationsPath...); err != nil {
			return err
		}
	}

	return nil
}

func (i *Injector) fetchHardwareProfile(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	hwp := &unstructured.Unstructured{}
	hwp.SetGroupVersionKind(gvk.HardwareProfile)

	if err := i.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, hwp); err != nil {
		return nil, err
	}

	return hwp, nil
}

func (i *Injector) fetchResourceClaimTemplate(ctx context.Context, namespace, name string) error {
	rct := &unstructured.Unstructured{Object: map[string]any{}}
	rct.SetGroupVersionKind(gvk.ResourceClaimTemplate)

	return i.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, rct)
}

// applyDRAIfConfigured reads the DRA template name from the HardwareProfile
// and applies it to the Notebook if present and valid.
func (i *Injector) applyDRAIfConfigured(
	ctx context.Context,
	notebook, hwp *unstructured.Unstructured,
) error {
	draTemplateName, draFound, draErr := unstructured.NestedString(hwp.Object, "spec", "dra", "resourceClaimTemplateName")
	if draErr != nil {
		return fmt.Errorf("failed to read spec.dra.resourceClaimTemplateName: %w", draErr)
	}

	if !draFound || draTemplateName == "" {
		// The HardwareProfile has no DRA configured. Per ADR-0015, editing an
		// already-referenced profile has no retroactive effect on already-admitted
		// workloads — the change only takes effect on the next workload created
		// against the profile. So we do not clean up existing DRA claims here.
		return nil
	}

	if errs := validation.IsDNS1123Subdomain(draTemplateName); len(errs) > 0 {
		return fmt.Errorf("invalid resourceClaimTemplateName %q in HardwareProfile '%s': %s",
			draTemplateName, hwp.GetName(), errs[0])
	}

	if err := i.applyDRAToNotebook(ctx, notebook, draTemplateName); err != nil {
		return fmt.Errorf("failed to apply DRA configuration: %w", err)
	}

	return nil
}

// applyDRAToNotebook configures Dynamic Resource Allocation on the Notebook by:
//  1. Validating the referenced ResourceClaimTemplate exists in the Notebook's namespace.
//  2. Adding a pod-level resourceClaim entry referencing the template.
//  3. Adding a container-level resources.claims entry on the main container.
//
// Existing user-managed pod-level resourceClaims are always preserved; only
// the webhook-managed claim (identified by draClaimName) is replaced.
func (i *Injector) applyDRAToNotebook(
	ctx context.Context,
	notebook *unstructured.Unstructured,
	templateName string,
) error {
	log := logf.FromContext(ctx)

	if err := i.fetchResourceClaimTemplate(ctx, notebook.GetNamespace(), templateName); err != nil {
		if k8serr.IsNotFound(err) {
			return fmt.Errorf("ResourceClaimTemplate '%s' not found in namespace '%s'",
				templateName, notebook.GetNamespace())
		}

		return fmt.Errorf("failed to get ResourceClaimTemplate '%s' from namespace '%s': %w",
			templateName, notebook.GetNamespace(), err)
	}

	// Build the pod-level resourceClaim entry.
	claim := map[string]any{
		"name":                      draClaimName,
		"resourceClaimTemplateName": templateName,
	}

	// Always preserve existing user-managed claims; only replace the webhook-managed one.
	var resourceClaims []any

	existingClaims, _, _ := unstructured.NestedSlice(notebook.Object, notebookResourceClaimsPath...)
	for _, existing := range existingClaims {
		if m, ok := existing.(map[string]any); ok {
			if name, _ := m["name"].(string); name == draClaimName {
				continue
			}
		}

		resourceClaims = append(resourceClaims, existing)
	}

	resourceClaims = append(resourceClaims, claim)

	if err := unstructured.SetNestedSlice(notebook.Object, resourceClaims, notebookResourceClaimsPath...); err != nil {
		return fmt.Errorf("failed to set resourceClaims: %w", err)
	}

	// Add container-level resources.claims on the main container.
	if err := setContainerResourceClaims(notebook, draClaimName); err != nil {
		return fmt.Errorf("failed to set container resource claims: %w", err)
	}

	log.V(1).Info("applied DRA configuration",
		"workload", notebook.GetName(), "resourceClaimTemplate", templateName,
		"claimName", draClaimName)

	return nil
}

// removeDRAFromNotebook removes the webhook-managed DRA resourceClaim from the
// pod-level resourceClaims and the container-level resources.claims.
func removeDRAFromNotebook(obj *unstructured.Unstructured) {
	removePodResourceClaim(obj, draClaimName)
	removeContainerResourceClaims(obj, draClaimName)
}

// removePodResourceClaim removes the named claim from the pod-level
// resourceClaims list, preserving all other entries. If the list becomes
// empty the field is removed entirely.
func removePodResourceClaim(obj *unstructured.Unstructured, claimName string) {
	existingClaims, found, _ := unstructured.NestedSlice(obj.Object, notebookResourceClaimsPath...)
	if !found || len(existingClaims) == 0 {
		return
	}

	remaining := filterOutClaim(existingClaims, claimName)

	if len(remaining) == 0 {
		unstructured.RemoveNestedField(obj.Object, notebookResourceClaimsPath...)
	} else {
		_ = unstructured.SetNestedSlice(obj.Object, remaining, notebookResourceClaimsPath...)
	}
}

// preserveUserResourceClaims extracts user-managed resources.claims entries
// (those not managed by the webhook) from a container map. Used to save them
// before delete(container, "resources") on a profile change.
func preserveUserResourceClaims(container map[string]any, webhookClaimName string) []any {
	resourcesRaw, ok := container["resources"]
	if !ok {
		return nil
	}

	resourcesMap, ok := resourcesRaw.(map[string]any)
	if !ok {
		return nil
	}

	existingClaims, ok := resourcesMap["claims"].([]any)
	if !ok {
		return nil
	}

	return filterOutClaim(existingClaims, webhookClaimName)
}

// filterOutClaim returns a copy of claims with the named entry removed.
func filterOutClaim(claims []any, claimName string) []any {
	remaining := make([]any, 0, len(claims))

	for _, claim := range claims {
		if m, ok := claim.(map[string]any); ok {
			if name, _ := m["name"].(string); name == claimName {
				continue
			}
		}

		remaining = append(remaining, claim)
	}

	return remaining
}

// setContainerResourceClaims adds a resources.claims entry to the main container
// referencing the given pod-level claim name.
func setContainerResourceClaims(notebook *unstructured.Unstructured, claimName string) error {
	containers, found, err := unstructured.NestedSlice(notebook.Object, notebookContainersPath...)
	if err != nil {
		return err
	}

	if !found || len(containers) == 0 {
		return nil
	}

	mainIdx := notebookMainContainerIndex(containers, notebook.GetName())
	if mainIdx < 0 {
		return nil
	}

	container, ok := containers[mainIdx].(map[string]any)
	if !ok {
		return errors.New("container is not a map[string]any")
	}

	resourcesMap, err := getOrCreateNestedMap(container, "resources")
	if err != nil {
		return err
	}

	claimRef := map[string]any{"name": claimName}

	// Replace or append the claim in existing claims list.
	var claims []any
	if existingClaims, ok := resourcesMap["claims"].([]any); ok {
		for _, c := range existingClaims {
			if m, ok := c.(map[string]any); ok {
				if name, _ := m["name"].(string); name == claimName {
					continue
				}
			}

			claims = append(claims, c)
		}
	}

	claims = append(claims, claimRef)
	resourcesMap["claims"] = claims
	container["resources"] = resourcesMap
	containers[mainIdx] = container

	return unstructured.SetNestedSlice(notebook.Object, containers, notebookContainersPath...)
}

// removeContainerResourceClaims removes the named claim from the main container's
// resources.claims list.
func removeContainerResourceClaims(obj *unstructured.Unstructured, claimName string) {
	containers, found, _ := unstructured.NestedSlice(obj.Object, notebookContainersPath...)
	if !found || len(containers) == 0 {
		return
	}

	mainIdx := notebookMainContainerIndex(containers, obj.GetName())
	if mainIdx < 0 {
		return
	}

	container, ok := containers[mainIdx].(map[string]any)
	if !ok {
		return
	}

	resourcesRaw, hasResources := container["resources"]
	if !hasResources {
		return
	}

	resourcesMap, ok := resourcesRaw.(map[string]any)
	if !ok {
		return
	}

	existingClaims, ok := resourcesMap["claims"].([]any)
	if !ok || len(existingClaims) == 0 {
		return
	}

	remaining := make([]any, 0, len(existingClaims))
	for _, c := range existingClaims {
		if m, ok := c.(map[string]any); ok {
			if name, _ := m["name"].(string); name == claimName {
				continue
			}
		}

		remaining = append(remaining, c)
	}

	if len(remaining) == 0 {
		delete(resourcesMap, "claims")
	} else {
		resourcesMap["claims"] = remaining
	}

	container["resources"] = resourcesMap
	containers[mainIdx] = container
	_ = unstructured.SetNestedSlice(obj.Object, containers, notebookContainersPath...)
}

// isStaleProfileReference checks whether the old object already referenced the same
// hardware profile. If true, the profile was deleted after being applied — the user
// shouldn't be blocked from editing their notebook because of a stale reference.
// If false, the user is actively switching to a non-existent profile, which should be denied.
func (i *Injector) isStaleProfileReference(req *admission.Request, currentProfileName string) bool {
	if req.OldObject.Raw == nil {
		return false
	}

	oldObj := &unstructured.Unstructured{}
	if err := json.Unmarshal(req.OldObject.Raw, oldObj); err != nil {
		return false
	}

	oldProfileName := getAnnotation(oldObj, metadata.HardwareProfileNameAnnotation)

	return oldProfileName == currentProfileName
}

func (i *Injector) applyHardwareProfileToNotebook(
	ctx context.Context,
	notebook, hwp *unstructured.Unstructured,
	profileChanged, isNewProfile bool,
) ([]string, error) {
	log := logf.FromContext(ctx)

	var warnings []string

	if profileChanged {
		log.V(1).Info("clearing existing scheduling settings due to profile change",
			"workload", notebook.GetName(), "hardwareProfile", hwp.GetName())

		removeLabel(notebook, kueueQueueNameLabel)
		unstructured.RemoveNestedField(notebook.Object, notebookNodeSelectorPath...)
		unstructured.RemoveNestedField(notebook.Object, notebookTolerationsPath...)
		removeDRAFromNotebook(notebook)
	}

	identifiers, idFound, _ := unstructured.NestedSlice(hwp.Object, "spec", "identifiers")
	if idFound && len(identifiers) > 0 {
		if err := applyResourcesToNotebookContainer(ctx, identifiers, notebook, profileChanged); err != nil {
			return nil, fmt.Errorf("failed to apply resource requirements: %w", err)
		}
	}

	// Apply DRA when the profile is being applied for the first time (create
	// or profile newly added to an existing notebook) or when the selected
	// profile changes. On same-profile updates, existing claims are left
	// as-is: the webhook does not rewrite resourceClaimTemplateName or
	// re-validate the template's existence, matching the ADR-0015 principle
	// that edits to an already-referenced profile have no retroactive effect
	// on already-admitted workloads.
	// DRA is applied before scheduling — it is additive and works alongside
	// both Kueue and node scheduling, so it must not be skipped by the Kueue
	// early return.
	if isNewProfile || profileChanged {
		if err := i.applyDRAIfConfigured(ctx, notebook, hwp); err != nil {
			return nil, err
		}
	}

	// Apply Kueue LocalQueue label if spec.scheduling.kueue.localQueueName is set.
	// When Kueue scheduling is active, node scheduling (nodeSelector/tolerations) is skipped
	// because Kueue handles pod placement via its queue configuration.
	kueueQueueName, kueueFound, kueueErr := unstructured.NestedString(hwp.Object, "spec", "scheduling", "kueue", "localQueueName")
	if kueueErr != nil {
		return nil, fmt.Errorf("failed to read spec.scheduling.kueue.localQueueName: %w", kueueErr)
	}

	if kueueFound && kueueQueueName != "" {
		if errs := validation.IsValidLabelValue(kueueQueueName); len(errs) > 0 {
			return nil, fmt.Errorf("invalid localQueueName %q in HardwareProfile '%s': %s",
				kueueQueueName, hwp.GetName(), errs[0])
		}

		if !profileChanged {
			existingValue := getLabel(notebook, kueueQueueNameLabel)
			if existingValue != "" && existingValue != kueueQueueName {
				warnings = append(warnings, fmt.Sprintf(
					"label '%s' has value '%s' which will be overwritten by HardwareProfile '%s' which has value '%s'",
					kueueQueueNameLabel, existingValue, hwp.GetName(), kueueQueueName))
			}
		}

		setLabel(notebook, kueueQueueNameLabel, kueueQueueName)

		return warnings, nil
	}

	nodeSelector, nsFound, _ := unstructured.NestedStringMap(hwp.Object, "spec", "scheduling", "node", "nodeSelector")
	if nsFound && len(nodeSelector) > 0 {
		nsWarnings, err := applyNodeSelector(notebook, nodeSelector, profileChanged, hwp.GetName())
		if err != nil {
			return nil, err
		}

		warnings = append(warnings, nsWarnings...)
	}

	tolerations, tolFound, _ := unstructured.NestedSlice(hwp.Object, "spec", "scheduling", "node", "tolerations")
	if tolFound && len(tolerations) > 0 {
		if err := applyTolerations(notebook, tolerations, profileChanged); err != nil {
			return nil, err
		}
	}

	return warnings, nil
}

func applyNodeSelector(notebook *unstructured.Unstructured, nodeSelector map[string]string, profileChanged bool, hwpName string) ([]string, error) {
	if profileChanged {
		if err := unstructured.SetNestedStringMap(notebook.Object, nodeSelector, notebookNodeSelectorPath...); err != nil {
			return nil, fmt.Errorf("failed to set nodeSelector: %w", err)
		}

		return nil, nil
	}

	mergedNS, nsWarnings, err := mergeNodeSelector(notebook, notebookNodeSelectorPath, nodeSelector, hwpName)
	if err != nil {
		return nil, fmt.Errorf("failed to merge nodeSelector: %w", err)
	}

	if err := unstructured.SetNestedStringMap(notebook.Object, mergedNS, notebookNodeSelectorPath...); err != nil {
		return nil, fmt.Errorf("failed to set merged nodeSelector: %w", err)
	}

	return nsWarnings, nil
}

func applyTolerations(notebook *unstructured.Unstructured, tolerations []any, profileChanged bool) error {
	if profileChanged {
		if err := unstructured.SetNestedSlice(notebook.Object, tolerations, notebookTolerationsPath...); err != nil {
			return fmt.Errorf("failed to set tolerations: %w", err)
		}

		return nil
	}

	mergedTol, err := mergeTolerations(notebook, notebookTolerationsPath, tolerations)
	if err != nil {
		return fmt.Errorf("failed to merge tolerations: %w", err)
	}

	if err := unstructured.SetNestedSlice(notebook.Object, mergedTol, notebookTolerationsPath...); err != nil {
		return fmt.Errorf("failed to set merged tolerations: %w", err)
	}

	return nil
}

func applyResourcesToNotebookContainer(ctx context.Context, identifiers []any, notebook *unstructured.Unstructured, profileChanged bool) error {
	log := logf.FromContext(ctx)

	containers, found, err := unstructured.NestedSlice(notebook.Object, notebookContainersPath...)
	if err != nil {
		return err
	}
	if !found || len(containers) == 0 {
		return nil
	}

	mainIdx := notebookMainContainerIndex(containers, notebook.GetName())
	if mainIdx < 0 {
		log.Info("No matching main container found; skipping HWP resource injection",
			"workload", notebook.GetName(), "namespace", notebook.GetNamespace())

		return nil
	}

	container, ok := containers[mainIdx].(map[string]any)
	if !ok {
		return errors.New("container is not a map[string]any")
	}

	if profileChanged {
		// Clear profile-managed resources (requests/limits) but preserve
		// user-managed resources.claims entries so container-level DRA
		// references for non-webhook claims survive the profile change.
		preservedClaims := preserveUserResourceClaims(container, draClaimName)
		delete(container, "resources")

		if len(preservedClaims) > 0 {
			resourcesMap := map[string]any{"claims": preservedClaims}
			container["resources"] = resourcesMap
		}
	}

	if err := applyIdentifiersToContainer(container, identifiers); err != nil {
		return err
	}

	containers[mainIdx] = container

	return unstructured.SetNestedSlice(notebook.Object, containers, notebookContainersPath...)
}

func notebookMainContainerIndex(containers []any, notebookName string) int {
	if len(containers) == 1 {
		return 0
	}

	for idx, c := range containers {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}

		name, _ := m["name"].(string)
		if name == notebookName {
			return idx
		}
	}

	return -1
}

func applyIdentifiersToContainer(container map[string]any, identifiers []any) error {
	resourcesMap, err := getOrCreateNestedMap(container, "resources")
	if err != nil {
		return err
	}

	requests, err := getOrCreateNestedMap(resourcesMap, "requests")
	if err != nil {
		return err
	}

	limits, err := getOrCreateNestedMap(resourcesMap, "limits")
	if err != nil {
		return err
	}

	for _, id := range identifiers {
		idMap, ok := id.(map[string]any)
		if !ok {
			continue
		}

		identifier, _, _ := unstructured.NestedString(idMap, "identifier")
		if identifier == "" {
			continue
		}

		defaultCount, _, _ := unstructured.NestedFieldNoCopy(idMap, "defaultCount")
		if defaultCount == nil {
			continue
		}

		quantity, err := parseQuantityValue(defaultCount)
		if err != nil {
			return fmt.Errorf("failed to convert resource quantity for %s: %w", identifier, err)
		}

		if _, exists := requests[identifier]; !exists {
			requests[identifier] = quantity.String()
		}

		if _, exists := limits[identifier]; !exists {
			if reqVal, reqExists := requests[identifier]; reqExists {
				limits[identifier] = reqVal
			}
		}
	}

	resourcesMap["requests"] = requests
	resourcesMap["limits"] = limits
	container["resources"] = resourcesMap

	return nil
}

func mergeNodeSelector(
	obj *unstructured.Unstructured,
	nodeSelectorPath []string,
	hwpNodeSelector map[string]string,
	hwpName string,
) (map[string]string, []string, error) {
	existingNodeSelector, _, err := unstructured.NestedStringMap(obj.Object, nodeSelectorPath...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get existing nodeSelector: %w", err)
	}

	var warnings []string

	merged := make(map[string]string)
	maps.Copy(merged, existingNodeSelector)

	for k, v := range hwpNodeSelector {
		if existingValue, exists := existingNodeSelector[k]; exists && existingValue != v {
			warnings = append(warnings, fmt.Sprintf(
				"nodeSelector key '%s' has value '%s' which will be overwritten by HardwareProfile '%s' which has value '%s'",
				k, existingValue, hwpName, v))
		}

		merged[k] = v
	}

	return merged, warnings, nil
}

func mergeTolerations(
	obj *unstructured.Unstructured,
	tolerationsPath []string,
	hwpTolerations []any,
) ([]any, error) {
	existingTolerations, _, err := unstructured.NestedSlice(obj.Object, tolerationsPath...)
	if err != nil {
		return nil, fmt.Errorf("failed to get existing tolerations: %w", err)
	}

	hwpTolKeys := make(map[string]bool)
	for _, tol := range hwpTolerations {
		if tolMap, ok := tol.(map[string]any); ok {
			hwpTolKeys[TolerationKey(tolMap)] = true
		}
	}

	merged := make([]any, 0, len(hwpTolerations)+len(existingTolerations))
	merged = append(merged, hwpTolerations...)

	for _, existing := range existingTolerations {
		if existingMap, ok := existing.(map[string]any); ok {
			if !hwpTolKeys[TolerationKey(existingMap)] {
				merged = append(merged, existing)
			}
		}
	}

	return merged, nil
}

// TolerationKey generates a unique key for a toleration for comparison.
func TolerationKey(tol map[string]any) string {
	key, _ := tol["key"].(string)
	operator, _ := tol["operator"].(string)
	value, _ := tol["value"].(string)
	effect, _ := tol["effect"].(string)

	ts := ""
	if v, ok := tol["tolerationSeconds"]; ok {
		ts = fmt.Sprintf("%v", v)
	}

	return fmt.Sprintf("%s:%s:%s:%s:%s", key, operator, value, effect, ts)
}

func parseQuantityValue(val any) (resource.Quantity, error) {
	switch v := val.(type) {
	case string:
		return resource.ParseQuantity(v)
	case int64:
		return *resource.NewQuantity(v, resource.DecimalSI), nil
	case float64:
		return resource.ParseQuantity(strconv.FormatFloat(v, 'f', -1, 64))
	default:
		return resource.ParseQuantity(fmt.Sprintf("%v", v))
	}
}

func getOrCreateNestedMap(obj map[string]any, field string) (map[string]any, error) {
	nested, found, err := unstructured.NestedMap(obj, field)
	if err != nil {
		return nil, fmt.Errorf("failed to get nested map for field %s: %w", field, err)
	}

	if !found {
		nested = make(map[string]any)
	}

	return nested, nil
}

func getAnnotation(obj *unstructured.Unstructured, key string) string {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return ""
	}

	return annotations[key]
}

func setAnnotation(obj *unstructured.Unstructured, key, value string) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}

	annotations[key] = value
	obj.SetAnnotations(annotations)
}

func removeAnnotation(obj *unstructured.Unstructured, key string) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		return
	}

	delete(annotations, key)
	obj.SetAnnotations(annotations)
}

func getLabel(obj *unstructured.Unstructured, key string) string {
	labels := obj.GetLabels()
	if labels == nil {
		return ""
	}

	return labels[key]
}

func setLabel(obj *unstructured.Unstructured, key, value string) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}

	labels[key] = value
	obj.SetLabels(labels)
}

func removeLabel(obj *unstructured.Unstructured, key string) {
	labels := obj.GetLabels()
	if labels == nil {
		return
	}

	delete(labels, key)
	obj.SetLabels(labels)
}
