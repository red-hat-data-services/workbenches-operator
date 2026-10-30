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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	componentsv1alpha1 "github.com/opendatahub-io/workbenches-operator/api/v1alpha1"
	"github.com/opendatahub-io/workbenches-operator/internal/metadata"
)

func newIngressContractReconciler(objects ...runtime.Object) *WorkbenchesReconciler {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = componentsv1alpha1.AddToScheme(scheme)
	return &WorkbenchesReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build(), Scheme: scheme}
}

func ingressContractConfigMap(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: NotebookControllerConfigMap, Namespace: "opendatahub"},
		Data:       data,
	}
}

func ingressContractWorkbenches(contract *componentsv1alpha1.IngressContractSpec) *componentsv1alpha1.Workbenches {
	return &componentsv1alpha1.Workbenches{
		Spec: componentsv1alpha1.WorkbenchesSpec{
			Platform:        "OpenDataHub",
			IngressContract: contract,
		},
	}
}

func TestObserveIngressContract(t *testing.T) {
	spec := &componentsv1alpha1.IngressContractSpec{Version: componentsv1alpha1.IngressContractVersionV1, SourceGeneration: 17}
	ctx := context.Background()

	tests := []struct {
		name      string
		objects   []runtime.Object
		contract  *componentsv1alpha1.IngressContractSpec
		wantMsg   string
		wantMatch bool
	}{
		{
			name:     "configmap missing",
			contract: spec,
			wantMsg:  "failed to read",
		},
		{
			name:     "contract key missing",
			objects:  []runtime.Object{ingressContractConfigMap(map[string]string{"ingresses": "[]"})},
			contract: spec,
			wantMsg:  "missing from",
		},
		{
			name:     "contract data malformed",
			objects:  []runtime.Object{ingressContractConfigMap(map[string]string{componentsv1alpha1.IngressContractConfigMapKey: `{"version":`})},
			contract: spec,
			wantMsg:  "malformed",
		},
		{
			name: "source generation mismatch",
			objects: []runtime.Object{ingressContractConfigMap(map[string]string{
				componentsv1alpha1.IngressContractConfigMapKey: `{"version":"v1","sourceGeneration":999}`,
			})},
			contract: spec,
			wantMsg:  "does not match spec",
		},
		{
			name: "matching projection",
			objects: []runtime.Object{ingressContractConfigMap(map[string]string{
				componentsv1alpha1.IngressContractConfigMapKey: `{"version":"v1","sourceGeneration":17}`,
			})},
			contract:  spec,
			wantMatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newIngressContractReconciler(tt.objects...)
			wb := ingressContractWorkbenches(tt.contract)

			observed, msg := r.observeIngressContract(ctx, wb)

			if tt.wantMatch {
				if msg != "" {
					t.Fatalf("observeIngressContract() message = %q, want empty", msg)
				}
				if observed != *tt.contract {
					t.Fatalf("observeIngressContract() = %+v, want %+v", observed, *tt.contract)
				}
				return
			}
			if msg == "" {
				t.Fatal("observeIngressContract() message empty, want error message")
			}
			if !strings.Contains(msg, tt.wantMsg) {
				t.Fatalf("observeIngressContract() message = %q, want substring %q", msg, tt.wantMsg)
			}
		})
	}
}

func TestSyncIngressContractStatusNoContract(t *testing.T) {
	r := newIngressContractReconciler()
	wb := ingressContractWorkbenches(nil)
	wb.Generation = 3

	r.syncIngressContractStatus(context.Background(), wb)

	if len(wb.Status.Conditions) != 1 {
		t.Fatalf("expected exactly one condition, got %d", len(wb.Status.Conditions))
	}
	cond := wb.Status.Conditions[0]
	if cond.Type != conditionTypeIngressContractReady || cond.Status != metav1.ConditionFalse || cond.Reason != "IngressContractNotProjected" {
		t.Fatalf("unexpected condition: %+v", cond)
	}
	if wb.Status.IngressContract != (componentsv1alpha1.IngressContractStatus{}) {
		t.Fatalf("expected cleared status, got %+v", wb.Status.IngressContract)
	}
}

func unstructuredDeployment(name string, labels map[string]string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("apps/v1")
	obj.SetKind("Deployment")
	obj.SetName(name)
	obj.SetNamespace("opendatahub")
	obj.SetLabels(labels)
	_ = unstructured.SetNestedField(obj.Object, map[string]any{}, "spec", "template", "metadata")
	return obj
}

