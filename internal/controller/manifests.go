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

package controller

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/kustomize/kyaml/yaml"
	sigyaml "sigs.k8s.io/yaml"

	componentsv1alpha1 "github.com/opendatahub-io/workbenches-operator/api/v1alpha1"
	"github.com/opendatahub-io/workbenches-operator/internal/metadata"
	"github.com/opendatahub-io/workbenches-operator/internal/platform"
)

const (
	fieldOwner     = "workbenches-operator"
	kindDeployment = "Deployment"
	kindService    = "Service"
)

// manifestGroupsForPlatform returns the kustomize root paths (relative to
// ManifestsBasePath) for the given platform type. The OpenShift overlay is
// always used for kf-notebook-controller because the operator only runs on
// OpenShift (both ODH and RHOAI). The notebooks overlay varies by platform.
func manifestGroupsForPlatform(platformType string, workbenchesV2Managed bool) []string {
	notebooksOverlay := "workbenches/notebooks/odh/base"

	if platformType == platform.SelfManagedRhoai {
		notebooksOverlay = "workbenches/notebooks/rhoai/base"
	}

	groups := []string{
		"workbenches/kf-notebook-controller/overlays/openshift",
		"workbenches/odh-notebook-controller/base",
		notebooksOverlay,
	}

	if workbenchesV2Managed {
		groups = append(groups, "workbenches/workspaces-controller/overlays/gateway")
	}

	return groups
}

func (r *WorkbenchesReconciler) workbenchesV2ManifestsExist() bool {
	v2Dir := filepath.Join(r.ManifestsBasePath, "workbenches", "workspaces-controller", "overlays", "gateway")
	info, err := os.Stat(v2Dir)

	return err == nil && info.IsDir()
}

// renderAndApply renders the upstream kustomize manifests with parameter injection
// and applies them to the cluster via Server-Side Apply with ForceOwnership.
// It copies manifests to a temp directory so the baked-in /opt/manifests stays immutable.
// After a successful apply it runs continuous GC (ODH-style): labeled managed resources
// that are no longer present in the rendered desired set are deleted so upgrades drop
// obsolete operands.
func (r *WorkbenchesReconciler) renderAndApply(
	ctx context.Context,
	owner *componentsv1alpha1.Workbenches,
	params map[string]string,
	namespace string,
	platformType string,
) error {
	l := log.FromContext(ctx)

	workDir, err := os.MkdirTemp("", "workbenches-manifests-*")
	if err != nil {
		return fmt.Errorf("failed to create temp work directory: %w", err)
	}

	defer func() {
		if removeErr := os.RemoveAll(workDir); removeErr != nil {
			log.FromContext(ctx).Error(removeErr, "failed to remove temp manifest directory")
		}
	}()

	// Copy the entire manifests tree once so overlay relative paths (../../base) resolve.
	srcRoot := filepath.Join(r.ManifestsBasePath, "workbenches")
	dstRoot := filepath.Join(workDir, "workbenches")

	if err := copyDir(srcRoot, dstRoot); err != nil {
		return fmt.Errorf("failed to copy manifests tree: %w", err)
	}

	groups := manifestGroupsForPlatform(platformType, owner.Spec.IsWorkbenchesV2Managed())
	desired := make(map[objectRef]struct{})

	for _, group := range groups {
		renderDir := filepath.Join(workDir, group)

		if _, statErr := os.Stat(renderDir); os.IsNotExist(statErr) {
			l.V(1).Info("manifest directory not found, skipping", "path", renderDir)

			continue
		}

		if err := patchKustomizeNamespace(renderDir, namespace, l); err != nil {
			return fmt.Errorf("failed to patch kustomize namespace for %s: %w", group, err)
		}

		objects, err := renderKustomize(renderDir, params)
		if err != nil {
			return fmt.Errorf("failed to render manifests for %s: %w", group, err)
		}

		l.Info("rendered manifests", "group", group, "count", len(objects))

		for _, obj := range objects {
			desired[objectRefFrom(obj)] = struct{}{}
		}

		if err := r.applyObjects(ctx, owner, objects); err != nil {
			return fmt.Errorf("failed to apply manifests for %s: %w", group, err)
		}
	}

	if err := r.gcOrphanedResources(ctx, namespace, desired); err != nil {
		return fmt.Errorf("failed to garbage-collect orphaned resources: %w", err)
	}

	return nil
}

