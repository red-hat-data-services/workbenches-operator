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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	sigyaml "sigs.k8s.io/yaml"

	componentsv1alpha1 "github.com/opendatahub-io/workbenches-operator/api/v1alpha1"
	"github.com/opendatahub-io/workbenches-operator/internal/metadata"
	"github.com/opendatahub-io/workbenches-operator/internal/platform"
)

const testDir = "/test"

var errSimulatedDeleteFailure = errors.New("simulated delete failure")

func TestManifestGroupsForPlatform(t *testing.T) {
	const wantKF = "workbenches/kf-notebook-controller/overlays/openshift"

	tests := []struct {
		name          string
		platformType  string
		wantNotebooks string
	}{
		{"OpenShift self-managed", platform.SelfManagedRhoai, "workbenches/notebooks/rhoai/base"},
		{"OpenDataHub", platform.OpenDataHub, "workbenches/notebooks/odh/base"},
		{"empty platform", "", "workbenches/notebooks/odh/base"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups := manifestGroupsForPlatform(tt.platformType, false)
			if len(groups) != 3 {
				t.Fatalf("expected 3 groups, got %d", len(groups))
			}

			if groups[0] != wantKF {
				t.Errorf("kf group = %q, want %q", groups[0], wantKF)
			}

			if groups[1] != "workbenches/odh-notebook-controller/base" {
				t.Errorf("odh group = %q, want workbenches/odh-notebook-controller/base", groups[1])
			}

			if groups[2] != tt.wantNotebooks {
				t.Errorf("notebooks group = %q, want %q", groups[2], tt.wantNotebooks)
			}
		})
	}
}

func TestManifestGroupsForPlatformWithWorkbenchesV2(t *testing.T) {
	tests := []struct {
		name         string
		platformType string
		wantCount    int
		wantV2       string
	}{
		{"ODH with v2 enabled", platform.OpenDataHub, 4, "workbenches/workspaces-controller/overlays/gateway"},
		{"RHOAI with v2 enabled", platform.SelfManagedRhoai, 4, "workbenches/workspaces-controller/overlays/gateway"},
		{"ODH with v2 disabled", platform.OpenDataHub, 3, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v2Managed := tt.wantCount == 4
			groups := manifestGroupsForPlatform(tt.platformType, v2Managed)

			if len(groups) != tt.wantCount {
				t.Fatalf("expected %d groups, got %d", tt.wantCount, len(groups))
			}

			if v2Managed {
				if groups[3] != tt.wantV2 {
					t.Errorf("v2 group = %q, want %q", groups[3], tt.wantV2)
				}
			}
		})
	}
}

func TestWriteParamsEnv(t *testing.T) {
	fSys := filesys.MakeFsInMemory()
	dir := testDir

	if err := fSys.Mkdir(dir); err != nil {
		t.Fatal(err)
	}

	params := map[string]string{
		paramGatewayURL:    "https://gw.example.com",
		paramSectionTitle:  "OpenShift AI",
		paramMLflowEnabled: "true",
	}

	if err := writeParamsEnv(fSys, dir, params); err != nil {
		t.Fatalf("writeParamsEnv() error = %v", err)
	}

	data, err := fSys.ReadFile(filepath.Join(dir, "params.env"))
	if err != nil {
		t.Fatalf("reading params.env: %v", err)
	}

	content := string(data)

	for k, v := range params {
		expected := k + "=" + v
		if !strings.Contains(content, expected) {
			t.Errorf("params.env missing %q; got:\n%s", expected, content)
		}
	}

	if !strings.HasSuffix(content, "\n") {
		t.Error("params.env should end with a newline")
	}
}

func TestWriteParamsEnvMergesWithExisting(t *testing.T) {
	fSys := filesys.MakeFsInMemory()
	dir := testDir

	if err := fSys.Mkdir(dir); err != nil {
		t.Fatal(err)
	}

	existing := "odh-notebook-controller-image=quay.io/opendatahub/odh-notebook-controller:main\n" +
		"kube-rbac-proxy=quay.io/opendatahub/proxy:latest\n" +
		"gateway-url=\n" +
		"mlflow-enabled=false\n"

	if err := fSys.WriteFile(filepath.Join(dir, "params.env"), []byte(existing)); err != nil {
		t.Fatal(err)
	}

	params := map[string]string{
		paramGatewayURL:    "https://gw.example.com",
		paramMLflowEnabled: "true",
	}

	if err := writeParamsEnv(fSys, dir, params); err != nil {
		t.Fatalf("writeParamsEnv() error = %v", err)
	}

	data, err := fSys.ReadFile(filepath.Join(dir, "params.env"))
	if err != nil {
		t.Fatalf("reading params.env: %v", err)
	}

	content := string(data)

	if !strings.Contains(content, "odh-notebook-controller-image=quay.io/opendatahub/odh-notebook-controller:main") {
		t.Error("existing image reference was not preserved")
	}

	if !strings.Contains(content, "kube-rbac-proxy=quay.io/opendatahub/proxy:latest") {
		t.Error("existing kube-rbac-proxy reference was not preserved")
	}

	if !strings.Contains(content, "gateway-url=https://gw.example.com") {
		t.Error("gateway-url was not overwritten")
	}

	if !strings.Contains(content, "mlflow-enabled=true") {
		t.Error("mlflow-enabled was not overwritten")
	}
}

func TestWriteParamsEnvEmpty(t *testing.T) {
	fSys := filesys.MakeFsInMemory()
	dir := testDir

	if err := fSys.Mkdir(dir); err != nil {
		t.Fatal(err)
	}

	if err := writeParamsEnv(fSys, dir, map[string]string{}); err != nil {
		t.Fatalf("writeParamsEnv() with empty params error = %v", err)
	}

	data, err := fSys.ReadFile(filepath.Join(dir, "params.env"))
	if err != nil {
		t.Fatalf("reading params.env: %v", err)
	}

	if string(data) != "\n" {
		t.Errorf("expected single newline for empty params, got %q", string(data))
	}
}

