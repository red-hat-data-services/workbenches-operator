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
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	componentsv1alpha1 "github.com/opendatahub-io/workbenches-operator/api/v1alpha1"
	"github.com/opendatahub-io/workbenches-operator/internal/metadata"
)

func registerLifecycleTests() {
	registerOperatorDeploymentTests()
	registerCELValidationTests()
	registerComponentLifecycleTests()
	registerStatusConditionTests()
	registerCELImmutabilityTests()
	registerReleaseMetadataTests()
	registerImageStreamStatusTests()
	registerMLflowIntegrationTests()
	registerDriftRecoveryTests()
	registerOperandHealthTests()
	registerPlatformConfigTests()
	registerServiceHealthTests()
	registerUpgradeTests()
	registerManagementStateTests()
}

func registerOperatorDeploymentTests() {
	Context("Operator deployment", Label("lifecycle"), func() {
		It("Should have the operator deployment running and ready", func() {
			Eventually(func(g Gomega) {
				deploy := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "workbenches-operator",
					Namespace: operatorNS,
				}, deploy)).To(Succeed(),
					"operator deployment not found in namespace %q — "+
						"deploy the operator first or set OPERATOR_NAMESPACE", operatorNS)

				g.Expect(deploy.Status.ReadyReplicas).To(BeNumerically(">=", 1),
					"operator deployment in namespace %q has no ready replicas", operatorNS)
			}, timeout, interval).Should(Succeed())
		})
	})
}

func registerCELValidationTests() {
	Context("CEL validation", Label("validation"), func() {
		It("Should reject a Workbenches CR with a name other than 'default'", func() {
			wb := &componentsv1alpha1.Workbenches{
				ObjectMeta: metav1.ObjectMeta{
					Name: "not-default",
				},
				Spec: componentsv1alpha1.WorkbenchesSpec{
					ManagementState:    "Managed",
					WorkbenchNamespace: defaultTestLegacyWorkbenchNamespace,
					Platform:           "OpenDataHub",
				},
			}

			err := k8sClient.Create(ctx, wb)
			Expect(err).To(HaveOccurred())
			Expect(errors.IsInvalid(err) || errors.IsForbidden(err)).To(BeTrue(),
				"expected validation error, got: %v", err)
		})
	})
}

func registerComponentLifecycleTests() {
	Context("Component lifecycle", Label("lifecycle"), func() {
		It("Should create a Workbenches CR and reach ProvisioningSucceeded", func() {
			wb := workbenchesCR()

			err := k8sClient.Create(ctx, wb)
			if errors.IsAlreadyExists(err) {
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: wb.Name}, wb)).To(Succeed())
			} else {
				Expect(err).NotTo(HaveOccurred())
			}

			// Operands always deploy into APPLICATIONS_NAMESPACE (opendatahub in e2e).
			operandNamespace = defaultTestApplicationsNamespace

			waitForCondition("ProvisioningSucceeded", metav1.ConditionTrue)

			appsNS := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: defaultTestApplicationsNamespace}, appsNS)).To(Succeed())
			Expect(appsNS.Labels).To(HaveKeyWithValue(metadata.OwnedNamespaceLabel, metadata.LabelTrue))

			legacyNS := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: defaultTestLegacyWorkbenchNamespace}, legacyNS)).To(Succeed())
			Expect(legacyNS.Labels).To(HaveKeyWithValue(metadata.OwnedNamespaceLabel, metadata.LabelTrue))
		})
	})
}