// copyDir recursively copies src to dst, creating dst and all subdirectories.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)

		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dst)+string(os.PathSeparator)) && filepath.Clean(target) != filepath.Clean(dst) {
			return fmt.Errorf("path traversal detected: %s escapes destination %s", target, dst)
		}

		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}

		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to copy symlink %s", path)
		}

		data, err := os.ReadFile(path) //nolint:gosec // reading baked-in manifests from a known path
		if err != nil {
			return fmt.Errorf("failed to read %s: %w", path, err)
		}

		return os.WriteFile(filepath.Clean(target), data, 0o600) //nolint:gosec // target is validated above to stay within dst
	})
}

// renderKustomize runs kustomize on a directory, injecting params.env values.
func renderKustomize(kustomizeDir string, params map[string]string) ([]*unstructured.Unstructured, error) {
	fSys := filesys.MakeFsOnDisk()

	if err := ensureKustomization(fSys, kustomizeDir); err != nil {
		return nil, fmt.Errorf("failed to ensure kustomization: %w", err)
	}

	// Apply RELATED_IMAGE_* overrides onto existing params.env / params-latest.env
	// keys before merging CR-derived params. Product builds inject digest-pinned
	// registry.redhat.io images via these env vars on the module-operator pod.
	if err := applyRelatedImageParams(fSys, kustomizeDir); err != nil {
		return nil, fmt.Errorf("failed to apply related image params: %w", err)
	}

	if err := writeParamsEnv(fSys, kustomizeDir, params); err != nil {
		return nil, fmt.Errorf("failed to write params.env: %w", err)
	}

	opts := krusty.MakeDefaultOptions()
	opts.Reorder = krusty.ReorderOptionLegacy

	k := krusty.MakeKustomizer(opts)

	resMap, err := k.Run(fSys, kustomizeDir)
	if err != nil {
		return nil, fmt.Errorf("kustomize run failed for %s: %w", kustomizeDir, err)
	}

	objects := make([]*unstructured.Unstructured, 0, resMap.Size())

	for _, res := range resMap.Resources() {
		jsonBytes, err := res.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("failed to marshal resource %s: %w", res.OrgId(), err)
		}

		obj := &unstructured.Unstructured{}
		if err := obj.UnmarshalJSON(jsonBytes); err != nil {
			return nil, fmt.Errorf("failed to unmarshal resource: %w", err)
		}

		objects = append(objects, obj)
	}

	return objects, nil
}

// writeParamsEnv merges operator parameters into the existing params.env file.
// Existing keys are overwritten; keys not in params are preserved so that
// upstream image references and other defaults remain intact when RELATED_IMAGE_*
// env vars are unset. Related-image overrides are applied separately by
// applyRelatedImageParams before this runs.
func writeParamsEnv(fSys filesys.FileSystem, kustomizeDir string, params map[string]string) error {
	paramsPath := filepath.Join(kustomizeDir, "params.env")

	existing := make(map[string]string)
	var orderedKeys []string

	if data, err := fSys.ReadFile(paramsPath); err == nil {
		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}

			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}

			existing[k] = v
			orderedKeys = append(orderedKeys, k)
		}
	}

	var newKeys []string

	for k, v := range params {
		if strings.ContainsAny(k, "\n\r=") {
			return fmt.Errorf("params key contains invalid characters: %q", k)
		}

		if strings.ContainsAny(v, "\n\r") {
			return fmt.Errorf("params value for key %q contains invalid control characters", k)
		}

		if _, found := existing[k]; !found {
			newKeys = append(newKeys, k)
		}

		existing[k] = v
	}

	sort.Strings(newKeys)
	orderedKeys = append(orderedKeys, newKeys...)

	lines := make([]string, 0, len(orderedKeys))
	for _, k := range orderedKeys {
		lines = append(lines, k+"="+existing[k])
	}

	content := strings.Join(lines, "\n") + "\n"

	return fSys.WriteFile(paramsPath, []byte(content))
}