func TestWriteParamsEnvNewKeysDeterministicOrder(t *testing.T) {
	fSys := filesys.MakeFsInMemory()
	dir := testDir

	if err := fSys.Mkdir(dir); err != nil {
		t.Fatal(err)
	}

	params := map[string]string{
		"zebra":  "z",
		"alpha":  "a",
		"middle": "m",
		"beta":   "b",
	}

	if err := writeParamsEnv(fSys, dir, params); err != nil {
		t.Fatalf("writeParamsEnv() error = %v", err)
	}

	data, err := fSys.ReadFile(filepath.Join(dir, "params.env"))
	if err != nil {
		t.Fatalf("reading params.env: %v", err)
	}

	expected := "alpha=a\nbeta=b\nmiddle=m\nzebra=z\n"
	if string(data) != expected {
		t.Errorf("new keys not in sorted order\ngot:  %q\nwant: %q", string(data), expected)
	}
}

func TestWriteParamsEnvRejectsControlCharacters(t *testing.T) {
	fSys := filesys.MakeFsInMemory()
	dir := testDir

	if err := fSys.Mkdir(dir); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		params map[string]string
	}{
		{"newline in value", map[string]string{"key": "val\nMALICIOUS=injected"}},
		{"carriage return in value", map[string]string{"key": "val\rMALICIOUS=injected"}},
		{"newline in key", map[string]string{"bad\nkey": "value"}},
		{"carriage return in key", map[string]string{"bad\rkey": "value"}},
		{"equals in key", map[string]string{"bad=key": "value"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := writeParamsEnv(fSys, dir, tt.params)
			if err == nil {
				t.Error("expected error for params containing control characters, got nil")
			}
		})
	}
}

func TestSetComponentLabels(t *testing.T) {
	t.Run("adds labels to unlabeled object", func(t *testing.T) {
		obj := &unstructured.Unstructured{}
		obj.SetName("test")

		setComponentLabels(obj)

		labels := obj.GetLabels()
		if labels[metadata.ComponentLabelKey] != metadata.LabelTrue {
			t.Errorf("expected %s=%s, got %s",
				metadata.ComponentLabelKey, metadata.LabelTrue, labels[metadata.ComponentLabelKey])
		}

		if labels[metadata.PartOfLabelKey] != metadata.ComponentLabelValue {
			t.Errorf("expected %s=%s, got %s",
				metadata.PartOfLabelKey, metadata.ComponentLabelValue, labels[metadata.PartOfLabelKey])
		}
	})

	t.Run("preserves existing labels", func(t *testing.T) {
		obj := &unstructured.Unstructured{}
		obj.SetLabels(map[string]string{"existing": "label"})

		setComponentLabels(obj)

		labels := obj.GetLabels()
		if labels["existing"] != "label" {
			t.Error("existing label was not preserved")
		}

		if labels[metadata.ComponentLabelKey] != metadata.LabelTrue {
			t.Error("component label was not set")
		}
	})

	t.Run("patches Deployment matchLabels and template labels", func(t *testing.T) {
		obj := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]any{"name": "notebook-controller"},
				"spec": map[string]any{
					"selector": map[string]any{
						"matchLabels": map[string]any{
							"app":                           "notebook-controller",
							"app.kubernetes.io/part-of":     "odh-notebook-controller",
							"component.opendatahub.io/name": "kf-notebook-controller",
						},
					},
					"template": map[string]any{
						"metadata": map[string]any{
							"labels": map[string]any{
								"app":                           "notebook-controller",
								"app.kubernetes.io/part-of":     "odh-notebook-controller",
								"component.opendatahub.io/name": "kf-notebook-controller",
							},
						},
					},
				},
			},
		}

		setComponentLabels(obj)

		matchLabels, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "selector", "matchLabels")
		if matchLabels[metadata.PartOfLabelKey] != metadata.ComponentLabelValue {
			t.Errorf("matchLabels: expected %s=%s, got %s",
				metadata.PartOfLabelKey, metadata.ComponentLabelValue, matchLabels[metadata.PartOfLabelKey])
		}

		if matchLabels[metadata.ComponentLabelKey] != metadata.LabelTrue {
			t.Errorf("matchLabels: expected %s=%s, got %s",
				metadata.ComponentLabelKey, metadata.LabelTrue, matchLabels[metadata.ComponentLabelKey])
		}

		if matchLabels["app"] != "notebook-controller" {
			t.Error("matchLabels: existing label 'app' was not preserved")
		}

		templateLabels, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "labels")
		if templateLabels[metadata.PartOfLabelKey] != metadata.ComponentLabelValue {
			t.Errorf("template labels: expected %s=%s, got %s",
				metadata.PartOfLabelKey, metadata.ComponentLabelValue, templateLabels[metadata.PartOfLabelKey])
		}

		if templateLabels[metadata.ComponentLabelKey] != metadata.LabelTrue {
			t.Errorf("template labels: expected %s=%s, got %s",
				metadata.ComponentLabelKey, metadata.LabelTrue, templateLabels[metadata.ComponentLabelKey])
		}
	})

	t.Run("patches Service spec.selector", func(t *testing.T) {
		obj := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]any{"name": "test-svc"},
				"spec": map[string]any{
					"selector": map[string]any{
						"app":                       "notebook-controller",
						"app.kubernetes.io/part-of": "odh-notebook-controller",
					},
				},
			},
		}

		setComponentLabels(obj)

		selector, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "selector")
		if selector[metadata.PartOfLabelKey] != metadata.ComponentLabelValue {
			t.Errorf("Service spec.selector[%s] = %q, want %q",
				metadata.PartOfLabelKey, selector[metadata.PartOfLabelKey], metadata.ComponentLabelValue)
		}

		if selector[metadata.ComponentLabelKey] != metadata.LabelTrue {
			t.Errorf("Service spec.selector[%s] = %q, want %q",
				metadata.ComponentLabelKey, selector[metadata.ComponentLabelKey], metadata.LabelTrue)
		}

		if selector["app"] != "notebook-controller" {
			t.Error("Service spec.selector: existing label 'app' was not preserved")
		}
	})

	t.Run("does not patch selector for non-Deployment non-Service kinds", func(t *testing.T) {
		obj := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "test-cm"},
				"data": map[string]any{
					"key": "value",
				},
			},
		}

		setComponentLabels(obj)

		labels := obj.GetLabels()
		if labels[metadata.ComponentLabelKey] != metadata.LabelTrue {
			t.Error("metadata.labels should still be patched")
		}
	})

	t.Run("skips Service selector patching when no selector exists", func(t *testing.T) {
		obj := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]any{"name": "no-selector-svc"},
				"spec": map[string]any{
					"ports": []any{
						map[string]any{"port": int64(443)},
					},
				},
			},
		}

		setComponentLabels(obj)

		_, found, _ := unstructured.NestedStringMap(obj.Object, "spec", "selector")
		if found {
			t.Error("Service without selector should not get a selector added")
		}
	})
}

