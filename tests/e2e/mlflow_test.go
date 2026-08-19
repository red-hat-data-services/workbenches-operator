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

package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	componentsv1alpha1 "github.com/opendatahub-io/workbenches-operator/api/v1alpha1"
)

func registerMLflowIntegrationTests() {
	Context("MLflow integration", Label("lifecycle"), func() {
		It("Should default MLFLOW_ENABLED to false on odh-notebook-controller-manager", func() {
			waitForMLflowEnabled("false")
		})

		It("Should set MLFLOW_ENABLED to true when mlflowEnabled is true on the CR", func() {
			updateWorkbenchesSpec(func(wb *componentsv1alpha1.Workbenches) {
				wb.Spec.MLflowEnabled = true
			})

			waitForCondition("Ready", metav1.ConditionTrue)
			waitForMLflowEnabled("true")
		})

		It("Should set MLFLOW_ENABLED back to false when mlflowEnabled is false", func() {
			updateWorkbenchesSpec(func(wb *componentsv1alpha1.Workbenches) {
				wb.Spec.MLflowEnabled = false
			})

			waitForCondition("Ready", metav1.ConditionTrue)
			waitForMLflowEnabled("false")
		})
	})
}