// applyObjects applies a set of unstructured objects to the cluster using Server-Side Apply.
// Namespace references are already set correctly by kustomize (via patchKustomizeNamespace).
// Owner references are set with SetControllerReference (matching opendatahub-operator) so
// Owns() watches fire on drift and Kubernetes GC can cascade-delete owned children when the
// Workbenches CR is deleted. Namespaces, CRDs, and ImageStreams are intentionally excluded.
//
// For Deployments, live container resources and replicas are merged onto the rendered
// manifest before SSA unless the live object has opendatahub.io/managed=true (parity with
// the former in-tree workbenches deploy.MergeDeployments path).
func (r *WorkbenchesReconciler) applyObjects(
	ctx context.Context,
	owner *componentsv1alpha1.Workbenches,
	objects []*unstructured.Unstructured,
) error {
	l := log.FromContext(ctx)

	for _, obj := range objects {
		setComponentLabels(obj)

		if shouldSetOwnerReference(obj) {
			// Clear any template-defined ownerReferences before setting the
			// controller reference — applyObjects is the single source of truth.
			obj.SetOwnerReferences(nil)

			if err := controllerutil.SetControllerReference(owner, obj, r.Scheme); err != nil {
				return fmt.Errorf("failed to set owner reference on %s %s/%s: %w",
					obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
			}
		}

		if err := r.preserveDeploymentCustomizations(ctx, obj); err != nil {
			return fmt.Errorf("failed to preserve Deployment customizations for %s/%s: %w",
				obj.GetNamespace(), obj.GetName(), err)
		}

		obj.SetManagedFields(nil)

		//nolint:staticcheck // client.Apply via Patch is the correct pattern for unstructured SSA
		err := r.Patch(ctx, obj,
			client.Apply,
			client.FieldOwner(fieldOwner),
			client.ForceOwnership,
		)
		if err != nil {
			l.Error(err, "SSA patch failed",
				"gvk", obj.GroupVersionKind(),
				"name", obj.GetName(),
				"namespace", obj.GetNamespace())

			return fmt.Errorf("failed to apply %s %s/%s: %w",
				obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
		}

		l.V(1).Info("applied resource",
			"kind", obj.GetKind(),
			"name", obj.GetName(),
			"namespace", obj.GetNamespace())
	}

	return nil
}

// preserveDeploymentCustomizations merges user-set container resources and replicas from
// the live Deployment onto the rendered object before SSA, matching opendatahub-operator's
// deploy.Action.apply Deployment branch.
//
// Behavior:
//   - non-Deployment: no-op
//   - Deployment does not exist yet: no-op (create from manifest)
//   - live has opendatahub.io/managed=true: no-op (SSA ForceOwnership reverts to manifest)
//   - otherwise: mergeDeployments(live → rendered)
func (r *WorkbenchesReconciler) preserveDeploymentCustomizations(
	ctx context.Context,
	obj *unstructured.Unstructured,
) error {
	if obj.GetKind() != kindDeployment {
		return nil
	}

	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(obj.GroupVersionKind())

	err := r.Get(ctx, client.ObjectKeyFromObject(obj), live)
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("failed to get live Deployment: %w", err)
	}

	if live.GetAnnotations()[metadata.ManagedAnnotation] == "true" {
		log.FromContext(ctx).V(1).Info("Deployment marked managed; applying manifest defaults",
			"name", obj.GetName(),
			"namespace", obj.GetNamespace())

		return nil
	}

	if err := mergeDeployments(live, obj); err != nil {
		return fmt.Errorf("failed to merge Deployment: %w", err)
	}

	log.FromContext(ctx).V(1).Info("preserved live Deployment resources/replicas",
		"name", obj.GetName(),
		"namespace", obj.GetNamespace())

	return nil
}

func setComponentLabels(obj *unstructured.Unstructured) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}

	labels[metadata.ComponentLabelKey] = metadata.LabelTrue
	labels[metadata.PartOfLabelKey] = metadata.ComponentLabelValue

	obj.SetLabels(labels)

	switch obj.GetKind() {
	case kindDeployment:
		patchNestedLabels(obj, "spec", "selector", "matchLabels")
		patchNestedLabels(obj, "spec", "template", "metadata", "labels")
	case kindService:
		patchNestedLabels(obj, "spec", "selector")
	}
}