func TestIsNamespaced(t *testing.T) {
	tests := []struct {
		kind     string
		expected bool
	}{
		{"Deployment", true},
		{"Service", true},
		{"ConfigMap", true},
		{"ServiceAccount", true},
		{"Namespace", false},
		{"ClusterRole", false},
		{"ClusterRoleBinding", false},
		{"CustomResourceDefinition", false},
		{"MutatingWebhookConfiguration", false},
		{"ValidatingWebhookConfiguration", false},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			obj.Object = map[string]any{
				"kind": tt.kind,
			}

			got := isNamespaced(obj)
			if got != tt.expected {
				t.Errorf("isNamespaced(%q) = %v, want %v", tt.kind, got, tt.expected)
			}
		})
	}
}

func TestShouldSetOwnerReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind string
		want bool
	}{
		{"Deployment", true},
		{"Service", true},
		{"ConfigMap", true},
		{"Secret", true},
		{"ServiceAccount", true},
		{"Role", true},
		{"RoleBinding", true},
		{"ClusterRole", true},
		{"ClusterRoleBinding", true},
		{"MutatingWebhookConfiguration", true},
		{"ValidatingWebhookConfiguration", true},
		{"Namespace", false},
		{"CustomResourceDefinition", false},
		{"ImageStream", false},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			t.Parallel()

			obj := &unstructured.Unstructured{}
			obj.SetKind(tt.kind)

			got := shouldSetOwnerReference(obj)
			if got != tt.want {
				t.Errorf("shouldSetOwnerReference(%q) = %v, want %v", tt.kind, got, tt.want)
			}
		})
	}
}

func TestApplyObjectsSetsOwnerReferences(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(componentsv1alpha1.AddToScheme(scheme))

	owner := &componentsv1alpha1.Workbenches{
		ObjectMeta: metav1.ObjectMeta{
			Name: componentsv1alpha1.WorkbenchesInstanceName,
			UID:  "owner-uid-123",
		},
	}

	deployment := &unstructured.Unstructured{}
	deployment.SetAPIVersion("apps/v1")
	deployment.SetKind("Deployment")
	deployment.SetName("notebook-controller")
	deployment.SetNamespace("opendatahub")
	deployment.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "example.com/v1",
		Kind:       "Stale",
		Name:       "stale",
		UID:        "stale-uid",
	}})

	clusterRole := &unstructured.Unstructured{}
	clusterRole.SetAPIVersion("rbac.authorization.k8s.io/v1")
	clusterRole.SetKind("ClusterRole")
	clusterRole.SetName("notebook-controller-role")

	namespace := &unstructured.Unstructured{}
	namespace.SetAPIVersion("v1")
	namespace.SetKind("Namespace")
	namespace.SetName("opendatahub")

	crd := &unstructured.Unstructured{}
	crd.SetAPIVersion("apiextensions.k8s.io/v1")
	crd.SetKind("CustomResourceDefinition")
	crd.SetName("notebooks.kubeflow.org")

	imageStream := &unstructured.Unstructured{}
	imageStream.SetAPIVersion("image.openshift.io/v1")
	imageStream.SetKind("ImageStream")
	imageStream.SetName("jupyter-minimal")
	imageStream.SetNamespace("opendatahub")

	var patched []unstructured.Unstructured

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				u, ok := obj.(*unstructured.Unstructured)
				if !ok {
					t.Fatalf("expected unstructured, got %T", obj)
				}
				patched = append(patched, *u.DeepCopy())

				return nil
			},
		}).
		Build()

	reconciler := &WorkbenchesReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	err := reconciler.applyObjects(context.Background(), owner, []*unstructured.Unstructured{
		deployment,
		clusterRole,
		namespace,
		crd,
		imageStream,
	})
	if err != nil {
		t.Fatalf("applyObjects() error = %v", err)
	}

	if len(patched) != 5 {
		t.Fatalf("expected 5 patched objects, got %d", len(patched))
	}

	byKind := map[string]unstructured.Unstructured{}
	for _, obj := range patched {
		byKind[obj.GetKind()] = obj
	}

	for _, kind := range []string{"Deployment", "ClusterRole"} {
		obj := byKind[kind]
		refs := obj.GetOwnerReferences()
		if len(refs) != 1 {
			t.Fatalf("%s ownerRefs len = %d, want 1", kind, len(refs))
		}

		ref := refs[0]
		if ref.Name != owner.Name || ref.UID != owner.UID {
			t.Fatalf("%s ownerRef = %+v, want Workbenches/%s uid=%s", kind, ref, owner.Name, owner.UID)
		}

		if ref.Controller == nil || !*ref.Controller {
			t.Fatalf("%s ownerRef.Controller = %v, want true", kind, ref.Controller)
		}

		if ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
			t.Fatalf("%s ownerRef.BlockOwnerDeletion = %v, want true", kind, ref.BlockOwnerDeletion)
		}

		if obj.GetLabels()[metadata.ComponentLabelKey] != metadata.LabelTrue {
			t.Fatalf("%s missing component label", kind)
		}
	}

	for _, kind := range []string{"Namespace", "CustomResourceDefinition", "ImageStream"} {
		obj := byKind[kind]
		if len(obj.GetOwnerReferences()) != 0 {
			t.Fatalf("%s ownerRefs = %+v, want none", kind, obj.GetOwnerReferences())
		}
	}
}

