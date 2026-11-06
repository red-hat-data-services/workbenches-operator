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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	componentsv1alpha1 "github.com/opendatahub-io/workbenches-operator/api/v1alpha1"
)

func TestConfigureWorkbenchProbes(t *testing.T) {
	for _, tt := range []struct {
		name   string
		probes *componentsv1alpha1.ODHNotebookControllerProbeSpec
		want   string
	}{
		{name: "configured", probes: &componentsv1alpha1.ODHNotebookControllerProbeSpec{TimeoutSeconds: 5}, want: "5"},
		{name: "one second", probes: &componentsv1alpha1.ODHNotebookControllerProbeSpec{TimeoutSeconds: 1}, want: "1"},
		{name: "omitted", want: "1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cm := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": notebookControllerSettingsConfigMap, "namespace": "applications",
				},
				"data": map[string]any{
					"OTHER_SETTING": "unchanged", workbenchProbeTimeoutKey: "99",
				},
			}}
			unrelated := cm.DeepCopy()
			unrelated.SetName("other-config")
			otherNamespace := cm.DeepCopy()
			otherNamespace.SetNamespace("other-namespace")

			objects, err := configureWorkbenchProbes([]*unstructured.Unstructured{unrelated, otherNamespace, cm}, tt.probes, "applications")
			if err != nil {
				t.Fatal(err)
			}
			if len(objects) != 3 {
				t.Fatalf("got %d objects, want 3 (no duplicate ConfigMap)", len(objects))
			}
			for i, obj := range objects {
				data, _, err := unstructured.NestedStringMap(obj.Object, "data")
				if err != nil {
					t.Fatal(err)
				}
				want := "99"
				if i == 2 {
					want = tt.want
				}
				if data[workbenchProbeTimeoutKey] != want || data["OTHER_SETTING"] != "unchanged" {
					t.Fatalf("object %d data = %v, want timeout %q and unchanged other setting", i, data, want)
				}
			}
		})
		t.Run(tt.name+" without settings ConfigMap", func(t *testing.T) {
			objects, err := configureWorkbenchProbes(nil, tt.probes, "applications")
			if err != nil {
				t.Fatal(err)
			}
			if len(objects) != 1 {
				t.Fatalf("got %d objects, want one settings ConfigMap", len(objects))
			}
			cm := objects[0]
			if cm.GetAPIVersion() != "v1" || cm.GetKind() != "ConfigMap" ||
				cm.GetName() != notebookControllerSettingsConfigMap || cm.GetNamespace() != "applications" {
				t.Fatalf("unexpected settings ConfigMap: %v", cm.Object)
			}
			timeout, found, err := unstructured.NestedString(cm.Object, "data", workbenchProbeTimeoutKey)
			if err != nil || !found || timeout != tt.want {
				t.Fatalf("timeout = %q, found = %v, error = %v; want %q", timeout, found, err, tt.want)
			}
		})
	}
}

func TestValidateProbeTimeout(t *testing.T) {
	for _, timeout := range []int32{-1, 0, 1, 5, 2147483647} {
		err := validateSpec(componentsv1alpha1.WorkbenchesSpec{
			ODHNotebookControllerProbe: &componentsv1alpha1.ODHNotebookControllerProbeSpec{TimeoutSeconds: timeout},
		})
		if (err != nil) != (timeout < 1) {
			t.Errorf("timeout %d: error = %v", timeout, err)
		}
	}
}