// patchNestedLabels sets the operator's component labels on a nested label map
// (e.g. spec.selector.matchLabels, spec.template.metadata.labels, or
// spec.selector for Services) so that selectors stay consistent with the
// top-level metadata labels. It is a no-op when the nested map does not exist.
func patchNestedLabels(obj *unstructured.Unstructured, fields ...string) {
	nested, found, err := unstructured.NestedStringMap(obj.Object, fields...)
	if err != nil || !found {
		return
	}

	nested[metadata.ComponentLabelKey] = metadata.LabelTrue
	nested[metadata.PartOfLabelKey] = metadata.ComponentLabelValue

	_ = unstructured.SetNestedStringMap(obj.Object, nested, fields...)
}

var clusterScopedKinds = map[string]bool{
	"Namespace":                      true,
	"ClusterRole":                    true,
	"ClusterRoleBinding":             true,
	"CustomResourceDefinition":       true,
	"MutatingWebhookConfiguration":   true,
	"ValidatingWebhookConfiguration": true,
}

// skipOwnerRefKinds are never owned by the Workbenches CR.
//   - Namespace: cascade would delete the entire namespace.
//   - CustomResourceDefinition: left on cluster for upgrade safety (ODH deployCRD pattern);
//     also never deleted during cleanup so Notebook CRs are not wiped.
//   - ImageStream: watched (managed part-of label) for drift + ImageStreamsAvailable;
//     never Owned; omitted from cleanupGVKs (ODH onlyOwned GC skipped them).
var skipOwnerRefKinds = map[string]bool{
	"Namespace":                true,
	"CustomResourceDefinition": true,
	"ImageStream":              true,
}

func isNamespaced(obj *unstructured.Unstructured) bool {
	return !clusterScopedKinds[obj.GetKind()]
}

func shouldSetOwnerReference(obj *unstructured.Unstructured) bool {
	return !skipOwnerRefKinds[obj.GetKind()]
}

// patchKustomizeNamespace sets the namespace field in the kustomization file
// at the given directory. If the file already has a namespace field it is
// replaced; otherwise one is added. This lets kustomize's built-in namespace
// transformer handle ALL namespace references — including internal refs in
// ClusterRoleBinding subjects and webhook service configs — so the operator
// does not need to post-process them.
//
// Uses structured YAML parsing to avoid injection risks and to preserve
// nested "namespace:" fields (e.g. inside replacements selectors).
func patchKustomizeNamespace(dir string, namespace string, logger logr.Logger) error {
	kustomizationPath := findKustomizationFile(dir)
	if kustomizationPath == "" {
		return fmt.Errorf("no kustomization file found in %s — namespace %q would not be applied", dir, namespace)
	}

	data, err := os.ReadFile(kustomizationPath) //nolint:gosec // reading from operator-owned temp directory
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", kustomizationPath, err)
	}

	var kustomization map[string]any
	if unmarshalErr := sigyaml.Unmarshal(data, &kustomization); unmarshalErr != nil {
		return fmt.Errorf("failed to parse %s: %w", kustomizationPath, unmarshalErr)
	}

	oldNS, _ := kustomization["namespace"].(string)
	if oldNS == namespace {
		logger.Info("kustomization namespace already set, skipping patch",
			"file", kustomizationPath,
			"namespace", namespace)

		return nil
	}

	kustomization["namespace"] = namespace

	logger.Info("patching kustomization namespace",
		"file", kustomizationPath,
		"oldNamespace", oldNS,
		"newNamespace", namespace)

	out, marshalErr := sigyaml.Marshal(kustomization)
	if marshalErr != nil {
		return fmt.Errorf("failed to serialize %s: %w", kustomizationPath, marshalErr)
	}

	return os.WriteFile(kustomizationPath, out, 0o600)
}