func TestApplyObjectsPreservesDeploymentCustomizations(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(componentsv1alpha1.AddToScheme(scheme))

	owner := &componentsv1alpha1.Workbenches{
		ObjectMeta: metav1.ObjectMeta{
			Name: componentsv1alpha1.WorkbenchesInstanceName,
			UID:  "owner-uid-456",
		},
	}

	tests := []struct {
		name            string
		liveAnnotations map[string]string
		wantCPU         string
		wantReplicas    int64
	}{
		{
			name:         "preserves live resources and replicas when unmanaged",
			wantCPU:      "1001m",
			wantReplicas: 2,
		},
		{
			name: "applies manifest defaults when managed=true",
			liveAnnotations: map[string]string{
				metadata.ManagedAnnotation: "true",
			},
			wantCPU:      "500m",
			wantReplicas: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			live := deploymentUnstructured("notebook-controller-deployment", "opendatahub", "1001m", 2)
			if tt.liveAnnotations != nil {
				live.SetAnnotations(tt.liveAnnotations)
			}

			desired := deploymentUnstructured("notebook-controller-deployment", "opendatahub", "500m", 1)

			var patched *unstructured.Unstructured

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(live).
				WithInterceptorFuncs(interceptor.Funcs{
					Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
						u, ok := obj.(*unstructured.Unstructured)
						if !ok {
							t.Fatalf("expected unstructured, got %T", obj)
						}
						patched = u.DeepCopy()

						return nil
					},
				}).
				Build()

			reconciler := &WorkbenchesReconciler{
				Client: fakeClient,
				Scheme: scheme,
			}

			if err := reconciler.applyObjects(context.Background(), owner, []*unstructured.Unstructured{desired}); err != nil {
				t.Fatalf("applyObjects() error = %v", err)
			}

			if patched == nil {
				t.Fatal("expected Deployment to be patched")
			}

			replicas, found, err := unstructured.NestedInt64(patched.Object, "spec", "replicas")
			if err != nil || !found {
				t.Fatalf("replicas found=%v err=%v", found, err)
			}

			if replicas != tt.wantReplicas {
				t.Errorf("replicas = %d, want %d", replicas, tt.wantReplicas)
			}

			containers, _, err := unstructured.NestedSlice(patched.Object, "spec", "template", "spec", "containers")
			if err != nil || len(containers) == 0 {
				t.Fatalf("containers: len=%d err=%v", len(containers), err)
			}

			c0, _ := containers[0].(map[string]any)
			resources, _ := c0["resources"].(map[string]any)
			limits, _ := resources["limits"].(map[string]any)
			if limits["cpu"] != tt.wantCPU {
				t.Errorf("limits.cpu = %v, want %s", limits["cpu"], tt.wantCPU)
			}
		})
	}
}

func deploymentUnstructured(name, namespace, cpuLimit string, replicas int64) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"replicas": replicas,
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"name": "manager",
							"resources": map[string]any{
								"limits": map[string]any{
									"cpu": cpuLimit,
								},
								"requests": map[string]any{
									"cpu": cpuLimit,
								},
							},
						},
					},
				},
			},
		},
	}}

	return obj
}

