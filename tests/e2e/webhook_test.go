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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func registerWebhookTests() {
	Context("Webhook setup", Label("webhook"), func() {
		BeforeAll(func() {
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: webhookTestNamespace},
			}

			err := k8sClient.Create(ctx, ns)
			if err != nil && !errors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
		})

		AfterAll(func() {
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: webhookTestNamespace},
			}
			_ = k8sClient.Delete(ctx, ns)
		})

		registerConnectionInjectionTests()
		registerHardwareProfileTests()
	})
}

func registerConnectionInjectionTests() {
	Context("Connection injection webhook", func() {
		const secretName = "e2e-test-connection"
		const notebookName = "e2e-connection-nb"

		BeforeAll(func() {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: webhookTestNamespace,
					Labels: map[string]string{
						"opendatahub.io/managed": "true",
					},
				},
				StringData: map[string]string{
					"API_KEY": "test-value",
				},
			}

			err := k8sClient.Create(ctx, secret)
			if err != nil && !errors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
		})

		AfterAll(func() {
			nb := newNotebook(notebookName, nil)
			_ = k8sClient.Delete(ctx, nb)

			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: webhookTestNamespace,
				},
			}
			_ = k8sClient.Delete(ctx, secret)
		})

		It("Should inject envFrom when a Notebook references a connection secret", func() {
			nb := newNotebook(notebookName, map[string]string{
				"opendatahub.io/connections": webhookTestNamespace + "/" + secretName,
			})

			Expect(k8sClient.Create(ctx, nb)).To(Succeed())

			Eventually(func() bool {
				created := &unstructured.Unstructured{}
				created.SetGroupVersionKind(notebookGVK())

				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      notebookName,
					Namespace: webhookTestNamespace,
				}, created)
				if err != nil {
					return false
				}

				containers, found, err := unstructured.NestedSlice(created.Object,
					"spec", "template", "spec", "containers")
				if err != nil || !found || len(containers) == 0 {
					return false
				}

				container, ok := containers[0].(map[string]any)
				if !ok {
					return false
				}

				envFrom, found, err := unstructured.NestedSlice(container, "envFrom")
				if err != nil || !found {
					return false
				}

				for _, ef := range envFrom {
					efMap, ok := ef.(map[string]any)
					if !ok {
						continue
					}

					secretRef, found, err := unstructured.NestedMap(efMap, "secretRef")
					if err != nil || !found {
						continue
					}

					if secretRef["name"] == secretName {
						return true
					}
				}

				return false
			}, webhookTimeout, webhookInterval).Should(BeTrue(),
				"webhook should inject envFrom referencing the connection secret")
		})
	})
}

func registerHardwareProfileTests() {
	Context("Hardware profile injection webhook", func() {
		const hwpName = "e2e-test-hwp"
		const notebookName = "e2e-hwp-nb"

		BeforeAll(func() {
			hwpCRD := &unstructured.Unstructured{}
			hwpCRD.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "apiextensions.k8s.io",
				Version: "v1",
				Kind:    "CustomResourceDefinition",
			})

			Expect(k8sClient.Get(ctx, client.ObjectKey{
				Name: "hardwareprofiles.infrastructure.opendatahub.io",
			}, hwpCRD)).To(Succeed(),
				"HardwareProfile CRD must be installed on the cluster")

			hwp := &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "infrastructure.opendatahub.io/v1",
					"kind":       "HardwareProfile",
					"metadata": map[string]any{
						"name":      hwpName,
						"namespace": webhookTestNamespace,
					},
					"spec": map[string]any{
						"identifiers": []any{
							map[string]any{
								"displayName":  "CPU",
								"identifier":   "cpu",
								"minCount":     int64(1),
								"maxCount":     int64(4),
								"defaultCount": int64(2),
							},
							map[string]any{
								"displayName":  "Memory",
								"identifier":   "memory",
								"minCount":     int64(1),
								"maxCount":     int64(8),
								"defaultCount": int64(4),
							},
						},
						"scheduling": map[string]any{
							"type": "Node",
							"node": map[string]any{
								"nodeSelector": map[string]any{
									"node-role.kubernetes.io/worker": "",
								},
								"tolerations": []any{
									map[string]any{
										"key":      "test-key",
										"operator": "Exists",
										"effect":   "NoSchedule",
									},
								},
							},
						},
					},
				},
			}

			err := k8sClient.Create(ctx, hwp)
			if err != nil && !errors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
		})

		AfterAll(func() {
			nb := newNotebook(notebookName, nil)
			_ = k8sClient.Delete(ctx, nb)

			hwp := &unstructured.Unstructured{}
			hwp.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "infrastructure.opendatahub.io",
				Version: "v1",
				Kind:    "HardwareProfile",
			})
			hwp.SetName(hwpName)
			hwp.SetNamespace(webhookTestNamespace)
			_ = k8sClient.Delete(ctx, hwp)
		})

		It("Should apply hardware profile settings when a Notebook has the HWP annotation", func() {
			nb := newNotebook(notebookName, map[string]string{
				"opendatahub.io/hardware-profile-name": hwpName,
			})

			Expect(k8sClient.Create(ctx, nb)).To(Succeed())

			created := &unstructured.Unstructured{}
			created.SetGroupVersionKind(notebookGVK())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      notebookName,
				Namespace: webhookTestNamespace,
			}, created)).To(Succeed())

			tolerations, found, err := unstructured.NestedSlice(created.Object,
				"spec", "template", "spec", "tolerations")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), "tolerations should be present after HWP injection")
			Expect(tolerations).NotTo(BeEmpty())

			hasTestToleration := false
			for _, t := range tolerations {
				tMap, ok := t.(map[string]any)
				if !ok {
					continue
				}

				if tMap["key"] == "test-key" &&
					tMap["operator"] == "Exists" &&
					tMap["effect"] == "NoSchedule" {
					hasTestToleration = true

					break
				}
			}

			Expect(hasTestToleration).To(BeTrue(),
				"webhook should inject tolerations from the hardware profile")

			nsAnnotation, _, _ := unstructured.NestedString(created.Object,
				"metadata", "annotations", "opendatahub.io/hardware-profile-namespace")
			Expect(nsAnnotation).To(Equal(webhookTestNamespace),
				"webhook should set the hardware-profile-namespace annotation")
		})
	})

	registerKueueTests()
	registerEventEmissionTests()
	registerProfileSwitchTests()
	registerProfileRemovalTests()
}