func findKustomizationFile(dir string) string {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	return ""
}

// cleanupGVKs lists the GroupVersionKinds of namespaced resources to clean up
// (Removed / finalizer / continuous GC). Aligned with in-tree ODH workbenches:
// Secrets were Owned and GC-eligible; ImageStreams were never Owned and were
// skipped by onlyOwned GC, so they are intentionally omitted here.
var cleanupGVKs = []schema.GroupVersionKind{
	{Group: "apps", Version: "v1", Kind: kindDeployment},
	{Group: "", Version: "v1", Kind: "ConfigMap"},
	{Group: "", Version: "v1", Kind: "Secret"},
	{Group: "", Version: "v1", Kind: kindService},
	{Group: "", Version: "v1", Kind: "ServiceAccount"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"},
}

// cleanupClusterGVKs lists the GroupVersionKinds of cluster-scoped resources to clean up.
// CustomResourceDefinitions are intentionally omitted: ODH never owned or GCd CRDs, and
// deleting notebooks.kubeflow.org would cascade-delete all Notebook instances.
var cleanupClusterGVKs = []schema.GroupVersionKind{
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"},
	{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "MutatingWebhookConfiguration"},
	{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingWebhookConfiguration"},
}

// objectRef uniquely identifies a managed resource for desired-set GC.
type objectRef struct {
	gvk       schema.GroupVersionKind
	namespace string
	name      string
}

func objectRefFrom(obj *unstructured.Unstructured) objectRef {
	return objectRef{
		gvk:       obj.GroupVersionKind(),
		namespace: obj.GetNamespace(),
		name:      obj.GetName(),
	}
}

func componentMatchingLabels() client.MatchingLabels {
	return client.MatchingLabels{
		metadata.ComponentLabelKey: metadata.LabelTrue,
		metadata.PartOfLabelKey:    metadata.ComponentLabelValue,
	}
}

// gcOrphanedResources deletes labeled managed resources that are no longer in the
// rendered desired set (continuous GC, matching ODH gc.NewAction intent).
// Skips when desired is empty to avoid wiping the cluster if rendering produced nothing.
// CRDs are never GCd (not listed in cleanupClusterGVKs).
func (r *WorkbenchesReconciler) gcOrphanedResources(
	ctx context.Context,
	namespace string,
	desired map[objectRef]struct{},
) error {
	l := log.FromContext(ctx)

	if len(desired) == 0 {
		l.Info("skipping continuous GC: no desired resources rendered")

		return nil
	}

	l.V(1).Info("running continuous GC for orphaned managed resources",
		"desired", len(desired),
		"namespace", namespace)

	var errs []error
	componentLabel := componentMatchingLabels()

	for _, gvk := range cleanupGVKs {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk)

		if err := r.List(ctx, list, client.InNamespace(namespace), componentLabel); err != nil {
			if meta.IsNoMatchError(err) {
				l.V(1).Info("skipping GVK during GC (API not available)", "gvk", gvk)

				continue
			}

			errs = append(errs, fmt.Errorf("failed to list %s for GC: %w", gvk, err))

			continue
		}

		for i := range list.Items {
			obj := &list.Items[i]
			ref := objectRefFrom(obj)
			if _, keep := desired[ref]; keep {
				continue
			}

			l.Info("garbage-collecting orphaned resource",
				"kind", obj.GetKind(),
				"name", obj.GetName(),
				"namespace", obj.GetNamespace())

			if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
				errs = append(errs, fmt.Errorf("failed to GC %s %s/%s: %w",
					obj.GetKind(), obj.GetNamespace(), obj.GetName(), err))
			}
		}
	}

	for _, gvk := range cleanupClusterGVKs {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk)

		if err := r.List(ctx, list, componentLabel); err != nil {
			if meta.IsNoMatchError(err) {
				l.V(1).Info("skipping cluster GVK during GC (API not available)", "gvk", gvk)

				continue
			}

			errs = append(errs, fmt.Errorf("failed to list cluster %s for GC: %w", gvk, err))

			continue
		}

		for i := range list.Items {
			obj := &list.Items[i]
			ref := objectRefFrom(obj)
			if _, keep := desired[ref]; keep {
				continue
			}

			l.Info("garbage-collecting orphaned cluster resource",
				"kind", obj.GetKind(),
				"name", obj.GetName())

			if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
				errs = append(errs, fmt.Errorf("failed to GC %s %s: %w",
					obj.GetKind(), obj.GetName(), err))
			}
		}
	}

	return errors.Join(errs...)
}