func TestRenderKustomize(t *testing.T) {
	dir := t.TempDir()

	deployYAML := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-controller
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: test
  template:
    metadata:
      labels:
        app: test
    spec:
      containers:
      - name: manager
        image: test:latest
`
	if err := os.WriteFile(filepath.Join(dir, "deployment.yaml"), []byte(deployYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	kustomizationYAML := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- deployment.yaml
`
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(kustomizationYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	objects, err := renderKustomize(dir, map[string]string{})
	if err != nil {
		t.Fatalf("renderKustomize() error = %v", err)
	}

	if len(objects) != 1 {
		t.Fatalf("expected 1 object, got %d", len(objects))
	}

	obj := objects[0]
	if obj.GetKind() != "Deployment" {
		t.Errorf("expected kind Deployment, got %s", obj.GetKind())
	}

	if obj.GetName() != "test-controller" {
		t.Errorf("expected name test-controller, got %s", obj.GetName())
	}
}

func TestRenderKustomizeWithParams(t *testing.T) {
	dir := t.TempDir()

	cmYAML := `apiVersion: v1
kind: ConfigMap
metadata:
  name: workbench-config
data: {}
`
	if err := os.WriteFile(filepath.Join(dir, "configmap.yaml"), []byte(cmYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	kustomizationYAML := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- configmap.yaml
configMapGenerator:
- name: workbench-params
  envs:
  - params.env
`
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(kustomizationYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	params := map[string]string{
		paramSectionTitle: "Red Hat OpenShift AI",
	}

	objects, err := renderKustomize(dir, params)
	if err != nil {
		t.Fatalf("renderKustomize() error = %v", err)
	}

	if len(objects) != 2 {
		t.Fatalf("expected 2 objects (configmap + generated configmap), got %d", len(objects))
	}

	var foundGeneratedCM bool

	for _, obj := range objects {
		if obj.GetKind() == "ConfigMap" && strings.HasPrefix(obj.GetName(), "workbench-params") {
			foundGeneratedCM = true

			data, ok, err := unstructured.NestedStringMap(obj.Object, "data")
			if err != nil || !ok {
				t.Fatal("generated ConfigMap missing data field")
			}

			if data[paramSectionTitle] != "Red Hat OpenShift AI" {
				t.Errorf("expected section-title=%q, got %q", "Red Hat OpenShift AI", data[paramSectionTitle])
			}
		}
	}

	if !foundGeneratedCM {
		t.Error("generated ConfigMap not found in rendered objects")
	}
}

func TestRenderKustomizeNonexistentDir(t *testing.T) {
	_, err := renderKustomize("/nonexistent/path", map[string]string{})
	if err == nil {
		t.Fatal("expected error for nonexistent directory")
	}
}

func TestEnsureKustomization(t *testing.T) {
	t.Run("creates kustomization when none exists", func(t *testing.T) {
		dir := t.TempDir()

		if err := os.WriteFile(filepath.Join(dir, "deploy.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(dir, "service.yml"), []byte("apiVersion: v1\nkind: Service\nmetadata:\n  name: test\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		fSys := filesys.MakeFsOnDisk()
		if err := ensureKustomization(fSys, dir); err != nil {
			t.Fatalf("ensureKustomization() error = %v", err)
		}

		data, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml")) //nolint:gosec // test reads from controlled temp dir
		if err != nil {
			t.Fatalf("reading generated kustomization.yaml: %v", err)
		}

		content := string(data)
		if !strings.Contains(content, "deploy.yaml") {
			t.Error("kustomization.yaml should reference deploy.yaml")
		}

		if !strings.Contains(content, "service.yml") {
			t.Error("kustomization.yaml should reference service.yml")
		}
	})

	t.Run("skips when kustomization.yaml already exists", func(t *testing.T) {
		dir := t.TempDir()

		existing := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n"
		if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(existing), 0o600); err != nil {
			t.Fatal(err)
		}

		fSys := filesys.MakeFsOnDisk()
		if err := ensureKustomization(fSys, dir); err != nil {
			t.Fatalf("ensureKustomization() error = %v", err)
		}

		data, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml")) //nolint:gosec // test reads from controlled temp dir
		if err != nil {
			t.Fatal(err)
		}

		if string(data) != existing {
			t.Error("existing kustomization.yaml should not be modified")
		}
	})
}

func TestRenderKustomizeMultipleResources(t *testing.T) {
	dir := t.TempDir()

	deployYAML := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ctrl
  template:
    metadata:
      labels:
        app: ctrl
    spec:
      containers:
      - name: mgr
        image: ctrl:v1
`
	saYAML := `apiVersion: v1
kind: ServiceAccount
metadata:
  name: controller-sa
`
	crYAML := `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: controller-role
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get"]
`

	for name, content := range map[string]string{
		"deployment.yaml":     deployYAML,
		"serviceaccount.yaml": saYAML,
		"clusterrole.yaml":    crYAML,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	kustomizationYAML := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- deployment.yaml
- serviceaccount.yaml
- clusterrole.yaml
`
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(kustomizationYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	objects, err := renderKustomize(dir, map[string]string{})
	if err != nil {
		t.Fatalf("renderKustomize() error = %v", err)
	}

	if len(objects) != 3 {
		t.Fatalf("expected 3 objects, got %d", len(objects))
	}

	kinds := make([]string, 0, len(objects))
	for _, obj := range objects {
		kinds = append(kinds, obj.GetKind())
	}

	sort.Strings(kinds)

	expected := []string{"ClusterRole", "Deployment", "ServiceAccount"}
	for i, k := range expected {
		if kinds[i] != k {
			t.Errorf("expected kind[%d]=%s, got %s", i, k, kinds[i])
		}
	}
}

func TestRenderKustomizeFromReadOnlySource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only directory semantics do not apply to root")
	}

	srcDir := t.TempDir()

	deployYAML := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: readonly-test
spec:
  replicas: 1
  selector:
    matchLabels:
      app: test
  template:
    metadata:
      labels:
        app: test
    spec:
      containers:
      - name: mgr
        image: test:latest
`
	if err := os.WriteFile(filepath.Join(srcDir, "deployment.yaml"), []byte(deployYAML), 0o400); err != nil {
		t.Fatal(err)
	}

	kustomizationYAML := `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- deployment.yaml
`
	if err := os.WriteFile(filepath.Join(srcDir, "kustomization.yaml"), []byte(kustomizationYAML), 0o400); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(srcDir, 0o500); err != nil { //nolint:gosec // read+execute is intentional to simulate read-only /opt/manifests
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = os.Chmod(srcDir, 0o700) //nolint:gosec // restore permissions for t.TempDir cleanup
	})

	_, err := renderKustomize(srcDir, map[string]string{paramSectionTitle: "Test"})
	if err == nil {
		t.Fatal("expected error when rendering directly into read-only directory")
	}

	renderDir := t.TempDir()

	cpErr := copyDir(srcDir, renderDir)
	if cpErr != nil {
		t.Fatalf("copyDir() from read-only source failed: %v", cpErr)
	}

	objects, err := renderKustomize(renderDir, map[string]string{paramSectionTitle: "Test"})
	if err != nil {
		t.Fatalf("renderKustomize() on copied dir error = %v", err)
	}

	if len(objects) != 1 {
		t.Fatalf("expected 1 object, got %d", len(objects))
	}

	if objects[0].GetName() != "readonly-test" {
		t.Errorf("expected name readonly-test, got %s", objects[0].GetName())
	}
}

func TestCopyDir(t *testing.T) {
	srcDir := t.TempDir()

	subDir := filepath.Join(srcDir, "sub")
	if err := os.MkdirAll(subDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(srcDir, "root.yaml"), []byte("root"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(subDir, "nested.yaml"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}

	dstDir := filepath.Join(t.TempDir(), "copy")
	if err := copyDir(srcDir, dstDir); err != nil {
		t.Fatalf("copyDir() error = %v", err)
	}

	rootData, err := os.ReadFile(filepath.Join(dstDir, "root.yaml")) //nolint:gosec // test reads from controlled temp dir
	if err != nil {
		t.Fatalf("reading copied root.yaml: %v", err)
	}

	if string(rootData) != "root" {
		t.Errorf("expected 'root', got %q", string(rootData))
	}

	nestedData, err := os.ReadFile(filepath.Join(dstDir, "sub", "nested.yaml")) //nolint:gosec // test reads from controlled temp dir
	if err != nil {
		t.Fatalf("reading copied nested.yaml: %v", err)
	}

	if string(nestedData) != "nested" {
		t.Errorf("expected 'nested', got %q", string(nestedData))
	}
}

// TestRenderRealManifests validates that the manifest groups for every platform
// point to directories that exist and can be rendered by kustomize. It runs
// against the committed manifests in opt/manifests/, which must be present in
// the repository (see opt/README.md).
func TestRenderRealManifests(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	basePath := filepath.Join(repoRoot, "opt", "manifests")

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		t.Fatalf("opt/manifests not found — run 'make manifests-fetch' and commit the result")
	}

	platforms := []string{
		platform.OpenDataHub,
		platform.SelfManagedRhoai,
		"",
	}

	params := map[string]string{
		paramSectionTitle:  "Test",
		paramMLflowEnabled: "false",
		paramGatewayURL:    "",
	}

	for _, p := range platforms {
		name := p
		if name == "" {
			name = "empty"
		}

		for _, v2Managed := range []bool{false, true} {
			testName := name
			if v2Managed {
				testName += "-workbenchesV2Managed"
			}

			t.Run(testName, func(t *testing.T) {
				groups := manifestGroupsForPlatform(p, v2Managed)

				workDir := t.TempDir()
				srcRoot := filepath.Join(basePath, "workbenches")
				dstRoot := filepath.Join(workDir, "workbenches")

				if err := copyDir(srcRoot, dstRoot); err != nil {
					t.Fatalf("copyDir() failed: %v", err)
				}

				for _, group := range groups {
					t.Run(filepath.Base(group), func(t *testing.T) {
						srcDir := filepath.Join(basePath, group)

						if _, statErr := os.Stat(srcDir); os.IsNotExist(statErr) {
							t.Fatalf("manifest group directory does not exist: %s", srcDir)
						}

						renderDir := filepath.Join(workDir, group)

						objects, err := renderKustomize(renderDir, params)
						if err != nil {
							t.Fatalf("renderKustomize(%s) failed: %v", group, err)
						}

						if len(objects) == 0 {
							t.Errorf("renderKustomize(%s) produced 0 objects", group)
						}

						t.Logf("rendered %d objects", len(objects))
					})
				}
			})
		}
	}
}

// TestComponentLabelsConsistencyInRealManifests renders the real upstream manifests,
// applies setComponentLabels, and verifies that component labels are consistent
// across all selector-related fields (Deployment matchLabels, template labels,
// Service selectors) so Services can select their pods.
func TestComponentLabelsConsistencyInRealManifests(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	basePath := filepath.Join(repoRoot, "opt", "manifests")

	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		t.Fatalf("opt/manifests not found — run 'make manifests-fetch' and commit the result")
	}

	params := map[string]string{
		paramSectionTitle:  "Test",
		paramMLflowEnabled: "false",
		paramGatewayURL:    "",
	}

	for _, p := range []string{platform.OpenDataHub, platform.SelfManagedRhoai} {
		t.Run(p, func(t *testing.T) {
			groups := manifestGroupsForPlatform(p, false)

			workDir := t.TempDir()
			if err := copyDir(filepath.Join(basePath, "workbenches"), filepath.Join(workDir, "workbenches")); err != nil {
				t.Fatalf("copyDir() failed: %v", err)
			}

			for _, group := range groups {
				renderDir := filepath.Join(workDir, group)

				objects, err := renderKustomize(renderDir, params)
				if err != nil {
					t.Fatalf("renderKustomize(%s) failed: %v", group, err)
				}

				for _, obj := range objects {
					setComponentLabels(obj)

					kind := obj.GetKind()
					name := obj.GetName()

					metaLabels := obj.GetLabels()
					if metaLabels[metadata.PartOfLabelKey] != metadata.ComponentLabelValue {
						t.Errorf("%s/%s: metadata.labels[%s] = %q, want %q",
							kind, name, metadata.PartOfLabelKey,
							metaLabels[metadata.PartOfLabelKey], metadata.ComponentLabelValue)
					}

					if metaLabels[metadata.ComponentLabelKey] != metadata.LabelTrue {
						t.Errorf("%s/%s: metadata.labels[%s] = %q, want %q",
							kind, name, metadata.ComponentLabelKey,
							metaLabels[metadata.ComponentLabelKey], metadata.LabelTrue)
					}

					if kind == "Deployment" {
						matchLabels, found, _ := unstructured.NestedStringMap(obj.Object, "spec", "selector", "matchLabels")
						if found {
							if matchLabels[metadata.PartOfLabelKey] != metadata.ComponentLabelValue {
								t.Errorf("%s/%s: spec.selector.matchLabels[%s] = %q, want %q",
									kind, name, metadata.PartOfLabelKey,
									matchLabels[metadata.PartOfLabelKey], metadata.ComponentLabelValue)
							}

							if matchLabels[metadata.ComponentLabelKey] != metadata.LabelTrue {
								t.Errorf("%s/%s: spec.selector.matchLabels[%s] = %q, want %q",
									kind, name, metadata.ComponentLabelKey,
									matchLabels[metadata.ComponentLabelKey], metadata.LabelTrue)
							}
						}

						templateLabels, found, _ := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "labels")
						if found {
							if templateLabels[metadata.PartOfLabelKey] != metadata.ComponentLabelValue {
								t.Errorf("%s/%s: spec.template.metadata.labels[%s] = %q, want %q",
									kind, name, metadata.PartOfLabelKey,
									templateLabels[metadata.PartOfLabelKey], metadata.ComponentLabelValue)
							}

							if templateLabels[metadata.ComponentLabelKey] != metadata.LabelTrue {
								t.Errorf("%s/%s: spec.template.metadata.labels[%s] = %q, want %q",
									kind, name, metadata.ComponentLabelKey,
									templateLabels[metadata.ComponentLabelKey], metadata.LabelTrue)
							}
						}
					}

					if kind == "Service" {
						svcSelector, found, _ := unstructured.NestedStringMap(obj.Object, "spec", "selector")
						if found {
							if svcSelector[metadata.PartOfLabelKey] != metadata.ComponentLabelValue {
								t.Errorf("%s/%s: spec.selector[%s] = %q, want %q",
									kind, name, metadata.PartOfLabelKey,
									svcSelector[metadata.PartOfLabelKey], metadata.ComponentLabelValue)
							}

							if svcSelector[metadata.ComponentLabelKey] != metadata.LabelTrue {
								t.Errorf("%s/%s: spec.selector[%s] = %q, want %q",
									kind, name, metadata.ComponentLabelKey,
									svcSelector[metadata.ComponentLabelKey], metadata.LabelTrue)
							}
						}
					}
				}
			}
		})
	}
}

func TestGcOrphanedResourcesDeletesOrphans(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(componentsv1alpha1.AddToScheme(scheme))

	namespace := "test-gc-orphans"
	componentLabels := map[string]string{
		metadata.ComponentLabelKey: metadata.LabelTrue,
		metadata.PartOfLabelKey:    metadata.ComponentLabelValue,
	}

	desiredDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "notebook-controller",
			Namespace: namespace,
			Labels:    componentLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "notebook-controller"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "notebook-controller"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
			},
		},
	}

	orphanDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "legacy-notebook-controller",
			Namespace: namespace,
			Labels:    componentLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "legacy"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "legacy"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
			},
		},
	}

	unlabeledDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-deployment",
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "other"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(desiredDeploy, orphanDeploy, unlabeledDeploy).
		Build()

	reconciler := &WorkbenchesReconciler{Client: fakeClient, Scheme: scheme}

	desired := map[objectRef]struct{}{
		{
			gvk:       schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
			namespace: namespace,
			name:      "notebook-controller",
		}: {},
	}

	if err := reconciler.gcOrphanedResources(context.Background(), namespace, desired); err != nil {
		t.Fatalf("gcOrphanedResources() error = %v", err)
	}

	got := &appsv1.DeploymentList{}
	if err := fakeClient.List(context.Background(), got, client.InNamespace(namespace)); err != nil {
		t.Fatalf("list deployments: %v", err)
	}

	names := map[string]bool{}
	for i := range got.Items {
		names[got.Items[i].Name] = true
	}

	if !names["notebook-controller"] {
		t.Error("desired deployment was deleted")
	}
	if names["legacy-notebook-controller"] {
		t.Error("orphaned labeled deployment was not garbage-collected")
	}
	if !names["other-deployment"] {
		t.Error("unlabeled deployment should be left alone")
	}
}

