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
	"bufio"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	componentsv1alpha1 "github.com/opendatahub-io/workbenches-operator/api/v1alpha1"
	"github.com/opendatahub-io/workbenches-operator/internal/metadata"
)

const (
	ssaOwnershipStability = 20 * time.Second
	// The broken apply path reconciles about every 3s. A pair of completions can
	// come from an unrelated watch; three or more inside this gap is the loop.
	reconcileLoopGap = 8 * time.Second

	fieldManagerWorkbenchesOperator = "workbenches-operator"
)

// operandStabilityGVKs are the kinds applyObjects can emit. resourceVersion is the
// wrong signal here: Deployment status, ImageStream import, and a one-time
// caBundle injection all bump it while this operator is idle. The field manager
// timestamp moves when our apply actually rewrites the object.
var operandStabilityGVKs = []schema.GroupVersionKind{
	{Group: "apps", Version: "v1", Kind: "Deployment"},
	{Group: "", Version: "v1", Kind: "ConfigMap"},
	{Group: "", Version: "v1", Kind: "Secret"},
	{Group: "", Version: "v1", Kind: "Service"},
	{Group: "", Version: "v1", Kind: "ServiceAccount"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"},
	{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"},
	{Group: "image.openshift.io", Version: "v1", Kind: "ImageStream"},
	{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"},
	{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "MutatingWebhookConfiguration"},
	{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingWebhookConfiguration"},
	{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"},
	{Group: "kubeflow.org", Version: "v1beta1", Kind: "WorkspaceKind"},
}

func registerSSAOwnershipTests() {
	Context("SSA ownership", Label("lifecycle"), func() {
		It("Should leave controller-owned fields alone and keep applied objects steady", func() {
			for _, name := range []string{
				"notebook-controller-kubeflow-notebooks-admin",
				"odh-notebook-controller-notebooks-admin",
			} {
				Eventually(func(g Gomega) {
					role := getClusterRole(g, name)
					_, found, err := unstructured.NestedFieldNoCopy(role.Object, "aggregationRule")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(found).To(BeTrue(), "%s missing aggregationRule", name)

					rules, rulesFound, err := unstructured.NestedSlice(role.Object, "rules")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(rulesFound).To(BeTrue(), "%s rules not populated yet", name)
					g.Expect(rules).NotTo(BeEmpty(), "%s rules still empty", name)

					g.Expect(managedFieldsOwnRules(role.GetManagedFields(), fieldManagerWorkbenchesOperator)).To(BeFalse(),
						"%s: workbenches-operator still owns .rules", name)
					g.Expect(aggregationControllerOwnsRules(role.GetManagedFields())).To(BeTrue(),
						"%s: aggregation controller does not own .rules", name)
				}, timeout, interval).Should(Succeed())
			}

			adminRVs := clusterRoleResourceVersions(
				"notebook-controller-kubeflow-notebooks-admin",
				"odh-notebook-controller-notebooks-admin",
			)
			editRVs := clusterRoleResourceVersions(
				"notebook-controller-kubeflow-notebooks-edit",
				"odh-notebook-controller-notebooks-edit",
			)

			generation := getWorkbenches().Generation
			managerTimes := operandFieldManagerTimes(Default)
			Expect(managerTimes).NotTo(BeEmpty())
			Expect(managerTimes).To(HaveKey(operandObjectKey("ClusterRole", "", "notebook-controller-kubeflow-notebooks-admin")))
			Expect(managerTimes).To(HaveKey(operandObjectKey("ClusterRole", "", "odh-notebook-controller-notebooks-admin")))

			logStart := time.Now()

			Consistently(func(g Gomega) {
				adminChanges := clusterRoleResourceVersionDiff(g, adminRVs)
				g.Expect(adminChanges).To(BeEmpty(),
					"aggregated ClusterRole resourceVersion changed:\n%s",
					strings.Join(adminChanges, "\n"))

				editChanges := clusterRoleResourceVersionDiff(g, editRVs)
				g.Expect(editChanges).To(BeEmpty(),
					"static ClusterRole resourceVersion changed:\n%s",
					strings.Join(editChanges, "\n"))

				g.Expect(workbenchesGeneration(g)).To(Equal(generation),
					"Workbenches generation changed during the stability window")
				changes := fieldManagerTimeDiff(managerTimes, operandFieldManagerTimes(g))
				g.Expect(changes).To(BeEmpty(),
					"workbenches-operator managedFields timestamp changed while generation was steady:\n%s",
					strings.Join(changes, "\n"))
			}, ssaOwnershipStability, interval).Should(Succeed())

			assertOperatorReconcileIsNotLooping(logStart)
		})
	})
}

func getClusterRole(g Gomega, name string) *unstructured.Unstructured {
	role := &unstructured.Unstructured{}
	role.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole",
	})
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, role)).To(Succeed())

	return role
}

func clusterRoleResourceVersions(names ...string) map[string]string {
	versions := make(map[string]string, len(names))
	for _, name := range names {
		versions[name] = getClusterRole(Default, name).GetResourceVersion()
	}

	return versions
}

func clusterRoleResourceVersionDiff(g Gomega, before map[string]string) []string {
	names := make([]string, 0, len(before))
	for name := range before {
		names = append(names, name)
	}

	sort.Strings(names)

	changes := make([]string, 0)

	for _, name := range names {
		now := getClusterRole(g, name).GetResourceVersion()
		if now == before[name] {
			continue
		}

		changes = append(changes, fmt.Sprintf("%s: %s -> %s", name, before[name], now))
	}

	return changes
}

func workbenchesGeneration(g Gomega) int64 {
	wb := &componentsv1alpha1.Workbenches{}
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name: componentsv1alpha1.WorkbenchesInstanceName,
	}, wb)).To(Succeed())

	return wb.Generation
}