func registerKueueTests() {
	Context("Kueue LocalQueue label injection", Ordered, func() {
		const kueueHWPName = "e2e-kueue-hwp"
		const kueueNotebookName = "e2e-kueue-nb"
		const queueName = "e2e-test-queue"

		BeforeAll(func() {
			ensureCreated(newHardwareProfile(kueueHWPName, map[string]any{
				"identifiers": []any{
					map[string]any{
						"displayName":  "CPU",
						"identifier":   "cpu",
						"minCount":     int64(1),
						"defaultCount": int64(2),
					},
				},
				"scheduling": map[string]any{
					"type": "Queue",
					"kueue": map[string]any{
						"localQueueName": queueName,
					},
				},
			}))
		})

		AfterAll(func() {
			nb := newNotebook(kueueNotebookName, nil)
			_ = k8sClient.Delete(ctx, nb)

			hwp := &unstructured.Unstructured{}
			hwp.SetGroupVersionKind(hwpGVK())
			hwp.SetName(kueueHWPName)
			hwp.SetNamespace(webhookTestNamespace)
			_ = k8sClient.Delete(ctx, hwp)
		})

		It("Should set kueue.x-k8s.io/queue-name label from HardwareProfile", func() {
			nb := newNotebook(kueueNotebookName, map[string]string{
				"opendatahub.io/hardware-profile-name": kueueHWPName,
			})

			Expect(k8sClient.Create(ctx, nb)).To(Succeed())

			created := &unstructured.Unstructured{}
			created.SetGroupVersionKind(notebookGVK())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      kueueNotebookName,
				Namespace: webhookTestNamespace,
			}, created)).To(Succeed())

			labels := created.GetLabels()
			Expect(labels).To(HaveKeyWithValue("kueue.x-k8s.io/queue-name", queueName),
				"webhook should set the Kueue queue-name label")
		})

		It("Should NOT apply nodeSelector or tolerations when Kueue is configured", func() {
			created := &unstructured.Unstructured{}
			created.SetGroupVersionKind(notebookGVK())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      kueueNotebookName,
				Namespace: webhookTestNamespace,
			}, created)).To(Succeed())

			_, nsFound, _ := unstructured.NestedStringMap(created.Object,
				"spec", "template", "spec", "nodeSelector")
			Expect(nsFound).To(BeFalse(),
				"nodeSelector should NOT be applied when Kueue scheduling is active")

			_, tolFound, _ := unstructured.NestedSlice(created.Object,
				"spec", "template", "spec", "tolerations")
			Expect(tolFound).To(BeFalse(),
				"tolerations should NOT be applied when Kueue scheduling is active")
		})
	})
}