func TestGcOrphanedResourcesDeletesClusterScopedOrphans(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(rbacv1.AddToScheme(scheme))
	utilruntime.Must(componentsv1alpha1.AddToScheme(scheme))

	namespace := "test-gc-cluster-orphans"
	componentLabels := map[string]string{
		metadata.ComponentLabelKey: metadata.LabelTrue,
		metadata.PartOfLabelKey:    metadata.ComponentLabelValue,
	}

	desiredRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "workbenches-desired",
			Labels: componentLabels,
		},
	}
	orphanRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "workbenches-orphan",
			Labels: componentLabels,
		},
	}
	unlabeledRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "other-clusterrole"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(desiredRole, orphanRole, unlabeledRole).
		Build()

	reconciler := &WorkbenchesReconciler{Client: fakeClient, Scheme: scheme}
	desired := map[objectRef]struct{}{
		{
			gvk:  schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"},
			name: "workbenches-desired",
		}: {},
	}

	if err := reconciler.gcOrphanedResources(context.Background(), namespace, desired); err != nil {
		t.Fatalf("gcOrphanedResources() error = %v", err)
	}

	got := &rbacv1.ClusterRoleList{}
	if err := fakeClient.List(context.Background(), got); err != nil {
		t.Fatalf("list clusterroles: %v", err)
	}

	names := map[string]bool{}
	for i := range got.Items {
		names[got.Items[i].Name] = true
	}

	if !names["workbenches-desired"] {
		t.Error("desired ClusterRole was deleted")
	}
	if names["workbenches-orphan"] {
		t.Error("orphaned labeled ClusterRole was not garbage-collected")
	}
	if !names["other-clusterrole"] {
		t.Error("unlabeled ClusterRole should be left alone")
	}
}