func registerStatusConditionTests() {
	Context("Status conditions completeness", Label("lifecycle"), func() {
		It("Should have all expected status conditions set", func() {
			Eventually(func() int {
				wb := getWorkbenches()

				return len(wb.Status.Conditions)
			}, timeout, interval).Should(BeNumerically(">=", 3))

			wb := getWorkbenches()

			provCond := meta.FindStatusCondition(wb.Status.Conditions, "ProvisioningSucceeded")
			Expect(provCond).NotTo(BeNil())
			Expect(provCond.Status).To(Equal(metav1.ConditionTrue))

			deployCond := meta.FindStatusCondition(wb.Status.Conditions, "DeploymentsAvailable")
			Expect(deployCond).NotTo(BeNil())

			readyCond := meta.FindStatusCondition(wb.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
		})

		It("Should populate status.applicationsNamespace with the operand namespace", func() {
			wb := getWorkbenches()
			Expect(wb.Status.ApplicationsNamespace).To(Equal(operandNamespace))
			Expect(wb.Status.WorkbenchNamespace).To(Equal(defaultTestLegacyWorkbenchNamespace))
		})

		It("Should set observedGeneration to match metadata.generation", func() {
			wb := getWorkbenches()
			Expect(wb.Status.ObservedGeneration).To(Equal(wb.Generation))
		})
	})
}

func registerCELImmutabilityTests() {
	Context("CEL immutability", Label("validation"), func() {
		It("Should reject updates to workbenchNamespace", func() {
			wb := getWorkbenches()

			wb.Spec.WorkbenchNamespace = "different-namespace"
			err := k8sClient.Update(ctx, wb)
			Expect(err).To(HaveOccurred(), "updating workbenchNamespace should be rejected")
		})
	})
}

func registerDriftRecoveryTests() {
	Context("Drift recovery", Label("lifecycle"), func() {
		It("Should recreate deleted Deployments on next reconcile", func() {
			list := &appsv1.DeploymentList{}
			expectDriftRecoveryAll("Deployment", list,
				func() []client.Object {
					items := make([]client.Object, 0, len(list.Items))

					for i := range list.Items {
						items = append(items, list.Items[i].DeepCopy())
					}

					return items
				},
				func() client.Object { return &appsv1.Deployment{} },
				nil,
			)
		})

		It("Should recreate deleted ConfigMaps on next reconcile", func() {
			list := &corev1.ConfigMapList{}
			expectDriftRecoveryAll("ConfigMap", list,
				func() []client.Object {
					items := make([]client.Object, 0, len(list.Items))

					for i := range list.Items {
						items = append(items, list.Items[i].DeepCopy())
					}

					return items
				},
				func() client.Object { return &corev1.ConfigMap{} },
				func(obj client.Object) bool {
					return skipKustomizeGeneratedConfigMap(obj.GetName())
				},
			)
		})

		It("Should recreate deleted Services on next reconcile", func() {
			list := &corev1.ServiceList{}
			expectDriftRecoveryAll("Service", list,
				func() []client.Object {
					items := make([]client.Object, 0, len(list.Items))

					for i := range list.Items {
						items = append(items, list.Items[i].DeepCopy())
					}

					return items
				},
				func() client.Object { return &corev1.Service{} },
				nil,
			)
		})
	})
}

func registerManagementStateTests() {
	Context("Managed to Removed transition", Label("lifecycle"), func() {
		It("Should transition to Failed phase when management state is Removed", func() {
			updateWorkbenchesSpec(func(wb *componentsv1alpha1.Workbenches) {
				wb.Spec.ManagementState = "Removed"
			})

			waitForPhase("Failed")
			waitForCondition("Ready", metav1.ConditionFalse)
			waitForCondition("ProvisioningSucceeded", metav1.ConditionFalse)
		})

		It("Should remove labeled operand deployments when management state is Removed", func() {
			Expect(operandNamespace).NotTo(BeEmpty())

			Eventually(func(g Gomega) {
				deploys := &appsv1.DeploymentList{}
				g.Expect(k8sClient.List(ctx, deploys,
					client.InNamespace(operandNamespace),
					managedResourceLabels(),
				)).To(Succeed())
				g.Expect(deploys.Items).To(BeEmpty(),
					"component-labeled deployments should be removed in Removed state")
			}, timeout, interval).Should(Succeed())
		})

		It("Should remove all managed operand resource types when management state is Removed", func() {
			expectNoManagedOperandResources()
		})

		It("Should clear status.releases when management state is Removed", func() {
			Eventually(func(g Gomega) {
				wb := getWorkbenches()
				g.Expect(wb.Status.Releases).To(BeEmpty())
			}, timeout, interval).Should(Succeed())
		})
	})

	Context("Removed to Managed round-trip", Label("lifecycle"), func() {
		It("Should recover to a healthy state when switching back to Managed", func() {
			updateWorkbenchesSpec(func(wb *componentsv1alpha1.Workbenches) {
				wb.Spec.ManagementState = "Managed"
			})

			waitForCondition("ProvisioningSucceeded", metav1.ConditionTrue)
			waitForPhase("Ready")
		})
	})
}

func registerOperandHealthTests() {
	Context("Operand health", Label("lifecycle"), func() {
		It("Should have the odh-notebook-controller-manager deployment ready", func() {
			Expect(operandNamespace).NotTo(BeEmpty(),
				"operandNamespace must be set by a prior test")

			Eventually(func(g Gomega) {
				deploy := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "odh-notebook-controller-manager",
					Namespace: operandNamespace,
				}, deploy)).To(Succeed(),
					"odh-notebook-controller-manager deployment not found in namespace %q", operandNamespace)

				g.Expect(deploy.Status.ReadyReplicas).To(BeNumerically(">=", 1),
					"odh-notebook-controller-manager in %q has no ready replicas", operandNamespace)
			}, timeout, interval).Should(Succeed())
		})

		It("Should have the notebook-controller-deployment ready", func() {
			Expect(operandNamespace).NotTo(BeEmpty(),
				"operandNamespace must be set by a prior test")

			Eventually(func(g Gomega) {
				deploy := &appsv1.Deployment{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "notebook-controller-deployment",
					Namespace: operandNamespace,
				}, deploy)).To(Succeed(),
					"notebook-controller-deployment not found in namespace %q", operandNamespace)

				g.Expect(deploy.Status.ReadyReplicas).To(BeNumerically(">=", 1),
					"notebook-controller-deployment in %q has no ready replicas", operandNamespace)
			}, timeout, interval).Should(Succeed())
		})
	})
}