func operandObjectKey(kind, namespace, name string) string {
	return fmt.Sprintf("%s/%s/%s", kind, namespace, name)
}

// operandFieldManagerTimes records when workbenches-operator last wrote each
// labeled operand. Other controllers may still bump resourceVersion.
func operandFieldManagerTimes(g Gomega) map[string]string {
	times := map[string]string{}

	for _, gvk := range operandStabilityGVKs {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk)

		err := k8sClient.List(ctx, list, client.MatchingLabels{
			metadata.ComponentLabelKey: metadata.LabelTrue,
			metadata.PartOfLabelKey:    metadata.ComponentLabelValue,
		})
		if meta.IsNoMatchError(err) {
			continue
		}

		g.Expect(err).NotTo(HaveOccurred())

		for i := range list.Items {
			obj := &list.Items[i]
			stamp, ok := workbenchesOperatorFieldManagerStamp(obj.GetManagedFields())
			g.Expect(ok).To(BeTrue(), "%s %s/%s has no %s managedFields entry",
				obj.GetKind(), obj.GetNamespace(), obj.GetName(), fieldManagerWorkbenchesOperator)
			times[operandObjectKey(obj.GetKind(), obj.GetNamespace(), obj.GetName())] = stamp
		}
	}

	return times
}

func fieldManagerTimeDiff(before, after map[string]string) []string {
	keys := make([]string, 0, len(before)+len(after))
	seen := make(map[string]struct{}, len(before)+len(after))

	for key := range before {
		seen[key] = struct{}{}
		keys = append(keys, key)
	}

	for key := range after {
		if _, ok := seen[key]; ok {
			continue
		}

		keys = append(keys, key)
	}

	sort.Strings(keys)

	changes := make([]string, 0)

	for _, key := range keys {
		was, hadBefore := before[key]
		now, hasAfter := after[key]

		switch {
		case !hadBefore:
			changes = append(changes, fmt.Sprintf("%s: added %s", key, now))
		case !hasAfter:
			changes = append(changes, fmt.Sprintf("%s: removed (was %s)", key, was))
		case was != now:
			changes = append(changes, fmt.Sprintf("%s: %s -> %s", key, was, now))
		}
	}

	return changes
}