func podTemplateAnnotation(t *testing.T, obj *unstructured.Unstructured) any {
	t.Helper()
	value, found, err := unstructured.NestedMap(obj.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		t.Fatalf("failed to read pod template annotations: %v", err)
	}
	if !found {
		return nil
	}
	return value[metadata.IngressContractAnnotationKey]
}

func TestStampIngressContract(t *testing.T) {
	contract := &componentsv1alpha1.IngressContractSpec{Version: componentsv1alpha1.IngressContractVersionV1, SourceGeneration: 9}

	notebookController := unstructuredDeployment("odh-notebook-controller-manager", map[string]string{
		metadata.ComponentNameLabelKey: "odh-notebook-controller",
	})
	kfNotebookController := unstructuredDeployment("kf-notebook-controller-manager", map[string]string{
		metadata.ComponentNameLabelKey: "kf-notebook-controller",
	})
	otherComponent := unstructuredDeployment("webhook", map[string]string{
		metadata.ComponentNameLabelKey: "workbenches-webhook",
	})
	noLabel := unstructuredDeployment("unlabeled", nil)
	configMap := &unstructured.Unstructured{}
	configMap.SetAPIVersion("v1")
	configMap.SetKind("ConfigMap")
	configMap.SetName("cfg")

	objects := []*unstructured.Unstructured{notebookController, kfNotebookController, otherComponent, noLabel, configMap}

	if err := stampIngressContract(objects, contract); err != nil {
		t.Fatalf("stampIngressContract() error = %v", err)
	}

	for _, obj := range []*unstructured.Unstructured{notebookController, kfNotebookController} {
		if got := podTemplateAnnotation(t, obj); got != "v1:9" {
			t.Fatalf("deployment %s stamp = %v, want v1:9", obj.GetName(), got)
		}
	}
	for _, obj := range []*unstructured.Unstructured{otherComponent, noLabel} {
		if got := podTemplateAnnotation(t, obj); got != nil {
			t.Fatalf("deployment %s stamp = %v, want unset", obj.GetName(), got)
		}
	}

	// A nil contract leaves annotations untouched: the stale stamp is inert
	// (the rollout gate requires an exact match while a contract exists), and
	// a null write would coerce to "" on the typed annotations map.
	if err := stampIngressContract(objects, nil); err != nil {
		t.Fatalf("stampIngressContract() error = %v", err)
	}
	if got := podTemplateAnnotation(t, notebookController); got != "v1:9" {
		t.Fatalf("deployment stamp after nil contract = %v, want unchanged v1:9", got)
	}
}

var (
	replicasOne  = int32(1)
	replicasZero = int32(0)
	staleStamp   = "v1:9"
)

// rolloutGateDeploymentSpec describes one deployment for the rollout gate
// table test. Named fields keep each scenario explicit; every value is a
// deliberate scenario input, not a default. A nil stamp leaves the annotation
// absent, and nil replicas exercises the desired=1 default.
type rolloutGateDeploymentSpec struct {
	name       string
	component  string
	replicas   *int32
	stamp      *string
	generation int64
	observed   int64
	updated    int32
	ready      int32
	total      int32
}

func (s rolloutGateDeploymentSpec) build() *appsv1.Deployment {
	annotations := map[string]string{}
	if s.stamp != nil {
		annotations[metadata.IngressContractAnnotationKey] = *s.stamp
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       s.name,
			Namespace:  "opendatahub",
			Generation: s.generation,
			Labels: map[string]string{
				metadata.ComponentNameLabelKey: s.component,
				metadata.ComponentLabelKey:     metadata.LabelTrue,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: s.replicas,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: annotations},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: s.observed,
			UpdatedReplicas:    s.updated,
			ReadyReplicas:      s.ready,
			Replicas:           s.total,
		},
	}
}