func TestGcOrphanedResourcesSkipsWhenDesiredEmpty(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))

	namespace := "test-gc-empty"
	orphan := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "legacy-notebook-controller",
			Namespace: namespace,
			Labels: map[string]string{
				metadata.ComponentLabelKey: metadata.LabelTrue,
				metadata.PartOfLabelKey:    metadata.ComponentLabelValue,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "legacy"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "legacy"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(orphan).Build()
	reconciler := &WorkbenchesReconciler{Client: fakeClient, Scheme: scheme}

	if err := reconciler.gcOrphanedResources(context.Background(), namespace, nil); err != nil {
		t.Fatalf("gcOrphanedResources() error = %v", err)
	}

	got := &appsv1.Deployment{}
	if err := fakeClient.Get(context.Background(), client.ObjectKey{
		Namespace: namespace,
		Name:      "legacy-notebook-controller",
	}, got); err != nil {
		t.Fatalf("expected orphan to remain when desired is empty, get error = %v", err)
	}
}

func TestCleanupManagedResources(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))

	namespace := "test-cleanup-ns"

	labeledDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "notebook-controller",
			Namespace: namespace,
			Labels: map[string]string{
				metadata.ComponentLabelKey: metadata.LabelTrue,
				metadata.PartOfLabelKey:    metadata.ComponentLabelValue,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "notebook-controller"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "notebook-controller"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
			},
		},
	}

	unlabeledDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-deployment",
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "other"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "other"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
			},
		},
	}

	labeledSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "notebook-svc",
			Namespace: namespace,
			Labels: map[string]string{
				metadata.ComponentLabelKey: metadata.LabelTrue,
				metadata.PartOfLabelKey:    metadata.ComponentLabelValue,
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Port: 80}},
		},
	}

	labeledSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "notebook-secret",
			Namespace: namespace,
			Labels: map[string]string{
				metadata.ComponentLabelKey: metadata.LabelTrue,
				metadata.PartOfLabelKey:    metadata.ComponentLabelValue,
			},
		},
		Data: map[string][]byte{"key": []byte("value")},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(labeledDeploy, unlabeledDeploy, labeledSvc, labeledSecret).
		Build()

	reconciler := &WorkbenchesReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	ctx := context.Background()

	if err := reconciler.cleanupManagedResources(ctx, namespace); err != nil {
		t.Fatalf("cleanupManagedResources() error = %v", err)
	}

	// Labeled deployment should be gone
	deployList := &appsv1.DeploymentList{}
	if err := fakeClient.List(ctx, deployList, client.InNamespace(namespace)); err != nil {
		t.Fatalf("failed to list deployments: %v", err)
	}

	if len(deployList.Items) != 1 {
		t.Errorf("expected 1 remaining deployment, got %d", len(deployList.Items))
	}

	if len(deployList.Items) > 0 && deployList.Items[0].Name != "other-deployment" {
		t.Errorf("expected remaining deployment to be 'other-deployment', got %q", deployList.Items[0].Name)
	}

	// Labeled service should be gone
	svcList := &corev1.ServiceList{}
	if err := fakeClient.List(ctx, svcList, client.InNamespace(namespace)); err != nil {
		t.Fatalf("failed to list services: %v", err)
	}

	if len(svcList.Items) != 0 {
		t.Errorf("expected 0 services, got %d", len(svcList.Items))
	}

	// Labeled secret should be gone (owned + in cleanupGVKs, matching ODH)
	secretList := &corev1.SecretList{}
	if err := fakeClient.List(ctx, secretList, client.InNamespace(namespace)); err != nil {
		t.Fatalf("failed to list secrets: %v", err)
	}

	if len(secretList.Items) != 0 {
		t.Errorf("expected 0 secrets, got %d", len(secretList.Items))
	}
}