func workbenchesOperatorFieldManagerStamp(fields []metav1.ManagedFieldsEntry) (string, bool) {
	parts := make([]string, 0, len(fields))

	for _, entry := range fields {
		if entry.Manager != fieldManagerWorkbenchesOperator || entry.Time == nil {
			continue
		}

		parts = append(parts, string(entry.Operation)+"/"+entry.Subresource+"@"+entry.Time.UTC().Format(time.RFC3339Nano))
	}

	if len(parts) == 0 {
		return "", false
	}

	sort.Strings(parts)

	return strings.Join(parts, ","), true
}

func managedFieldsOwnRules(fields []metav1.ManagedFieldsEntry, manager string) bool {
	for _, entry := range fields {
		if entry.Manager != manager || entry.FieldsV1 == nil {
			continue
		}

		var doc map[string]json.RawMessage
		if err := json.Unmarshal(entry.FieldsV1.Raw, &doc); err != nil {
			continue
		}

		if _, ok := doc["f:rules"]; ok {
			return true
		}
	}

	return false
}

func aggregationControllerOwnsRules(fields []metav1.ManagedFieldsEntry) bool {
	for _, entry := range fields {
		if !strings.Contains(entry.Manager, "aggregation") {
			continue
		}

		if managedFieldsOwnRules([]metav1.ManagedFieldsEntry{entry}, entry.Manager) {
			return true
		}
	}

	return false
}

func assertOperatorReconcileIsNotLooping(since time.Time) {
	logs := operatorManagerLogsSince(since)

	var completions []time.Time

	scanner := bufio.NewScanner(strings.NewReader(logs))
	for scanner.Scan() {
		var line struct {
			TS  json.RawMessage `json:"ts"`
			Msg string          `json:"msg"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil || line.Msg != "reconciliation complete" {
			continue
		}

		at, ok := parseLogTime(line.TS)
		if !ok {
			continue
		}

		completions = append(completions, at)
	}

	Expect(scanner.Err()).NotTo(HaveOccurred())

	if len(completions) < 3 {
		return
	}

	for i := 1; i < len(completions); i++ {
		gap := completions[i].Sub(completions[i-1])
		Expect(gap).To(BeNumerically(">=", reconcileLoopGap),
			"reconciliation complete at %s and %s are %s apart; expected the aggregated ClusterRole loop to be gone",
			completions[i-1].Format(time.RFC3339), completions[i].Format(time.RFC3339), gap)
	}
}

func operatorManagerLogsSince(since time.Time) string {
	Expect(restConfig).NotTo(BeNil())

	clientset, err := kubernetes.NewForConfig(restConfig)
	Expect(err).NotTo(HaveOccurred())

	deploy := &appsv1.Deployment{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name:      "workbenches-operator",
		Namespace: operatorNS,
	}, deploy)).To(Succeed())

	pods := &corev1.PodList{}
	Expect(k8sClient.List(ctx, pods,
		client.InNamespace(operatorNS),
		client.MatchingLabels(deploy.Spec.Selector.MatchLabels),
	)).To(Succeed())
	running := pods.Items[:0]
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			running = append(running, pods.Items[i])
		}
	}

	Expect(running).NotTo(BeEmpty(), "workbenches-operator pod not running in %s", operatorNS)

	sinceTime := metav1.NewTime(since)
	var logs strings.Builder

	for i := range running {
		pod := &running[i]
		stream, err := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: "manager",
			SinceTime: &sinceTime,
		}).Stream(ctx)
		Expect(err).NotTo(HaveOccurred())

		buf := bufio.NewScanner(stream)
		buf.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		for buf.Scan() {
			logs.WriteString(buf.Text())
			logs.WriteByte('\n')
		}

		Expect(stream.Close()).To(Succeed())
		Expect(buf.Err()).NotTo(HaveOccurred())
	}

	return logs.String()
}

func parseLogTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 {
		return time.Time{}, false
	}

	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		parsed, err := time.Parse(time.RFC3339, asString)
		if err != nil {
			return time.Time{}, false
		}

		return parsed, true
	}

	var asFloat float64
	if err := json.Unmarshal(raw, &asFloat); err != nil {
		return time.Time{}, false
	}

	seconds := int64(asFloat)
	nanos := int64((asFloat - float64(seconds)) * float64(time.Second))

	return time.Unix(seconds, nanos), true
}