func TestIngressContractRolloutGeneration(t *testing.T) {
	contract := &componentsv1alpha1.IngressContractSpec{Version: componentsv1alpha1.IngressContractVersionV1, SourceGeneration: 17}
	expectedStamp := "v1:17"
	ctx := context.Background()

	// rolled returns a deployment of the given component that has fully
	// rolled out at the given generation with the current contract stamp.
	rolled := func(component string, generation int64) rolloutGateDeploymentSpec {
		return rolloutGateDeploymentSpec{
			name:       component + "-manager",
			component:  component,
			replicas:   &replicasOne,
			stamp:      &expectedStamp,
			generation: generation,
			observed:   generation,
			updated:    1,
			ready:      1,
			total:      1,
		}
	}

	odh := rolloutGateDeploymentSpec{name: "odh-notebook-controller-manager", component: "odh-notebook-controller"}
	kf := rolloutGateDeploymentSpec{name: "kf-notebook-controller-manager", component: "kf-notebook-controller"}

	tests := []struct {
		name        string
		deployments []rolloutGateDeploymentSpec
		wantGen     int64
		wantMsg     string
	}{
		{
			name:    "no deployments found",
			wantMsg: "no notebook controller deployment found for component odh-notebook-controller",
		},
		{
			name: "unrelated component deployments are not notebook controllers",
			deployments: []rolloutGateDeploymentSpec{
				{name: "model-mesh", component: "model-mesh", replicas: &replicasZero, generation: 2, observed: 2},
			},
			wantMsg: "no notebook controller deployment found for component odh-notebook-controller",
		},
		{
			name: "scaled to zero",
			deployments: []rolloutGateDeploymentSpec{
				{
					name: odh.name, component: odh.component,
					replicas: &replicasZero, stamp: &expectedStamp,
					generation: 1, observed: 1,
				},
			},
			wantMsg: "scaled to zero",
		},
		{
			name: "not stamped",
			deployments: []rolloutGateDeploymentSpec{
				{
					name: odh.name, component: odh.component,
					replicas:   &replicasOne,
					generation: 2, observed: 2, updated: 1, ready: 1, total: 1,
				},
			},
			wantMsg: "not yet carrying",
		},
		{
			name: "stale stamp mismatch",
			deployments: []rolloutGateDeploymentSpec{
				{
					name: odh.name, component: odh.component,
					replicas: &replicasOne, stamp: &staleStamp,
					generation: 2, observed: 2, updated: 1, ready: 1, total: 1,
				},
			},
			wantMsg: "not yet carrying",
		},
		{
			name: "observed generation lag",
			deployments: []rolloutGateDeploymentSpec{
				{
					name: odh.name, component: odh.component,
					replicas: &replicasOne, stamp: &expectedStamp,
					generation: 3, observed: 2, updated: 1, ready: 1, total: 1,
				},
			},
			wantMsg: "not observed by controller",
		},
		{
			name: "updated replicas below desired",
			deployments: []rolloutGateDeploymentSpec{
				{
					name: odh.name, component: odh.component,
					replicas: &replicasOne, stamp: &expectedStamp,
					generation: 3, observed: 3, updated: 0, ready: 1, total: 1,
				},
			},
			wantMsg: "rollout in progress",
		},
		{
			name: "ready replicas below desired",
			deployments: []rolloutGateDeploymentSpec{
				{
					name: odh.name, component: odh.component,
					replicas: &replicasOne, stamp: &expectedStamp,
					generation: 3, observed: 3, updated: 1, ready: 0, total: 1,
				},
			},
			wantMsg: "rollout in progress",
		},
		{
			name: "surge rollout with total above desired",
			deployments: []rolloutGateDeploymentSpec{
				{
					name: odh.name, component: odh.component,
					replicas: &replicasOne, stamp: &expectedStamp,
					generation: 3, observed: 3, updated: 1, ready: 1, total: 2,
				},
			},
			wantMsg: "rollout in progress",
		},
		{
			name: "odh only does not satisfy the gate",
			deployments: []rolloutGateDeploymentSpec{
				rolled("odh-notebook-controller", 4),
			},
			wantMsg: "no notebook controller deployment found for component kf-notebook-controller",
		},
		{
			name: "kf only does not satisfy the gate",
			deployments: []rolloutGateDeploymentSpec{
				rolled("kf-notebook-controller", 7),
			},
			wantMsg: "no notebook controller deployment found for component odh-notebook-controller",
		},
		{
			name: "replicas nil defaults to one and rolls out",
			deployments: []rolloutGateDeploymentSpec{
				{
					name: odh.name, component: odh.component,
					stamp:      &expectedStamp,
					generation: 5, observed: 5, updated: 1, ready: 1, total: 1,
				},
				rolled("kf-notebook-controller", 3),
			},
			wantGen: 5,
		},
		{
			name: "dual odh and kf deployments report max generation",
			deployments: []rolloutGateDeploymentSpec{
				rolled("odh-notebook-controller", 4),
				rolled("kf-notebook-controller", 7),
			},
			wantGen: 7,
		},
		{
			name: "kf rollout pending blocks while unrelated scaled-to-zero deployment is ignored",
			deployments: []rolloutGateDeploymentSpec{
				rolled("odh-notebook-controller", 4),
				{
					name: kf.name, component: kf.component,
					replicas: &replicasOne, stamp: &staleStamp,
					generation: 3, observed: 3, updated: 1, ready: 1, total: 1,
				},
				{name: "model-mesh", component: "model-mesh", replicas: &replicasZero, generation: 2, observed: 2},
			},
			wantMsg: "not yet carrying",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := make([]runtime.Object, 0, len(tt.deployments))
			for _, d := range tt.deployments {
				objects = append(objects, d.build())
			}
			r := newIngressContractReconciler(objects...)
			wb := ingressContractWorkbenches(contract)

			gen, msg := r.ingressContractRolloutGeneration(ctx, wb)

			if tt.wantMsg != "" {
				if msg == "" {
					t.Fatalf("ingressContractRolloutGeneration() message empty, want error message")
				}
				if !strings.Contains(msg, tt.wantMsg) {
					t.Fatalf("ingressContractRolloutGeneration() message = %q, want substring %q", msg, tt.wantMsg)
				}
				if gen != 0 {
					t.Fatalf("ingressContractRolloutGeneration() generation = %d, want 0 on failure", gen)
				}
				return
			}
			if msg != "" {
				t.Fatalf("ingressContractRolloutGeneration() message = %q, want empty", msg)
			}
			if gen != tt.wantGen {
				t.Fatalf("ingressContractRolloutGeneration() generation = %d, want %d", gen, tt.wantGen)
			}
		})
	}
}