func registerEventEmissionTests() {
	Context("Event emission on container name mismatch", Ordered, func() {
		const eventHWPName = "e2e-event-hwp"
		const eventNotebookName = "e2e-event-nb"

		BeforeAll(func() {
			ensureCreated(newHardwareProfile(eventHWPName, map[string]any{
				"identifiers": []any{
					map[string]any{
						"displayName":  "CPU",
						"identifier":   "cpu",
						"minCount":     int64(1),
						"defaultCount": int64(2),
					},
				},
			}))
		})

		AfterAll(func() {
			nb := &unstructured.Unstructured{}
			nb.SetGroupVersionKind(notebookGVK())
			nb.SetName(eventNotebookName)
			nb.SetNamespace(webhookTestNamespace)
			_ = k8sClient.Delete(ctx, nb)

			hwp := &unstructured.Unstructured{}
			hwp.SetGroupVersionKind(hwpGVK())
			hwp.SetName(eventHWPName)
			hwp.SetNamespace(webhookTestNamespace)
			_ = k8sClient.Delete(ctx, hwp)
		})

		It("Should emit a ContainerNameMismatch Warning event when container name does not match", func() {
			nb := &unstructured.Unstructured{}
			nb.SetGroupVersionKind(notebookGVK())
			nb.SetName(eventNotebookName)
			nb.SetNamespace(webhookTestNamespace)
			nb.SetAnnotations(map[string]string{
				"opendatahub.io/hardware-profile-name": eventHWPName,
			})

			// Multi-container with NO container matching the notebook name
			Expect(unstructured.SetNestedField(nb.Object, map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"name":  "wrong-name",
							"image": "jupyter/minimal-notebook:latest",
						},
						map[string]any{
							"name":  "sidecar",
							"image": "busybox:latest",
						},
					},
				},
			}, "spec", "template")).To(Succeed())

			Expect(k8sClient.Create(ctx, nb)).To(Succeed(),
				"Notebook should still be admitted despite container name mismatch")

			// Verify a ContainerNameMismatch Warning event was emitted
			Eventually(func() bool {
				events := &corev1.EventList{}
				if err := k8sClient.List(ctx, events,
					client.InNamespace(webhookTestNamespace)); err != nil {
					return false
				}

				for i := range events.Items {
					if events.Items[i].Reason == "ContainerNameMismatch" &&
						events.Items[i].Type == corev1.EventTypeWarning &&
						events.Items[i].InvolvedObject.Name == eventNotebookName {
						return true
					}
				}

				return false
			}, webhookTimeout, webhookInterval).Should(BeTrue(),
				"Expected a ContainerNameMismatch Warning event on the Notebook")
		})
	})
}

func registerProfileSwitchTests() {
	Context("Hardware profile switch via UPDATE", Ordered, func() {
		const switchHWP1 = "e2e-switch-hwp1"
		const switchHWP2 = "e2e-switch-hwp2"
		const switchNotebook = "e2e-switch-nb"

		BeforeAll(func() {
			ensureCreated(newHardwareProfile(switchHWP1, map[string]any{
				"identifiers": []any{
					map[string]any{
						"displayName":  "CPU",
						"identifier":   "cpu",
						"minCount":     int64(1),
						"defaultCount": int64(2),
					},
					map[string]any{
						"displayName":  "Memory",
						"identifier":   "memory",
						"minCount":     "1Gi",
						"defaultCount": "4Gi",
					},
				},
				"scheduling": map[string]any{
					"type": "Node",
					"node": map[string]any{
						"nodeSelector": map[string]any{
							"profile": "hwp1",
						},
					},
				},
			}))

			ensureCreated(newHardwareProfile(switchHWP2, map[string]any{
				"identifiers": []any{
					map[string]any{
						"displayName":  "CPU",
						"identifier":   "cpu",
						"minCount":     int64(4),
						"defaultCount": int64(8),
					},
					map[string]any{
						"displayName":  "Memory",
						"identifier":   "memory",
						"minCount":     "8Gi",
						"defaultCount": "16Gi",
					},
				},
				"scheduling": map[string]any{
					"type": "Node",
					"node": map[string]any{
						"nodeSelector": map[string]any{
							"profile": "hwp2",
						},
					},
				},
			}))
		})

		AfterAll(func() {
			nb := newNotebook(switchNotebook, nil)
			_ = k8sClient.Delete(ctx, nb)

			for _, name := range []string{switchHWP1, switchHWP2} {
				hwp := &unstructured.Unstructured{}
				hwp.SetGroupVersionKind(hwpGVK())
				hwp.SetName(name)
				hwp.SetNamespace(webhookTestNamespace)
				_ = k8sClient.Delete(ctx, hwp)
			}
		})

		It("Should apply first profile on creation", func() {
			nb := newNotebook(switchNotebook, map[string]string{
				"opendatahub.io/hardware-profile-name": switchHWP1,
			})

			Expect(k8sClient.Create(ctx, nb)).To(Succeed())

			created := &unstructured.Unstructured{}
			created.SetGroupVersionKind(notebookGVK())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      switchNotebook,
				Namespace: webhookTestNamespace,
			}, created)).To(Succeed())

			nodeSelector, found, _ := unstructured.NestedStringMap(created.Object,
				"spec", "template", "spec", "nodeSelector")
			Expect(found).To(BeTrue())
			Expect(nodeSelector).To(HaveKeyWithValue("profile", "hwp1"))
		})

		It("Should replace scheduling when switching to second profile", func() {
			updateNotebook(switchNotebook, func(existing *unstructured.Unstructured) {
				annotations := existing.GetAnnotations()
				annotations["opendatahub.io/hardware-profile-name"] = switchHWP2
				existing.SetAnnotations(annotations)
			})

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(notebookGVK())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      switchNotebook,
				Namespace: webhookTestNamespace,
			}, updated)).To(Succeed())

			nodeSelector, found, _ := unstructured.NestedStringMap(updated.Object,
				"spec", "template", "spec", "nodeSelector")
			Expect(found).To(BeTrue())
			Expect(nodeSelector).To(HaveKeyWithValue("profile", "hwp2"),
				"nodeSelector should reflect the new profile, not the old one")
		})
	})
}

