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

package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	componentsv1alpha1 "github.com/opendatahub-io/workbenches-operator/api/v1alpha1"
	"github.com/opendatahub-io/workbenches-operator/internal/controller"
	"github.com/opendatahub-io/workbenches-operator/internal/gvk"
	"github.com/opendatahub-io/workbenches-operator/internal/metadata"
	"github.com/opendatahub-io/workbenches-operator/internal/platformconfig"
	statusutil "github.com/opendatahub-io/workbenches-operator/internal/status"
)

const (
	testNotebookControllerDeployment = "odh-notebook-controller"
	conditionReasonUnknown           = "Unknown"
)

var _ = Describe("Workbenches Controller", func() {
	var (
		reconciler            *controller.WorkbenchesReconciler
		manifestsDir          string
		applicationsNamespace = "opendatahub"
		testPlatformVersion   = "2.20.0"
	)

	BeforeEach(func() {
		var err error
		manifestsDir, err = os.MkdirTemp("", "wb-test-manifests-*")
		Expect(err).NotTo(HaveOccurred())

		kustomizationContent := []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n")
		for _, sub := range []string{
			"workbenches/kf-notebook-controller/overlays/openshift",
			"workbenches/kf-notebook-controller",
			"workbenches/odh-notebook-controller/overlays/odh",
			"workbenches/odh-notebook-controller/overlays/rhoai",
			"workbenches/notebooks/odh/base",
			"workbenches/notebooks/rhoai/base",
		} {
			dir := filepath.Join(manifestsDir, sub)
			Expect(os.MkdirAll(dir, 0o750)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(dir, "kustomization.yaml"), kustomizationContent, 0o600)).To(Succeed())
		}

		metadataContent := []byte(`releases:
  - name: Kubeflow Notebook Controller
    version: 1.10.0
    repoUrl: https://github.com/kubeflow/kubeflow
`)
		Expect(os.WriteFile(
			filepath.Join(manifestsDir, "workbenches/kf-notebook-controller/component_metadata.yaml"),
			metadataContent,
			0o600,
		)).To(Succeed())

		reconciler = &controller.WorkbenchesReconciler{
			Client:                k8sClient,
			Scheme:                scheme.Scheme,
			ManifestsBasePath:     manifestsDir,
			ApplicationsNamespace: applicationsNamespace,
		}
	})

	AfterEach(func() {
		if manifestsDir != "" {
			Expect(os.RemoveAll(manifestsDir)).To(Succeed())
		}
	})

	// Readiness fixtures must be in the desired manifests so continuous GC retains them.
	createDesiredDeployment := func(namespace, name string) {
		deployment := createDeployment(namespace, name)
		data, err := yaml.Marshal(deployment)
		Expect(err).NotTo(HaveOccurred())
		groups := []string{"workbenches/odh-notebook-controller/overlays/odh", "workbenches/odh-notebook-controller/overlays/rhoai"}
		if name == "workspaces-controller" {
			groups = []string{"workbenches/workspaces-controller/overlays/gateway"}
		}
		for _, group := range groups {
			dir := filepath.Join(manifestsDir, group)
			Expect(os.WriteFile(filepath.Join(dir, "deployment.yaml"), data, 0o600)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(dir, "kustomization.yaml"),
				[]byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [deployment.yaml]\n"), 0o600)).To(Succeed())
		}
	}
	Context("Workbench probe configuration", func() {
		It("Should default and validate the probe timeout in the CRD", func() {
			wb := createWorkbenches("Managed", "probe-validation-notebooks", "OpenDataHub")
			DeferCleanup(func() { cleanupWorkbenches(wb) })
			Expect(getWorkbenches(wb.Name).Spec.ODHNotebookControllerProbe).To(BeNil())

			raw := &unstructured.Unstructured{}
			raw.SetAPIVersion("components.platform.opendatahub.io/v1alpha1")
			raw.SetKind("Workbenches")
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: wb.Name}, raw)).To(Succeed())
			Expect(unstructured.SetNestedMap(raw.Object, map[string]any{}, "spec", "odhNotebookControllerProbe")).To(Succeed())
			Expect(k8sClient.Update(ctx, raw)).To(Succeed())
			Expect(getWorkbenches(wb.Name).Spec.ODHNotebookControllerProbe.TimeoutSeconds).To(Equal(int32(1)))

			for _, timeout := range []int64{0, -1, 2147483648} {
				Expect(k8sClient.Get(ctx, client.ObjectKey{Name: wb.Name}, raw)).To(Succeed())
				Expect(unstructured.SetNestedField(raw.Object, timeout, "spec", "odhNotebookControllerProbe", "timeoutSeconds")).To(Succeed())
				Expect(apierrors.IsInvalid(k8sClient.Update(ctx, raw))).To(BeTrue())
			}

			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: wb.Name}, raw)).To(Succeed())
			Expect(unstructured.SetNestedField(raw.Object, int64(2147483647), "spec", "odhNotebookControllerProbe", "timeoutSeconds")).To(Succeed())
			Expect(k8sClient.Update(ctx, raw)).To(Succeed())
		})

		DescribeTable("Should reconcile the timeout from the CR into the runtime ConfigMap",
			func(platformType, namespaceOverride, expectedNamespace string) {
				reconciler.ApplicationsNamespace = namespaceOverride
				// Render another resource alongside the shared settings ConfigMap.
				for _, overlay := range []string{"odh", "rhoai"} {
					dir := filepath.Join(manifestsDir, "workbenches/odh-notebook-controller/overlays", overlay)
					Expect(os.WriteFile(filepath.Join(dir, "configmap.yaml"), []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: probe-test-fixture
data:
  fixture: unchanged
`), 0o600)).To(Succeed())
					Expect(os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- configmap.yaml
`), 0o600)).To(Succeed())
				}
				wb := createWorkbenches("Managed", "probe-policy-notebooks", platformType)
				cm := &corev1.ConfigMap{}
				key := client.ObjectKey{Namespace: expectedNamespace, Name: "notebook-controller-setting-config"}
				DeferCleanup(func() {
					Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
						Name: key.Name, Namespace: key.Namespace,
					}}))).To(Succeed())
					Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
						Name: "probe-test-fixture", Namespace: expectedNamespace,
					}}))).To(Succeed())
					cleanupWorkbenches(wb)
					cleanupNamespace("probe-policy-notebooks")
				})

				_, err := reconcileWorkbenches(reconciler, wb)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, key, cm)).To(Succeed())
				Expect(cm.Data).To(HaveKeyWithValue("WORKBENCH_PROBE_TIMEOUT_SECONDS", "1"))

				updated := getWorkbenches(wb.Name)
				updated.Spec.ODHNotebookControllerProbe = &componentsv1alpha1.ODHNotebookControllerProbeSpec{TimeoutSeconds: 5}
				Expect(k8sClient.Update(ctx, updated)).To(Succeed())
				_, err = reconcileWorkbenches(reconciler, wb)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, key, cm)).To(Succeed())
				Expect(cm.Data).To(HaveKeyWithValue("WORKBENCH_PROBE_TIMEOUT_SECONDS", "5"))
				Expect(cm.Labels).To(HaveKeyWithValue(metadata.ComponentLabelKey, metadata.LabelTrue))
				Expect(metav1.IsControlledBy(cm, wb)).To(BeTrue())

				// A direct ConfigMap edit must not override the durable CR policy.
				cm.Data["WORKBENCH_PROBE_TIMEOUT_SECONDS"] = "99"
				Expect(k8sClient.Update(ctx, cm)).To(Succeed())
				_, err = reconcileWorkbenches(reconciler, wb)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, key, cm)).To(Succeed())
				Expect(cm.Data).To(HaveKeyWithValue("WORKBENCH_PROBE_TIMEOUT_SECONDS", "5"))

				updated = getWorkbenches(wb.Name)
				updated.Spec.ODHNotebookControllerProbe.TimeoutSeconds = 10
				Expect(k8sClient.Update(ctx, updated)).To(Succeed())
				_, err = reconcileWorkbenches(reconciler, wb)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, key, cm)).To(Succeed())
				Expect(cm.Data).To(HaveKeyWithValue("WORKBENCH_PROBE_TIMEOUT_SECONDS", "10"))

				// The settings ConfigMap is shared with controller options outside the probe policy.
				cm.Data["INJECT_CLUSTER_PROXY_ENV"] = "true"
				Expect(k8sClient.Update(ctx, cm)).To(Succeed())
				settingsUID := cm.UID

				updated = getWorkbenches(wb.Name)
				updated.Spec.ODHNotebookControllerProbe = nil
				Expect(k8sClient.Update(ctx, updated)).To(Succeed())
				for range 2 {
					_, err = reconcileWorkbenches(reconciler, wb)
					Expect(err).NotTo(HaveOccurred())
					Expect(k8sClient.Get(ctx, key, cm)).To(Succeed())
					Expect(cm.UID).To(Equal(settingsUID))
					Expect(cm.Data).To(HaveKeyWithValue("WORKBENCH_PROBE_TIMEOUT_SECONDS", "1"))
					Expect(cm.Data).To(HaveKeyWithValue("INJECT_CLUSTER_PROXY_ENV", "true"))
				}
			},
			Entry("OpenDataHub", "OpenDataHub", "", "opendatahub"),
			Entry("SelfManagedRhoai", "SelfManagedRhoai", "", "redhat-ods-applications"),
			Entry("namespace override", "OpenDataHub", "custom-probe-applications", "custom-probe-applications"),
		)
	})

	Context("When reconciling a managed Workbenches resource", func() {
		It("Should update notebook controller ingress config", func() {
			wb := createWorkbenches("Managed", "legacy-ingress-test", "OpenDataHub")
			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupDeployments(applicationsNamespace)
				cleanupNamespace("legacy-ingress-test")
			})
			ensureNamespace(applicationsNamespace)
			createDesiredDeployment(applicationsNamespace, testNotebookControllerDeployment)
			config := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: controller.NotebookControllerConfigMap, Namespace: applicationsNamespace},
			}
			seed := corev1apply.ConfigMap(config.Name, config.Namespace).WithData(map[string]string{
				"additionalIngressNames": `["alpha"]`,
			})
			Expect(k8sClient.Apply(ctx, seed, client.FieldOwner(controller.FieldOwner), client.ForceOwnership)).To(Succeed())
			seedContract := corev1apply.ConfigMap(config.Name, config.Namespace).WithData(map[string]string{
				componentsv1alpha1.IngressContractConfigMapKey: `{"version":"v1","sourceGeneration":3}`,
			})
			Expect(k8sClient.Apply(ctx, seedContract, client.FieldOwner("external-manager"), client.ForceOwnership)).To(Succeed())
			wantIngresses := "[]"
			failConfig := false
			deploymentApplies := 0
			watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
			Expect(err).NotTo(HaveOccurred())
			reconciler.Client = interceptor.NewClient(watchClient, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if obj.GetName() == controller.NotebookControllerConfigMap && failConfig {
						return errors.New("ingress config apply failed")
					}
					if obj.GetObjectKind().GroupVersionKind() == gvk.Deployment {
						deploymentApplies++
						current := &corev1.ConfigMap{}
						Expect(c.Get(ctx, client.ObjectKeyFromObject(config), current)).To(Succeed())
						Expect(current.Data["ingresses"]).To(MatchJSON(wantIngresses), "ingress config must precede operand apply")
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			_, err = reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())
			key := client.ObjectKey{Name: controller.NotebookControllerConfigMap, Namespace: applicationsNamespace}
			Expect(k8sClient.Get(ctx, key, config)).To(Succeed())
			Expect(config.Data["ingresses"]).To(Equal("[]"))
			Expect(config.Data).NotTo(HaveKey(componentsv1alpha1.IngressContractConfigMapKey))
			Expect(config.Data).NotTo(HaveKey("additionalIngressNames"))
			Expect(metav1.IsControlledBy(config, wb)).To(BeTrue())
			Expect(config.Labels).To(HaveKeyWithValue(metadata.ComponentLabelKey, metadata.LabelTrue))
			Expect(config.Labels).To(HaveKeyWithValue(metadata.PartOfLabelKey, metadata.ComponentLabelValue))

			updated := getWorkbenches(wb.Name)
			updated.Spec.IngressContract = &componentsv1alpha1.IngressContractSpec{
				Version:          componentsv1alpha1.IngressContractVersionV1,
				SourceGeneration: 7,
			}
			updated.Spec.Ingresses = []componentsv1alpha1.Ingress{
				{Name: "primary", GatewayName: "default-gateway", GatewayNamespace: "default-ingress", IsDefault: true},
				{Name: "alpha", GatewayName: "team-gateway", GatewayNamespace: "team-ingress", Hostname: "team.example.com"},
			}
			wantIngresses = `[
				{"name":"primary","gatewayName":"default-gateway","gatewayNamespace":"default-ingress","isDefault":true},
				{"name":"alpha","gatewayName":"team-gateway","gatewayNamespace":"team-ingress","hostname":"team.example.com"}
			]`
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())
			_, err = reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, config)).To(Succeed())
			Expect(config.Data["ingresses"]).To(MatchJSON(wantIngresses))
			Expect(config.Data[componentsv1alpha1.IngressContractConfigMapKey]).To(MatchJSON(
				`{"version":"v1","sourceGeneration":7}`,
			))

			updated = getWorkbenches(wb.Name)
			updated.Spec.IngressContract = nil
			updated.Spec.Ingresses = updated.Spec.Ingresses[1:]
			updated.Spec.Ingresses[0].GatewayName = "replacement-gateway"
			wantIngresses = `[
				{"name":"alpha","gatewayName":"replacement-gateway","gatewayNamespace":"team-ingress","hostname":"team.example.com"}
			]`
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())
			_, err = reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, config)).To(Succeed())
			Expect(config.Data["ingresses"]).To(MatchJSON(wantIngresses))

			updated = getWorkbenches(wb.Name)
			updated.Spec.Ingresses = nil
			wantIngresses = "[]"
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())
			_, err = reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, config)).To(Succeed())
			Expect(config.Data["ingresses"]).To(Equal("[]"))
			Expect(config.Data).NotTo(HaveKey(componentsv1alpha1.IngressContractConfigMapKey))
			Expect(deploymentApplies).To(Equal(4))

			failConfig = true
			_, err = reconcileWorkbenches(reconciler, wb)
			Expect(err).To(MatchError(ContainSubstring("ingress config apply failed")))
			Expect(deploymentApplies).To(Equal(4), "operands must not be applied when ingress config fails")
			Expect(k8sClient.Get(ctx, key, config)).To(Succeed())
			Expect(config.Data["ingresses"]).To(Equal("[]"))
		})

		It("Should reject ingress entries with missing or empty required fields and duplicate names", func() {
			wb := createWorkbenches("Managed", "", "OpenDataHub")
			DeferCleanup(func() { cleanupWorkbenches(wb) })
			for _, entries := range []string{
				`[{"gatewayName":"gateway","gatewayNamespace":"ingress"}]`,
				`[{"name":"alpha","gatewayNamespace":"ingress"}]`,
				`[{"name":"alpha","gatewayName":"gateway"}]`,
				`[{"name":"","gatewayName":"gateway","gatewayNamespace":"ingress"}]`,
				`[{"name":"alpha","gatewayName":"","gatewayNamespace":"ingress"}]`,
				`[{"name":"alpha","gatewayName":"gateway","gatewayNamespace":""}]`,
				`[{"name":"alpha","gatewayName":"one","gatewayNamespace":"ingress"},{"name":"alpha","gatewayName":"two","gatewayNamespace":"ingress"}]`,
			} {
				By("rejecting " + entries)
				patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"ingresses":`+entries+`}}`))
				Expect(apierrors.IsInvalid(k8sClient.Patch(ctx, wb, patch))).To(BeTrue())
			}
		})

		It("Should enforce Gateway reference syntax and ingress string length limits", func() {
			wb := createWorkbenches("Managed", "", "OpenDataHub")
			DeferCleanup(func() { cleanupWorkbenches(wb) })
			for _, field := range []string{"gatewayName", "gatewayNamespace"} {
				for _, value := range []string{"a", "0", "team-gateway", "team.gateway", "Gateway", "bad_name", "bad/name", "bad name", "-bad", "bad-", ".bad", "bad.", "bad..name"} {
					entry := map[string]string{"name": "alpha", "gatewayName": "gateway", "gatewayNamespace": "ingress"}
					entry[field] = value
					data, err := json.Marshal(map[string]any{"spec": map[string]any{"ingresses": []map[string]string{entry}}})
					Expect(err).NotTo(HaveOccurred())
					err = k8sClient.Patch(ctx, wb, client.RawPatch(types.MergePatchType, data))
					if value == "a" || value == "0" || value == "team-gateway" || (field == "gatewayName" && value == "team.gateway") {
						Expect(err).NotTo(HaveOccurred(), field+"="+value)
					} else {
						Expect(apierrors.IsInvalid(err)).To(BeTrue(), field+"="+value)
					}
				}
			}
			for _, limit := range []struct {
				field string
				max   int
			}{{"name", 63}, {"gatewayName", 253}, {"gatewayNamespace", 63}, {"hostname", 253}} {
				entry := map[string]string{"name": "alpha", "gatewayName": "gateway", "gatewayNamespace": "ingress"}
				for _, length := range []int{limit.max, limit.max + 1} {
					entry[limit.field] = strings.Repeat("a", length)
					data, err := json.Marshal(map[string]any{"spec": map[string]any{"ingresses": []map[string]string{entry}}})
					Expect(err).NotTo(HaveOccurred())
					err = k8sClient.Patch(ctx, wb, client.RawPatch(types.MergePatchType, data))
					if length == limit.max {
						Expect(err).NotTo(HaveOccurred(), limit.field)
					} else {
						Expect(apierrors.IsInvalid(err)).To(BeTrue(), limit.field)
					}
				}
			}
		})

		It("Should validate optional hostname syntax", func() {
			wb := createWorkbenches("Managed", "", "OpenDataHub")
			DeferCleanup(func() { cleanupWorkbenches(wb) })
			for _, hostname := range []string{
				"", "a", "0", "team-notebooks.example.com", "Bad_Name", "UPPER.example.com", "*.example.com",
				"bad/name", "bad name", "-bad", "bad-", ".bad", "bad.", "bad..name",
			} {
				entry := map[string]string{"name": "Opaque_Name", "gatewayName": "gateway", "gatewayNamespace": "ingress", "hostname": hostname}
				data, err := json.Marshal(map[string]any{"spec": map[string]any{"ingresses": []map[string]string{entry}}})
				Expect(err).NotTo(HaveOccurred())
				err = k8sClient.Patch(ctx, wb, client.RawPatch(types.MergePatchType, data))
				if hostname == "" || hostname == "a" || hostname == "0" || hostname == "team-notebooks.example.com" {
					Expect(err).NotTo(HaveOccurred(), hostname)
				} else {
					Expect(apierrors.IsInvalid(err)).To(BeTrue(), hostname)
				}
			}
		})

		It("Should allow at most one default ingress without limiting the number of entries", func() {
			wb := createWorkbenches("Managed", "", "OpenDataHub")
			DeferCleanup(func() { cleanupWorkbenches(wb) })
			entries := make([]componentsv1alpha1.Ingress, 300)
			for i := range entries {
				entries[i] = componentsv1alpha1.Ingress{Name: fmt.Sprintf("ingress-%d", i), GatewayName: "gateway", GatewayNamespace: "ingress"}
			}
			for _, defaults := range []int{0, 1, 2} {
				updated := getWorkbenches(wb.Name)
				updated.Spec.Ingresses = entries
				for i := range defaults {
					updated.Spec.Ingresses[i].IsDefault = true
				}
				err := k8sClient.Update(ctx, updated)
				if defaults < 2 {
					Expect(err).NotTo(HaveOccurred())
				} else {
					Expect(apierrors.IsInvalid(err)).To(BeTrue())
				}
			}
		})

		It("Should reject duplicate resource identities before applying the conflicting group", func() {
			wb := createWorkbenches("Managed", "", "OpenDataHub")
			DeferCleanup(func() { cleanupWorkbenches(wb) })
			watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
			Expect(err).NotTo(HaveOccurred())
			var applied []string
			reconciler.Client = interceptor.NewClient(watchClient, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					applied = append(applied, obj.GetName())
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			for _, name := range []string{controller.NotebookControllerConfigMap, "duplicate-config"} {
				applied = nil
				for _, group := range []string{"workbenches/kf-notebook-controller/overlays/openshift", "workbenches/odh-notebook-controller/overlays/odh"} {
					dir := filepath.Join(manifestsDir, group)
					resource := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n"
					Expect(os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(resource), 0o600)).To(Succeed())
					Expect(os.WriteFile(filepath.Join(dir, "kustomization.yaml"),
						[]byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [config.yaml]\n"), 0o600)).To(Succeed())
				}
				_, err = reconcileWorkbenches(reconciler, wb)
				Expect(err).To(MatchError(And(ContainSubstring("duplicate resource"), ContainSubstring(applicationsNamespace+"/"+name))))
				wantApplied := []string{controller.NotebookControllerConfigMap}
				if name != controller.NotebookControllerConfigMap {
					wantApplied = append(wantApplied, name)
				}
				Expect(applied).To(Equal(wantApplied))
				config := &corev1.ConfigMap{}
				Expect(k8sClient.Get(ctx, client.ObjectKey{Name: controller.NotebookControllerConfigMap, Namespace: applicationsNamespace}, config)).To(Succeed())
				Expect(config.Data["ingresses"]).To(Equal("[]"))
			}
		})

		It("Should create the applications namespace and set status conditions", func() {
			legacyNS := "legacy-notebooks-ns"

			wb := createWorkbenches("Managed", legacyNS, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace(legacyNS)
			})

			result, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			// Requeue expected since no deployments are present
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: applicationsNamespace}, ns)).To(Succeed())
			Expect(ns.Labels).To(HaveKeyWithValue(metadata.OwnedNamespaceLabel, metadata.LabelTrue))
			Expect(ns.OwnerReferences).To(BeEmpty(), "generated namespaces must not be controller-owned")

			legacy := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: legacyNS}, legacy)).To(Succeed())
			Expect(legacy.Labels).To(HaveKeyWithValue(metadata.OwnedNamespaceLabel, metadata.LabelTrue))
			Expect(legacy.OwnerReferences).To(BeEmpty(), "legacy workbench namespaces must not be controller-owned")

			updated := getWorkbenches(wb.Name)
			Expect(updated.Status.ObservedGeneration).To(Equal(updated.Generation))
			Expect(updated.Status.ApplicationsNamespace).To(Equal(applicationsNamespace))
			Expect(updated.Status.WorkbenchNamespace).To(Equal(legacyNS))

			provCond := meta.FindStatusCondition(updated.Status.Conditions, "ProvisioningSucceeded")
			Expect(provCond).NotTo(BeNil())
			Expect(provCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(provCond.Reason).To(Equal("Provisioned"))

			Expect(updated.Status.Releases).To(HaveLen(1))
			Expect(updated.Status.Releases[0].Name).To(Equal("Kubeflow Notebook Controller"))
			Expect(updated.Status.Releases[0].Version).To(Equal("1.10.0"))
			Expect(updated.Status.Releases[0].RepoURL).To(Equal("https://github.com/kubeflow/kubeflow"))

			releaseCond := meta.FindStatusCondition(updated.Status.Conditions, "ReleaseMetadataAvailable")
			Expect(releaseCond).NotTo(BeNil())
			Expect(releaseCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(releaseCond.Reason).To(Equal("Available"))
		})

		It("Should continue reconciliation when release metadata is malformed", func() {
			nsName := "test-ns-bad-metadata"
			Expect(os.WriteFile(
				filepath.Join(manifestsDir, "workbenches/kf-notebook-controller/component_metadata.yaml"),
				[]byte("not: valid: yaml: ["),
				0o600,
			)).To(Succeed())

			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace(nsName)
			})

			result, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			updated := getWorkbenches(wb.Name)

			provCond := meta.FindStatusCondition(updated.Status.Conditions, "ProvisioningSucceeded")
			Expect(provCond).NotTo(BeNil())
			Expect(provCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(provCond.Reason).To(Equal("Provisioned"))
			Expect(updated.Status.Releases).To(BeEmpty())

			releaseCond := meta.FindStatusCondition(updated.Status.Conditions, "ReleaseMetadataAvailable")
			Expect(releaseCond).NotTo(BeNil())
			Expect(releaseCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(releaseCond.Reason).To(Equal("ReleaseMetadataFailed"))
		})

		It("Should set phase=Pending before provisioning on first observe", func() {
			nsName := "test-ns-pending"
			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace(nsName)
			})

			result, err := reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeFalse())

			pending := getWorkbenches(wb.Name)
			Expect(pending.Status.Phase).To(Equal(statusutil.PhasePending))
			Expect(pending.Status.ObservedGeneration).To(Equal(int64(0)))

			_, err = reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Status.Phase).To(Equal(statusutil.PhaseInitializing))
		})

		It("Should set phase=Initializing when deployments are not yet available", func() {
			nsName := "test-ns-no-deploys"
			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace(nsName)
			})

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Status.Phase).To(Equal(statusutil.PhaseInitializing))
		})

		It("Should set DeploymentsAvailable=False when no deployments exist", func() {
			nsName := "test-ns-no-deploys"
			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace(nsName)
			})

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)

			deplCond := meta.FindStatusCondition(updated.Status.Conditions, "DeploymentsAvailable")
			Expect(deplCond).NotTo(BeNil())
			Expect(deplCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(deplCond.Reason).To(Equal("Unavailable"))
		})

		It("Should set phase=Upgrading after a spec change when previously ready", func() {
			nsName := "test-ns-upgrading"
			ensureNamespace(applicationsNamespace)
			createDesiredDeployment(applicationsNamespace, "odh-notebook-controller")

			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupDeployments(applicationsNamespace)
				cleanupNamespace(nsName)
			})

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			ready := getWorkbenches(wb.Name)
			Expect(ready.Status.Phase).To(Equal(statusutil.PhaseReady))
			Expect(ready.Status.Distribution.Name).To(Equal(platformconfig.DistributionNameStandalone))
			Expect(ready.Status.Distribution.Version).To(Equal("0.0.0"))

			ready.Spec.GatewayDomain = "gateway.example.com"
			Expect(k8sClient.Update(ctx, ready)).To(Succeed())

			updateDeploymentReplicas(applicationsNamespace, 1, 0)

			_, err = reconciler.Reconcile(ctx, requestFor(ready))
			Expect(err).NotTo(HaveOccurred())

			upgrading := getWorkbenches(wb.Name)
			Expect(upgrading.Status.Phase).To(Equal(statusutil.PhaseUpgrading))
		})

		It("Should set phase=Degraded when deployments regress after being ready", func() {
			nsName := "test-ns-degraded"
			ensureNamespace(applicationsNamespace)
			createDesiredDeployment(applicationsNamespace, "odh-notebook-controller")

			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupDeployments(applicationsNamespace)
				cleanupNamespace(nsName)
			})

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			ready := getWorkbenches(wb.Name)
			Expect(ready.Status.Phase).To(Equal(statusutil.PhaseReady))

			updateDeploymentReplicas(applicationsNamespace, 1, 0)

			_, err = reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Status.Phase).To(Equal(statusutil.PhaseDegraded))

			degradedCond := meta.FindStatusCondition(updated.Status.Conditions, "Degraded")
			Expect(degradedCond).NotTo(BeNil())
			Expect(degradedCond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("Should recover to Ready when deployments become available after Degraded", func() {
			nsName := "test-ns-degraded-recovery"
			ensureNamespace(applicationsNamespace)
			createDesiredDeployment(applicationsNamespace, "odh-notebook-controller")

			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupDeployments(applicationsNamespace)
				cleanupNamespace(nsName)
			})

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())
			Expect(getWorkbenches(wb.Name).Status.Phase).To(Equal(statusutil.PhaseReady))

			updateDeploymentReplicas(applicationsNamespace, 1, 0)

			_, err = reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())
			Expect(getWorkbenches(wb.Name).Status.Phase).To(Equal(statusutil.PhaseDegraded))

			updateDeploymentReplicas(applicationsNamespace, 1, 1)

			_, err = reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())

			recovered := getWorkbenches(wb.Name)
			Expect(recovered.Status.Phase).To(Equal(statusutil.PhaseReady))

			degradedCond := meta.FindStatusCondition(recovered.Status.Conditions, "Degraded")
			Expect(degradedCond).NotTo(BeNil())
			Expect(degradedCond.Status).To(Equal(metav1.ConditionFalse))
		})

		It("Should treat deployment scaled to zero as unavailable", func() {
			nsName := "test-ns-scaled-zero"
			ensureNamespace(applicationsNamespace)
			createDesiredDeployment(applicationsNamespace, "odh-notebook-controller")

			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupDeployments(applicationsNamespace)
				cleanupNamespace(nsName)
			})

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())
			Expect(getWorkbenches(wb.Name).Status.Phase).To(Equal(statusutil.PhaseReady))

			updateDeploymentReplicas(applicationsNamespace, 0, 0)

			_, err = reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Status.Phase).To(Equal(statusutil.PhaseDegraded))

			deplCond := meta.FindStatusCondition(updated.Status.Conditions, "DeploymentsAvailable")
			Expect(deplCond).NotTo(BeNil())
			Expect(deplCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(deplCond.Message).To(ContainSubstring("scaled to zero"))
		})

		It("Should set Ready=True when deployments are available in standalone mode", func() {
			nsName := "test-ns-ready"
			ensureNamespace(applicationsNamespace)
			createDesiredDeployment(applicationsNamespace, "odh-notebook-controller")

			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupDeployments(applicationsNamespace)
				cleanupNamespace(nsName)
			})

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Status.Phase).To(Equal(statusutil.PhaseReady))
			Expect(updated.Status.Distribution.Name).To(Equal(platformconfig.DistributionNameStandalone))
			Expect(updated.Status.Distribution.Version).To(Equal("0.0.0"))

			readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCond.Reason).To(Equal("ReconcileSuccess"))

			degradedCond := meta.FindStatusCondition(updated.Status.Conditions, "Degraded")
			Expect(degradedCond).NotTo(BeNil())
			Expect(degradedCond.Status).To(Equal(metav1.ConditionFalse))
		})

		It("Should update platform version on Standalone distribution after version change", func() {
			nsName := "test-ns-standalone-version-update"
			ensureNamespace(applicationsNamespace)
			createDesiredDeployment(applicationsNamespace, "odh-notebook-controller")

			// Create the platform ConfigMap with an initial platformVersion but no
			// distribution fields — this simulates Standalone mode with a version stamp.
			createPlatformConfig(applicationsNamespace, "", "", "1.0.0")

			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupDeployments(applicationsNamespace)
				cleanupNamespace(nsName)
				cleanupPlatformConfig(applicationsNamespace)
			})

			// Initial reconcile stamps platform version 1.0.0
			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Status.Phase).To(Equal(statusutil.PhaseReady))
			Expect(updated.Status.Distribution.Name).To(Equal(platformconfig.DistributionNameStandalone))

			platformRelease := findPlatformRelease(updated.Status.Releases)
			Expect(platformRelease).NotTo(BeNil())
			Expect(platformRelease.Version).To(Equal("1.0.0"))

			// Update the ConfigMap to change the platform version
			updatePlatformConfig(applicationsNamespace, "", "", "2.0.0")

			_, err = reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())

			updated = getWorkbenches(wb.Name)
			Expect(updated.Status.Phase).To(Equal(statusutil.PhaseReady))

			platformRelease = findPlatformRelease(updated.Status.Releases)
			Expect(platformRelease).NotTo(BeNil())
			Expect(platformRelease.Version).To(Equal("2.0.0"))
		})

		It("Should fall back to standalone when ApplicationsNamespace is not configured", func() {
			fallbackNS := "opendatahub"
			ensureNamespace(fallbackNS)
			cleanupDeployments(fallbackNS)
			createDesiredDeployment(fallbackNS, "odh-notebook-controller")

			standaloneReconciler := &controller.WorkbenchesReconciler{
				Client:            k8sClient,
				Scheme:            scheme.Scheme,
				ManifestsBasePath: manifestsDir,
			}

			wb := createWorkbenches("Managed", "legacy-ignored-ns", "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupDeployments(fallbackNS)
			})

			_, err := reconcileWorkbenches(standaloneReconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Status.Phase).To(Equal(statusutil.PhaseReady))
			Expect(updated.Status.ApplicationsNamespace).To(Equal(fallbackNS))
			Expect(updated.Status.WorkbenchNamespace).To(Equal("legacy-ignored-ns"))
			Expect(updated.Status.Distribution.Name).To(Equal(platformconfig.DistributionNameStandalone))
			Expect(updated.Status.Distribution.Version).To(Equal("0.0.0"))
		})

		It("Should fall back to platform default when ApplicationsNamespace is invalid", func() {
			fallbackNS := "opendatahub"
			ensureNamespace(fallbackNS)

			invalidReconciler := &controller.WorkbenchesReconciler{
				Client:                k8sClient,
				Scheme:                scheme.Scheme,
				ManifestsBasePath:     manifestsDir,
				ApplicationsNamespace: "bad/name",
			}

			wb := createWorkbenches("Managed", "legacy-invalid-apps-ns", "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace("legacy-invalid-apps-ns")
				// fallbackNS is the shared suite applications namespace — do not delete it.
				removeOwnedNamespaceLabel(fallbackNS)
			})

			_, err := reconcileWorkbenches(invalidReconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Status.ApplicationsNamespace).To(Equal(fallbackNS))

			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: fallbackNS}, ns)).To(Succeed())
			Expect(ns.Labels).To(HaveKeyWithValue(metadata.OwnedNamespaceLabel, metadata.LabelTrue))
		})

		It("Should fall back to redhat-ods-applications for SelfManagedRhoai when ApplicationsNamespace is unset", func() {
			fallbackNS := "redhat-ods-applications"
			ensureNamespace(fallbackNS)
			cleanupDeployments(fallbackNS)
			createDesiredDeployment(fallbackNS, "odh-notebook-controller")

			standaloneReconciler := &controller.WorkbenchesReconciler{
				Client:            k8sClient,
				Scheme:            scheme.Scheme,
				ManifestsBasePath: manifestsDir,
			}

			wb := createWorkbenches("Managed", "legacy-ignored-ns", "SelfManagedRhoai")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupDeployments(fallbackNS)
				cleanupNamespace(fallbackNS)
			})

			_, err := reconcileWorkbenches(standaloneReconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Status.ApplicationsNamespace).To(Equal(fallbackNS))
			Expect(updated.Status.WorkbenchNamespace).To(Equal("legacy-ignored-ns"))
		})

		It("Should remain not Ready when platform version config is missing on managed distribution", func() {
			nsName := "test-ns-no-platform-version"
			ensureNamespace(applicationsNamespace)
			createDesiredDeployment(applicationsNamespace, "odh-notebook-controller")
			createPlatformConfig(applicationsNamespace, platformconfig.DistributionNameSelfManagedRHOAI, "1.0.0", "")

			wb := createWorkbenches("Managed", nsName, "SelfManagedRhoai")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupDeployments(applicationsNamespace)
				cleanupNamespace(nsName)
				cleanupPlatformConfig(applicationsNamespace)
			})

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Status.Phase).To(Equal(statusutil.PhaseInitializing))

			readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal("PlatformVersionPending"))
		})

		It("Should set Ready=True when deployments, distribution, and handshake are complete", func() {
			nsName := "test-ns-managed-ready"
			ensureNamespace(applicationsNamespace)
			createDesiredDeployment(applicationsNamespace, "odh-notebook-controller")
			createPlatformConfig(applicationsNamespace, "OpenDataHub", "2.0.0", testPlatformVersion)

			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupDeployments(applicationsNamespace)
				cleanupNamespace(nsName)
				cleanupPlatformConfig(applicationsNamespace)
			})

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Status.Phase).To(Equal(statusutil.PhaseReady))
			Expect(updated.Status.Distribution.Name).To(Equal("OpenDataHub"))
			Expect(updated.Status.Distribution.Version).To(Equal("2.0.0"))

			platformRelease := findPlatformRelease(updated.Status.Releases)
			Expect(platformRelease).NotTo(BeNil())
			Expect(platformRelease.Version).To(Equal(testPlatformVersion))
		})

		It("Should keep status.distribution until upgrade completes", func() {
			nsName := "test-ns-dist-upgrade"
			ensureNamespace(applicationsNamespace)
			createDesiredDeployment(applicationsNamespace, "odh-notebook-controller")
			createPlatformConfig(applicationsNamespace, "OpenDataHub", "1.0.0", testPlatformVersion)

			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupDeployments(applicationsNamespace)
				cleanupNamespace(nsName)
				cleanupPlatformConfig(applicationsNamespace)
			})

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Status.Distribution.Version).To(Equal("1.0.0"))

			updatePlatformConfig(applicationsNamespace, "OpenDataHub", "2.0.0", testPlatformVersion)
			updateDeploymentReplicas(applicationsNamespace, 1, 0)

			_, err = reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())

			updated = getWorkbenches(wb.Name)
			Expect(updated.Status.Distribution.Version).To(Equal("1.0.0"))

			readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))

			updateDeploymentReplicas(applicationsNamespace, 1, 1)

			_, err = reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())

			updated = getWorkbenches(wb.Name)
			Expect(updated.Status.Distribution.Version).To(Equal("2.0.0"))
			Expect(updated.Status.Phase).To(Equal(statusutil.PhaseReady))
		})

		It("Should gate Ready on distribution alignment before deployments are ready", func() {
			nsName := "test-ns-dist-pending"
			createNamespace(nsName)
			createPlatformConfig(applicationsNamespace, "OpenDataHub", "1.0.0", testPlatformVersion)

			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace(nsName)
				cleanupPlatformConfig(applicationsNamespace)
			})

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Status.Distribution.Name).To(BeEmpty())
			Expect(updated.Status.Distribution.Version).To(BeEmpty())

			readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		})

		It("Should deploy into ApplicationsNamespace for SelfManagedRhoai platform", func() {
			wb := createWorkbenches("Managed", "", "SelfManagedRhoai")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace("rhods-notebooks")
			})

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Status.ApplicationsNamespace).To(Equal(applicationsNamespace))
			Expect(updated.Status.WorkbenchNamespace).To(BeEmpty())

			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: applicationsNamespace}, ns)).To(Succeed())

			legacy := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "rhods-notebooks"}, legacy)).To(Succeed())
			Expect(legacy.Labels).To(HaveKeyWithValue(metadata.OwnedNamespaceLabel, metadata.LabelTrue))
		})

		It("Should label a pre-existing applications namespace", func() {
			ensureNamespace(applicationsNamespace)
			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: applicationsNamespace}, ns)).To(Succeed())
			ns.OwnerReferences = nil
			if ns.Labels != nil {
				delete(ns.Labels, metadata.OwnedNamespaceLabel)
			}
			Expect(k8sClient.Update(ctx, ns)).To(Succeed())

			wb := createWorkbenches("Managed", "legacy-ignored-ns", "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
			})

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: applicationsNamespace}, ns)).To(Succeed())
			Expect(ns.Labels).To(HaveKeyWithValue(metadata.OwnedNamespaceLabel, metadata.LabelTrue))
			Expect(ns.OwnerReferences).To(BeEmpty(), "pre-existing namespaces must not be claimed")
		})

		It("Should create legacy workbenchNamespace without deploying operands there", func() {
			legacyNS := "legacy-jupyterhub-ns"
			wb := &componentsv1alpha1.Workbenches{
				ObjectMeta: metav1.ObjectMeta{Name: componentsv1alpha1.WorkbenchesInstanceName},
				Spec: componentsv1alpha1.WorkbenchesSpec{
					ManagementState:    "Managed",
					WorkbenchNamespace: legacyNS,
					Platform:           "SelfManagedRhoai",
					GatewayDomain:      "gateway.example.com",
					MLflowEnabled:      true,
				},
			}
			Expect(k8sClient.Create(ctx, wb)).To(Succeed())

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace(legacyNS)
			})

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Spec.WorkbenchNamespace).To(Equal(legacyNS))
			Expect(updated.Status.ApplicationsNamespace).To(Equal(applicationsNamespace))
			Expect(updated.Status.WorkbenchNamespace).To(Equal(legacyNS))

			legacy := &corev1.Namespace{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: legacyNS}, legacy)).To(Succeed())
			Expect(legacy.Labels).To(HaveKeyWithValue(metadata.OwnedNamespaceLabel, metadata.LabelTrue))
			Expect(legacy.OwnerReferences).To(BeEmpty(), "legacy workbench namespaces must not be controller-owned")

			deploys := &appsv1.DeploymentList{}
			Expect(k8sClient.List(ctx, deploys, client.InNamespace(legacyNS))).To(Succeed())
			Expect(deploys.Items).To(BeEmpty(), "operands must not deploy into legacy workbenchNamespace")
		})
	})

	Context("When reconciling a Removed Workbenches resource", func() {
		It("Should set Ready=False and ProvisioningSucceeded=False", func() {
			wb := createWorkbenches("Removed", "", "")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
			})

			result, err := reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			updated := getWorkbenches(wb.Name)
			Expect(updated.Status.Phase).To(Equal(statusutil.PhaseFailed))

			readyCond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCond.Reason).To(Equal("Removed"))

			provCond := meta.FindStatusCondition(updated.Status.Conditions, "ProvisioningSucceeded")
			Expect(provCond).NotTo(BeNil())
			Expect(provCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(provCond.Reason).To(Equal("Removed"))

			Expect(updated.Status.Releases).To(BeEmpty())
			Expect(updated.Status.ApplicationsNamespace).To(Equal(applicationsNamespace))
			Expect(updated.Status.WorkbenchNamespace).To(BeEmpty())
		})

		It("Should clean up labeled resources when transitioning to Removed", func() {
			nsName := "test-ns-removed-cleanup"
			ensureNamespace(applicationsNamespace)
			createDeployment(applicationsNamespace, "notebook-controller")

			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupDeployments(applicationsNamespace)
				cleanupNamespace(nsName)
			})

			// First reconcile in Managed state adds the finalizer
			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			// Transition to Removed
			updated := getWorkbenches(wb.Name)
			updated.Spec.ManagementState = "Removed"
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())

			// Verify the labeled deployment was deleted
			deploys := &appsv1.DeploymentList{}
			Expect(k8sClient.List(ctx, deploys, client.InNamespace(applicationsNamespace), client.MatchingLabels{
				metadata.ComponentLabelKey: metadata.LabelTrue,
				metadata.PartOfLabelKey:    metadata.ComponentLabelValue,
			})).To(Succeed())
			Expect(deploys.Items).To(BeEmpty())
			config := &corev1.ConfigMap{}
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{
				Name: controller.NotebookControllerConfigMap, Namespace: applicationsNamespace,
			}, config))).To(BeTrue())

			// Verify status is set correctly
			final := getWorkbenches(wb.Name)
			Expect(final.Status.Phase).To(Equal(statusutil.PhaseFailed))
		})
	})

	Context("When the resource does not exist", func() {
		It("Should return no error and empty result", func() {
			result, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "nonexistent"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("Finalizer management", func() {
		It("Should add the finalizer on first reconcile", func() {
			nsName := "test-ns-finalizer-add"
			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace(nsName)
			})

			_, err := reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Finalizers).To(ContainElement("components.platform.opendatahub.io/workbenches-cleanup"))
		})

		It("Should clean up labeled resources and remove finalizer on deletion", func() {
			nsName := "test-ns-finalizer-del"
			ensureNamespace(applicationsNamespace)
			createDeployment(applicationsNamespace, "notebook-controller-deployment")

			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupDeployments(applicationsNamespace)
				cleanupNamespace(nsName)
			})

			// First reconcile adds the finalizer
			_, err := reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Finalizers).To(ContainElement("components.platform.opendatahub.io/workbenches-cleanup"))

			// Delete the CR (sets DeletionTimestamp)
			Expect(k8sClient.Delete(ctx, updated)).To(Succeed())

			// Reconcile should trigger cleanup and remove the finalizer
			_, err = reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())

			// Verify the deployment was deleted
			deploys := &appsv1.DeploymentList{}
			Expect(k8sClient.List(ctx, deploys, client.InNamespace(applicationsNamespace), client.MatchingLabels{
				metadata.ComponentLabelKey: metadata.LabelTrue,
				metadata.PartOfLabelKey:    metadata.ComponentLabelValue,
			})).To(Succeed())
			Expect(deploys.Items).To(BeEmpty())
		})

		It("Should skip cleanup and complete deletion when finalizer is absent", func() {
			nsName := "test-ns-no-finalizer"
			ensureNamespace(applicationsNamespace)
			createDeployment(applicationsNamespace, "should-survive")

			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupDeployments(applicationsNamespace)
				cleanupNamespace(nsName)
			})

			// First reconcile adds the workbenches finalizer
			_, err := reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())

			// Remove the workbenches finalizer manually (simulating it was never added)
			// and add a temporary holder so the object isn't immediately deleted
			updated := getWorkbenches(wb.Name)
			controllerutil.RemoveFinalizer(updated, "components.platform.opendatahub.io/workbenches-cleanup")
			controllerutil.AddFinalizer(updated, "test-holder")
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())

			// Delete the CR (DeletionTimestamp is set, held by test-holder)
			Expect(k8sClient.Delete(ctx, updated)).To(Succeed())

			// Reconcile should see DeletionTimestamp but no workbenches finalizer,
			// so it skips cleanup entirely. test-holder keeps the object alive.
			result, err := reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			// The labeled deployment should still exist (no cleanup was performed)
			deploys := &appsv1.DeploymentList{}
			Expect(k8sClient.List(ctx, deploys, client.InNamespace(applicationsNamespace), client.MatchingLabels{
				metadata.ComponentLabelKey: metadata.LabelTrue,
				metadata.PartOfLabelKey:    metadata.ComponentLabelValue,
			})).To(Succeed())
			Expect(deploys.Items).To(HaveLen(1))
		})

		It("Should handle idempotent deletion when resources are already gone", func() {
			nsName := "test-ns-idempotent"
			createNamespace(nsName)

			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace(nsName)
			})

			// First reconcile adds the finalizer
			_, err := reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			Expect(updated.Finalizers).To(ContainElement("components.platform.opendatahub.io/workbenches-cleanup"))

			// Delete the CR — no labeled resources exist in the namespace
			Expect(k8sClient.Delete(ctx, updated)).To(Succeed())

			// Reconcile should succeed even though there's nothing to clean up
			result, err := reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	Context("When conditions have empty reasons", func() {
		It("Should sanitize empty reasons before status update", func() {
			nsName := "test-ns-empty-reason"
			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace(nsName)
			})

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			// We cannot write a condition with empty Reason through the
			// validated status API (CRD enforces minLength: 1). Instead,
			// wrap the client so that every Get of the Workbenches CR
			// injects a foreign condition with an empty Reason, simulating
			// what a pre-existing or foreign-controller condition looks
			// like when the reconciler reads the object.
			watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
			Expect(err).NotTo(HaveOccurred())

			wrappedClient := interceptor.NewClient(watchClient, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if getErr := c.Get(ctx, key, obj, opts...); getErr != nil {
						return getErr
					}
					if wbObj, ok := obj.(*componentsv1alpha1.Workbenches); ok {
						meta.SetStatusCondition(&wbObj.Status.Conditions, metav1.Condition{
							Type:    "ForeignCondition",
							Status:  metav1.ConditionFalse,
							Reason:  "",
							Message: "set by another controller with empty reason",
						})
					}
					return nil
				},
			})

			interceptedReconciler := &controller.WorkbenchesReconciler{
				Client:                wrappedClient,
				Scheme:                scheme.Scheme,
				ManifestsBasePath:     manifestsDir,
				ApplicationsNamespace: applicationsNamespace,
			}

			// Reconcile with the intercepted client — the sanitizer must
			// fix the empty Reason before the Status().Update() call.
			_, err = interceptedReconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())

			// Read back via the real client (no interception) and verify
			// the foreign condition was persisted with a sanitized Reason.
			final := getWorkbenches(wb.Name)
			foreignCond := meta.FindStatusCondition(final.Status.Conditions, "ForeignCondition")
			Expect(foreignCond).NotTo(BeNil())
			Expect(foreignCond.Reason).To(Equal(conditionReasonUnknown),
				"empty Reason should have been sanitized to 'Unknown'")
		})
	})

	Context("When transitioning between states", func() {
		It("Should transition from Managed to Removed", func() {
			nsName := "test-ns-transition"
			wb := createWorkbenches("Managed", nsName, "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace(nsName)
			})

			_, err := reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())

			// Transition to Removed
			updated := getWorkbenches(wb.Name)
			updated.Spec.ManagementState = "Removed"
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())

			result, err := reconciler.Reconcile(ctx, requestFor(wb))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			final := getWorkbenches(wb.Name)
			Expect(final.Status.Phase).To(Equal(statusutil.PhaseFailed))

			readyCond := meta.FindStatusCondition(final.Status.Conditions, "Ready")
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		})
	})

	Context("WorkbenchesV2 submodule condition", func() {
		It("Should set WorkbenchesV2Ready=False/Removed when workbenchesV2 is nil", func() {
			wb := createWorkbenches("Managed", "test-ns-v2-nil", "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace("test-ns-v2-nil")
			})

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			updated := getWorkbenches(wb.Name)
			v2Cond := meta.FindStatusCondition(updated.Status.Conditions, "WorkbenchesV2Ready")
			Expect(v2Cond).NotTo(BeNil())
			Expect(v2Cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(v2Cond.Reason).To(Equal("Removed"))
		})

		It("Should set WorkbenchesV2Ready=False/Removed when workbenchesV2 is explicitly Removed", func() {
			wb := createWorkbenches("Managed", "test-ns-v2-removed", "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace("test-ns-v2-removed")
			})

			updated := getWorkbenches(wb.Name)
			updated.Spec.WorkbenchesV2 = &componentsv1alpha1.WorkbenchesV2Spec{ManagementState: "Removed"}
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			final := getWorkbenches(wb.Name)
			v2Cond := meta.FindStatusCondition(final.Status.Conditions, "WorkbenchesV2Ready")
			Expect(v2Cond).NotTo(BeNil())
			Expect(v2Cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(v2Cond.Reason).To(Equal("Removed"))
		})

		It("Should set WorkbenchesV2Ready=False/ManifestsNotAvailable when Managed but no manifests", func() {
			wb := createWorkbenches("Managed", "test-ns-v2-no-manifests", "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace("test-ns-v2-no-manifests")
			})

			updated := getWorkbenches(wb.Name)
			updated.Spec.WorkbenchesV2 = &componentsv1alpha1.WorkbenchesV2Spec{ManagementState: "Managed"}
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			final := getWorkbenches(wb.Name)
			v2Cond := meta.FindStatusCondition(final.Status.Conditions, "WorkbenchesV2Ready")
			Expect(v2Cond).NotTo(BeNil())
			Expect(v2Cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(v2Cond.Reason).To(Equal("ManifestsNotAvailable"))
		})

		It("Should set WorkbenchesV2Ready=False/Unavailable when Managed, manifests exist, "+
			"but the workspaces-controller deployment is not found", func() {
			v2Dir := filepath.Join(manifestsDir, "workbenches", "workspaces-controller", "overlays", "gateway")
			Expect(os.MkdirAll(v2Dir, 0o750)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(v2Dir, "kustomization.yaml"),
				[]byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n"), 0o600)).To(Succeed())

			wb := createWorkbenches("Managed", "test-ns-v2-unavailable", "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace("test-ns-v2-unavailable")
			})

			updated := getWorkbenches(wb.Name)
			updated.Spec.WorkbenchesV2 = &componentsv1alpha1.WorkbenchesV2Spec{ManagementState: "Managed"}
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			final := getWorkbenches(wb.Name)
			v2Cond := meta.FindStatusCondition(final.Status.Conditions, "WorkbenchesV2Ready")
			Expect(v2Cond).NotTo(BeNil())
			Expect(v2Cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(v2Cond.Reason).To(Equal("Unavailable"))
		})

		It("Should set WorkbenchesV2Ready=True/Available when Managed, manifests exist, "+
			"and the workspaces-controller deployment is ready", func() {
			v2Dir := filepath.Join(manifestsDir, "workbenches", "workspaces-controller", "overlays", "gateway")
			Expect(os.MkdirAll(v2Dir, 0o750)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(v2Dir, "kustomization.yaml"),
				[]byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n"), 0o600)).To(Succeed())

			ensureNamespace(applicationsNamespace)

			wb := createWorkbenches("Managed", "test-ns-v2-available", "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupDeployments(applicationsNamespace)
				cleanupNamespace("test-ns-v2-available")
			})

			createDesiredDeployment(applicationsNamespace, "workspaces-controller")

			updated := getWorkbenches(wb.Name)
			updated.Spec.WorkbenchesV2 = &componentsv1alpha1.WorkbenchesV2Spec{ManagementState: "Managed"}
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			final := getWorkbenches(wb.Name)
			v2Cond := meta.FindStatusCondition(final.Status.Conditions, "WorkbenchesV2Ready")
			Expect(v2Cond).NotTo(BeNil())
			Expect(v2Cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(v2Cond.Reason).To(Equal("Available"))
		})

		It("Should set WorkbenchesV2Ready=False/Removed when global managementState is Removed", func() {
			wb := createWorkbenches("Removed", "test-ns-v2-global-removed", "OpenDataHub")

			DeferCleanup(func() {
				cleanupWorkbenches(wb)
				cleanupNamespace("test-ns-v2-global-removed")
			})

			updated := getWorkbenches(wb.Name)
			updated.Spec.WorkbenchesV2 = &componentsv1alpha1.WorkbenchesV2Spec{ManagementState: "Managed"}
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())

			_, err := reconcileWorkbenches(reconciler, wb)
			Expect(err).NotTo(HaveOccurred())

			final := getWorkbenches(wb.Name)
			v2Cond := meta.FindStatusCondition(final.Status.Conditions, "WorkbenchesV2Ready")
			Expect(v2Cond).NotTo(BeNil())
			Expect(v2Cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(v2Cond.Reason).To(Equal("Removed"))
		})
	})
})

// Test helpers

func createWorkbenches(state, ns, platformType string) *componentsv1alpha1.Workbenches {
	wb := &componentsv1alpha1.Workbenches{
		ObjectMeta: metav1.ObjectMeta{Name: componentsv1alpha1.WorkbenchesInstanceName},
		Spec: componentsv1alpha1.WorkbenchesSpec{
			ManagementState:    state,
			WorkbenchNamespace: ns,
			Platform:           platformType,
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, wb)).To(Succeed())

	return wb
}

func getWorkbenches(name string) *componentsv1alpha1.Workbenches {
	wb := &componentsv1alpha1.Workbenches{}
	ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: name}, wb)).To(Succeed())

	return wb
}

func requestFor(wb *componentsv1alpha1.Workbenches) ctrl.Request {
	return ctrl.Request{
		NamespacedName: types.NamespacedName{Name: wb.Name},
	}
}

// reconcileWorkbenches runs reconciliation, including the initial Pending bootstrap pass.
func reconcileWorkbenches(r *controller.WorkbenchesReconciler, wb *componentsv1alpha1.Workbenches) (ctrl.Result, error) {
	result, err := r.Reconcile(ctx, requestFor(wb))
	if err != nil {
		return result, err
	}

	latest := getWorkbenches(wb.Name)
	if latest.Status.Phase == statusutil.PhasePending && latest.Status.ObservedGeneration == 0 {
		return r.Reconcile(ctx, requestFor(wb))
	}

	return result, nil
}

func createNamespace(name string) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, ns)).To(Succeed())
}

func createDeployment(namespace, name string) *appsv1.Deployment {
	replicas := int32(1)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				metadata.ComponentLabelKey: metadata.LabelTrue,
				metadata.PartOfLabelKey:    metadata.ComponentLabelValue,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			// Match operand labels at creation: Deployment selectors are immutable.
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app":                      name,
					metadata.ComponentLabelKey: metadata.LabelTrue,
					metadata.PartOfLabelKey:    metadata.ComponentLabelValue,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                      name,
						metadata.ComponentLabelKey: metadata.LabelTrue,
						metadata.PartOfLabelKey:    metadata.ComponentLabelValue,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "manager", Image: "test:latest"},
					},
				},
			},
		},
	}

	deploy.SetGroupVersionKind(gvk.Deployment)
	desired := deploy.DeepCopy()
	err := k8sClient.Create(ctx, deploy)
	if client.IgnoreAlreadyExists(err) != nil {
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, deploy)).To(Succeed())
	deploy.Spec.Replicas = &replicas
	ExpectWithOffset(1, k8sClient.Update(ctx, deploy)).To(Succeed())

	deploy.Status.ReadyReplicas = 1
	deploy.Status.Replicas = 1
	deploy.Status.AvailableReplicas = 1
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, deploy)).To(Succeed())
	return desired
}

func updateDeploymentReplicas(namespace string, specReplicas, readyReplicas int32) {
	deploy := &appsv1.Deployment{}
	ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: testNotebookControllerDeployment}, deploy)).To(Succeed())

	deploy.Spec.Replicas = &specReplicas
	ExpectWithOffset(1, k8sClient.Update(ctx, deploy)).To(Succeed())

	deploy.Status.ReadyReplicas = readyReplicas
	deploy.Status.Replicas = specReplicas
	deploy.Status.AvailableReplicas = readyReplicas
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, deploy)).To(Succeed())
}

func findPlatformRelease(releases []componentsv1alpha1.ComponentRelease) *componentsv1alpha1.ComponentRelease {
	for i := range releases {
		if releases[i].Name == platformconfig.ReleaseName {
			return &releases[i]
		}
	}

	return nil
}

func cleanupWorkbenches(wb *componentsv1alpha1.Workbenches) {
	latest := &componentsv1alpha1.Workbenches{}

	err := k8sClient.Get(ctx, client.ObjectKeyFromObject(wb), latest)
	if err != nil {
		ExpectWithOffset(1, client.IgnoreNotFound(err)).To(Succeed())
		return
	}

	if len(latest.Finalizers) > 0 {
		latest.Finalizers = nil

		if err := k8sClient.Update(ctx, latest); err != nil {
			ExpectWithOffset(1, client.IgnoreNotFound(err)).To(Succeed())

			return
		}
	}

	ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, latest))).To(Succeed())
}

func cleanupNamespace(name string) {
	ns := &corev1.Namespace{}

	err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, ns)
	if err != nil {
		ExpectWithOffset(1, client.IgnoreNotFound(err)).To(Succeed())
		return
	}

	ExpectWithOffset(1, k8sClient.Delete(ctx, ns)).To(Succeed())
}

func removeOwnedNamespaceLabel(name string) {
	ns := &corev1.Namespace{}

	err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, ns)
	if client.IgnoreNotFound(err) != nil {
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return
	}

	if err != nil {
		return
	}

	if ns.Labels == nil {
		return
	}

	delete(ns.Labels, metadata.OwnedNamespaceLabel)
	if len(ns.Labels) == 0 {
		ns.Labels = nil
	}

	ExpectWithOffset(1, k8sClient.Update(ctx, ns)).To(Succeed())
}

func cleanupDeployments(namespace string) {
	deployments := &appsv1.DeploymentList{}

	ExpectWithOffset(1, k8sClient.List(ctx, deployments, client.InNamespace(namespace))).To(Succeed())

	for i := range deployments.Items {
		ExpectWithOffset(1, k8sClient.Delete(ctx, &deployments.Items[i])).To(Succeed())
	}
}

func ensureNamespace(name string) {
	ns := &corev1.Namespace{}
	err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, ns)
	if client.IgnoreNotFound(err) != nil {
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return
	}

	if err == nil {
		return
	}

	ExpectWithOffset(1, k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	})).To(Succeed())
}

func createPlatformConfig(namespace, distributionName, distributionVersion, platformVersion string) {
	ensureNamespace(namespace)

	data := map[string]string{
		platformconfig.DistributionNameKey:    distributionName,
		platformconfig.DistributionVersionKey: distributionVersion,
	}
	if platformVersion != "" {
		data[platformconfig.VersionDataKey] = platformVersion
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      platformconfig.ConfigMapName,
			Namespace: namespace,
		},
		Data: data,
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, cm)).To(Succeed())
}

func updatePlatformConfig(namespace, distributionName, distributionVersion, platformVersion string) {
	cm := &corev1.ConfigMap{}
	ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKey{
		Name:      platformconfig.ConfigMapName,
		Namespace: namespace,
	}, cm)).To(Succeed())

	if cm.Data == nil {
		cm.Data = map[string]string{}
	}

	cm.Data[platformconfig.DistributionNameKey] = distributionName
	cm.Data[platformconfig.DistributionVersionKey] = distributionVersion
	if platformVersion != "" {
		cm.Data[platformconfig.VersionDataKey] = platformVersion
	} else {
		delete(cm.Data, platformconfig.VersionDataKey)
	}
	ExpectWithOffset(1, k8sClient.Update(ctx, cm)).To(Succeed())
}

func cleanupPlatformConfig(namespace string) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      platformconfig.ConfigMapName,
			Namespace: namespace,
		},
	}
	ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, cm))).To(Succeed())
}