func TestReconcileDeleteReturnsErrorWhenCleanupFails(t *testing.T) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1.AddToScheme(scheme))
	utilruntime.Must(componentsv1alpha1.AddToScheme(scheme))

	namespace := "test-delete-cleanup-fail"
	deleteErr := errSimulatedDeleteFailure

	labeledDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "notebook-controller",
			Namespace: namespace,
			Labels: map[string]string{
				metadata.ComponentLabelKey: metadata.LabelTrue,
				metadata.PartOfLabelKey:    metadata.ComponentLabelValue,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "notebook-controller"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "notebook-controller"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(labeledDeploy).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				return deleteErr
			},
		}).
		Build()

	reconciler := &WorkbenchesReconciler{
		Client:                fakeClient,
		Scheme:                scheme,
		ApplicationsNamespace: namespace,
	}

	wb := &componentsv1alpha1.Workbenches{
		ObjectMeta: metav1.ObjectMeta{
			Name: componentsv1alpha1.WorkbenchesInstanceName,
		},
		Spec: componentsv1alpha1.WorkbenchesSpec{
			ManagementState:    "Managed",
			WorkbenchNamespace: namespace,
			Platform:           "OpenDataHub",
		},
	}
	controllerutil.AddFinalizer(wb, workbenchesFinalizer)

	ctx := context.Background()

	result, err := reconciler.reconcileDelete(ctx, wb)
	if err == nil {
		t.Fatal("expected reconcileDelete to return an error when cleanup fails, got nil")
	}

	if !strings.Contains(err.Error(), "simulated delete failure") {
		t.Errorf("expected error to contain 'simulated delete failure', got: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Error("expected no explicit requeue on error")
	}

	if !controllerutil.ContainsFinalizer(wb, workbenchesFinalizer) {
		t.Error("finalizer should not be removed when cleanup fails")
	}
}

func TestPatchKustomizeNamespace(t *testing.T) {
	t.Run("replaces existing namespace", func(t *testing.T) {
		dir := t.TempDir()
		content := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: opendatahub\nresources:\n- deploy.yaml\n"

		if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := patchKustomizeNamespace(dir, "my-custom-ns", logr.Discard()); err != nil {
			t.Fatalf("patchKustomizeNamespace() error = %v", err)
		}

		data, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml")) //nolint:gosec // test reads from controlled temp dir
		if err != nil {
			t.Fatal(err)
		}

		got := string(data)
		if !strings.Contains(got, "namespace: my-custom-ns") {
			t.Errorf("expected 'namespace: my-custom-ns', got:\n%s", got)
		}

		if strings.Contains(got, "opendatahub") {
			t.Error("old namespace 'opendatahub' should have been replaced")
		}

		if !strings.Contains(got, "resources:") {
			t.Error("non-namespace content should be preserved")
		}
	})

	t.Run("adds namespace when missing", func(t *testing.T) {
		dir := t.TempDir()
		content := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- deploy.yaml\n"

		if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := patchKustomizeNamespace(dir, "added-ns", logr.Discard()); err != nil {
			t.Fatalf("patchKustomizeNamespace() error = %v", err)
		}

		data, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml")) //nolint:gosec // test reads from controlled temp dir
		if err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(string(data), "namespace: added-ns") {
			t.Errorf("expected 'namespace: added-ns' to be appended, got:\n%s", string(data))
		}
	})

	t.Run("returns error when directory has no kustomization file", func(t *testing.T) {
		dir := t.TempDir()

		err := patchKustomizeNamespace(dir, "whatever", logr.Discard())
		if err == nil {
			t.Fatal("patchKustomizeNamespace() should return error when no kustomization file exists")
		}

		if !strings.Contains(err.Error(), "no kustomization file found") {
			t.Fatalf("unexpected error message: %v", err)
		}
	})

	t.Run("works with kustomization.yml variant", func(t *testing.T) {
		dir := t.TempDir()
		content := "namespace: old-ns\nresources:\n- svc.yaml\n"

		if err := os.WriteFile(filepath.Join(dir, "kustomization.yml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := patchKustomizeNamespace(dir, "new-ns", logr.Discard()); err != nil {
			t.Fatalf("patchKustomizeNamespace() error = %v", err)
		}

		data, err := os.ReadFile(filepath.Join(dir, "kustomization.yml")) //nolint:gosec // test reads from controlled temp dir
		if err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(string(data), "namespace: new-ns") {
			t.Errorf("expected 'namespace: new-ns', got:\n%s", string(data))
		}
	})

	t.Run("does not modify nested namespace fields", func(t *testing.T) {
		dir := t.TempDir()
		content := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" +
			"resources:\n- ../default\nreplacements:\n- source:\n    kind: ConfigMap\n" +
			"  targets:\n  - select:\n      namespace: system\n      kind: Deployment\n"

		if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := patchKustomizeNamespace(dir, "my-ns", logr.Discard()); err != nil {
			t.Fatalf("patchKustomizeNamespace() error = %v", err)
		}

		data, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml")) //nolint:gosec // test reads from controlled temp dir
		if err != nil {
			t.Fatal(err)
		}

		got := string(data)
		if !strings.Contains(got, "namespace: my-ns") {
			t.Error("top-level namespace should have been added")
		}

		if !strings.Contains(got, "namespace: system") {
			t.Error("nested 'namespace: system' in replacements selector should be preserved")
		}
	})

	t.Run("namespace with newline does not inject extra YAML keys", func(t *testing.T) {
		dir := t.TempDir()
		content := "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" +
			"resources:\n- deploy.yaml\n"

		if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		malicious := "test-ns\nmalicious-key: malicious-value"
		if err := patchKustomizeNamespace(dir, malicious, logr.Discard()); err != nil {
			t.Fatalf("patchKustomizeNamespace() error = %v", err)
		}

		data, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml")) //nolint:gosec // test reads from controlled temp dir
		if err != nil {
			t.Fatal(err)
		}

		var parsed map[string]any
		if err := sigyaml.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("output should be valid YAML: %v", err)
		}

		if _, exists := parsed["malicious-key"]; exists {
			t.Error("YAML injection should not produce extra top-level keys")
		}

		ns, ok := parsed["namespace"].(string)
		if !ok {
			t.Fatal("namespace should be a string")
		}

		if !strings.Contains(ns, "malicious-key") {
			t.Error("the injected payload should be safely contained within the namespace value")
		}
	})
}