func TestIngressContractRolloutGenerationListError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = componentsv1alpha1.AddToScheme(scheme)
	r := &WorkbenchesReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				return errors.New("api server unreachable")
			},
		}).Build(),
		Scheme: scheme,
	}
	wb := ingressContractWorkbenches(&componentsv1alpha1.IngressContractSpec{
		Version: componentsv1alpha1.IngressContractVersionV1, SourceGeneration: 17,
	})

	gen, msg := r.ingressContractRolloutGeneration(context.Background(), wb)

	if gen != 0 {
		t.Fatalf("generation = %d, want 0 on list error", gen)
	}
	if !strings.Contains(msg, "failed to list deployments") {
		t.Fatalf("message = %q, want list failure", msg)
	}
}

func TestSetErrorStatusClearsIngressContractEvidence(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = componentsv1alpha1.AddToScheme(scheme)

	wb := ingressContractWorkbenches(&componentsv1alpha1.IngressContractSpec{
		Version: componentsv1alpha1.IngressContractVersionV1, SourceGeneration: 17,
	})
	wb.Generation = 5
	wb.Status.IngressContract = componentsv1alpha1.IngressContractStatus{
		Version:              componentsv1alpha1.IngressContractVersionV1,
		SourceGeneration:     17,
		DeploymentGeneration: 4,
	}
	meta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{
		Type:               conditionTypeIngressContractReady,
		Status:             metav1.ConditionTrue,
		Reason:             "IngressContractReady",
		ObservedGeneration: 4,
	})

	r := &WorkbenchesReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(&componentsv1alpha1.Workbenches{}).
			WithRuntimeObjects(wb).Build(),
		Scheme: scheme,
	}

	_, _ = r.setErrorStatus(context.Background(), wb, "ManifestApplyFailed", errors.New("apply boom"))

	cond := meta.FindStatusCondition(wb.Status.Conditions, conditionTypeIngressContractReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "IngressContractEvidenceStale" {
		t.Fatalf("IngressContractReady after error = %+v, want False/IngressContractEvidenceStale", cond)
	}
	if cond.ObservedGeneration != 5 {
		t.Fatalf("ObservedGeneration = %d, want 5", cond.ObservedGeneration)
	}
	if wb.Status.IngressContract != (componentsv1alpha1.IngressContractStatus{}) {
		t.Fatalf("status.ingressContract = %+v, want cleared", wb.Status.IngressContract)
	}
}