// cleanupManagedResources deletes all resources that were applied by this operator,
// identified by the component labels. It cleans both namespaced and cluster-scoped resources.
// Cleanup is best-effort: all resource types are attempted even if some fail, and
// any errors are aggregated and returned at the end.
func (r *WorkbenchesReconciler) cleanupManagedResources(ctx context.Context, namespace string) error {
	l := log.FromContext(ctx)
	l.Info("cleaning up managed resources", "namespace", namespace)

	var errs []error

	componentLabel := componentMatchingLabels()

	for _, gvk := range cleanupGVKs {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk)

		if err := r.List(ctx, list, client.InNamespace(namespace), componentLabel); err != nil {
			if meta.IsNoMatchError(err) {
				l.Info("skipping GVK during cleanup (API not available)", "gvk", gvk)

				continue
			}

			errs = append(errs, fmt.Errorf("failed to list %s: %w", gvk, err))

			continue
		}

		for i := range list.Items {
			obj := &list.Items[i]
			l.Info("deleting resource", "kind", obj.GetKind(), "name", obj.GetName(), "namespace", obj.GetNamespace())

			if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
				errs = append(errs, fmt.Errorf("failed to delete %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err))
			}
		}
	}

	for _, gvk := range cleanupClusterGVKs {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk)

		if err := r.List(ctx, list, componentLabel); err != nil {
			if meta.IsNoMatchError(err) {
				l.Info("skipping cluster GVK during cleanup (API not available)", "gvk", gvk)

				continue
			}

			errs = append(errs, fmt.Errorf("failed to list cluster %s: %w", gvk, err))

			continue
		}

		for i := range list.Items {
			obj := &list.Items[i]
			l.Info("deleting cluster resource", "kind", obj.GetKind(), "name", obj.GetName())

			if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
				errs = append(errs, fmt.Errorf("failed to delete %s %s: %w", obj.GetKind(), obj.GetName(), err))
			}
		}
	}

	l.Info("managed resources cleanup complete")

	return errors.Join(errs...)
}

// ensureKustomization creates a minimal kustomization.yaml if one does not exist,
// pointing to all YAML files in the directory. This handles upstream directories
// that rely on being included as bases rather than standalone kustomize roots.
func ensureKustomization(fSys filesys.FileSystem, dir string) error {
	kustomizationPath := filepath.Join(dir, "kustomization.yaml")
	if fSys.Exists(kustomizationPath) {
		return nil
	}

	kustomizationPath = filepath.Join(dir, "kustomization.yml")
	if fSys.Exists(kustomizationPath) {
		return nil
	}

	kustomizationPath = filepath.Join(dir, "Kustomization")
	if fSys.Exists(kustomizationPath) {
		return nil
	}

	kustomization := types.Kustomization{
		TypeMeta: types.TypeMeta{
			APIVersion: types.KustomizationVersion,
			Kind:       types.KustomizationKind,
		},
	}

	entries, err := fSys.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("failed to read directory %s: %w", dir, err)
	}

	for _, name := range entries {
		if !fSys.IsDir(filepath.Join(dir, name)) && (strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")) {
			kustomization.Resources = append(kustomization.Resources, name)
		}
	}

	node, err := yaml.FromMap(map[string]any{
		"apiVersion": kustomization.APIVersion,
		"kind":       kustomization.Kind,
		"resources":  kustomization.Resources,
	})
	if err != nil {
		return fmt.Errorf("failed to build kustomization node: %w", err)
	}

	content, err := node.String()
	if err != nil {
		return fmt.Errorf("failed to serialize kustomization: %w", err)
	}

	return fSys.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(content))
}