func registerProfileRemovalTests() {
	Context("Hardware profile removal cleanup", Ordered, func() {
		const removalHWP = "e2e-removal-hwp"
		const removalNotebook = "e2e-removal-nb"

		BeforeAll(func() {
			ensureCreated(newHardwareProfile(removalHWP, map[string]any{
				"identifiers": []any{
					map[string]any{
						"displayName":  "CPU",
						"identifier":   "cpu",
						"minCount":     int64(1),
						"defaultCount": int64(2),
					},
				},
				"scheduling": map[string]any{
					"type": "Node",
					"node": map[string]any{
						"nodeSelector": map[string]any{
							"removable-key": "removable-value",
						},
						"tolerations": []any{
							map[string]any{
								"key":      "removable-tol",
								"operator": "Exists",
								"effect":   "NoSchedule",
							},
						},
					},
				},
			}))
		})

		AfterAll(func() {
			nb := newNotebook(removalNotebook, nil)
			_ = k8sClient.Delete(ctx, nb)

			hwp := &unstructured.Unstructured{}
			hwp.SetGroupVersionKind(hwpGVK())
			hwp.SetName(removalHWP)
			hwp.SetNamespace(webhookTestNamespace)
			_ = k8sClient.Delete(ctx, hwp)
		})

		It("Should apply scheduling on creation", func() {
			nb := newNotebook(removalNotebook, map[string]string{
				"opendatahub.io/hardware-profile-name": removalHWP,
			})

			Expect(k8sClient.Create(ctx, nb)).To(Succeed())

			created := &unstructured.Unstructured{}
			created.SetGroupVersionKind(notebookGVK())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      removalNotebook,
				Namespace: webhookTestNamespace,
			}, created)).To(Succeed())

			nodeSelector, found, _ := unstructured.NestedStringMap(created.Object,
				"spec", "template", "spec", "nodeSelector")
			Expect(found).To(BeTrue())
			Expect(nodeSelector).To(HaveKeyWithValue("removable-key", "removable-value"))
		})

		It("Should remove scheduling settings when HWP annotation is removed", func() {
			// Remove the HWP name annotation (keep namespace so removal logic triggers)
			updateNotebook(removalNotebook, func(existing *unstructured.Unstructured) {
				annotations := existing.GetAnnotations()
				delete(annotations, "opendatahub.io/hardware-profile-name")
				existing.SetAnnotations(annotations)
			})

			updated := &unstructured.Unstructured{}
			updated.SetGroupVersionKind(notebookGVK())
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      removalNotebook,
				Namespace: webhookTestNamespace,
			}, updated)).To(Succeed())

			_, nsFound, _ := unstructured.NestedStringMap(updated.Object,
				"spec", "template", "spec", "nodeSelector")
			Expect(nsFound).To(BeFalse(),
				"nodeSelector should be removed after HWP annotation removal")

			_, tolFound, _ := unstructured.NestedSlice(updated.Object,
				"spec", "template", "spec", "tolerations")
			Expect(tolFound).To(BeFalse(),
				"tolerations should be removed after HWP annotation removal")
		})
	})
}
