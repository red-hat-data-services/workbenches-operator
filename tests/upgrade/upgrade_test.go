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

package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	componentsv1alpha1 "github.com/opendatahub-io/workbenches-operator/api/v1alpha1"
	"github.com/opendatahub-io/workbenches-operator/internal/gvk"
	"github.com/opendatahub-io/workbenches-operator/internal/metadata"
)

const (
	timeout  = 8 * time.Minute
	interval = 5 * time.Second

	operatorDeploymentName       = "workbenches-operator"
	notebookControllerDeployment = "notebook-controller-deployment"
	odhNotebookControllerManager = "odh-notebook-controller-manager"
	webhookServiceName           = "workbenches-operator-webhook-service"

	legacyWorkbenchNamespace = "e2e-legacy-notebooks"
	runningNotebookName      = "upgrade-running"
	stoppedNotebookName      = "upgrade-stopped"
	probeNotebookName        = "upgrade-webhook-probe"
	probeSecretName          = "upgrade-connection"

	stoppedAnnotationKey = "kubeflow-resource-stopped"
	reconciliationLock   = "odh-notebook-controller-lock"
	notebookNameLabel    = "notebook-name"
	phasePrepare         = "prepare"
	phaseVerify          = "verify"

	// Long enough to see a reconcile loop, which is a few seconds per cycle.
	// A single managedFields sample is not enough.
	appliedObjectStability          = 20 * time.Second
	fieldManagerWorkbenchesOperator = "workbenches-operator"

	// go test uses this package directory as its working directory, so this is
	// tests/upgrade/artifacts/snapshot.json from the module root. That directory is gitignored.
	snapshotRelPath = "artifacts/snapshot.json"
)

func TestUpgrade(t *testing.T) {
	if os.Getenv("UPGRADE_PHASE") == "" {
		t.Skip("UPGRADE_PHASE is unset; run tests/upgrade/run.sh")
	}

	RegisterFailHandler(Fail)
	RunSpecs(t, "Upgrade Suite")
}

type upgradeSnapshot struct {
	ApplicationsNamespace string `json:"applicationsNamespace"`
	WorkbenchNamespace    string `json:"workbenchNamespace"`
	RunningNotebook       string `json:"runningNotebook"`
	RunningPodUID         string `json:"runningPodUID"`
	RunningRestartCount   int32  `json:"runningRestartCount"`
	StoppedNotebook       string `json:"stoppedNotebook"`
	StoppedAnnotation     string `json:"stoppedAnnotation"`
}

var (
	k8sClient  client.Client
	kubeClient kubernetes.Interface
	ctx        context.Context
	cancel     context.CancelFunc

	operatorNS    string
	appsNS        string
	workloadNS    string
	notebookImage string
	phase         string
)

var _ = BeforeSuite(func() {
	ctx, cancel = context.WithCancel(context.Background()) //nolint:fatcontext // suite-scoped, cancelled in AfterSuite

	phase = os.Getenv("UPGRADE_PHASE")
	Expect(phase).To(BeElementOf(phasePrepare, phaseVerify))

	operatorNS = envOr("OPERATOR_NAMESPACE", "workbenches-operator-system")
	appsNS = envOr("APPLICATIONS_NAMESPACE", "opendatahub")
	workloadNS = envOr("WORKLOAD_NAMESPACE", "upgrade-workbenches")
	notebookImage = envOr("NOTEBOOK_IMAGE", "registry.k8s.io/pause:3.10")

	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" {
		kubeconfigPath = os.Getenv("HOME") + "/.kube/config"
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	Expect(err).NotTo(HaveOccurred())

	scheme := runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(componentsv1alpha1.AddToScheme(scheme)).To(Succeed())

	k8sClient, err = client.New(config, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())

	kubeClient, err = kubernetes.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	cancel()
})

var _ = Describe("operator upgrade", func() {
	It("prepares the baseline cluster", func() {
		if phase != phasePrepare {
			Skip("UPGRADE_PHASE is not prepare")
		}

		ensureWorkbenches()
		waitForReadyConditions()
		waitForDeployment(appsNS, notebookControllerDeployment)
		waitForDeployment(appsNS, odhNotebookControllerManager)
		waitForWebhookEndpoints()

		ensureNamespace(workloadNS)
		replaceNotebook(runningNotebookName, nil)
		waitForRunningNotebook(runningNotebookName)

		// The odh controller writes kubeflow-resource-stopped=odh-notebook-controller-lock
		// while it mutates a new Notebook, then clears the annotation. A stop set
		// at create is removed with that lock, so stop only after the first reconcile.
		replaceNotebook(stoppedNotebookName, nil)
		waitForRunningNotebook(stoppedNotebookName)
		stopNotebook(stoppedNotebookName)
		annotation := waitForStoppedNotebook(stoppedNotebookName)

		pod, err := findNotebookPod(runningNotebookName)
		Expect(err).NotTo(HaveOccurred())
		writeSnapshot(upgradeSnapshot{
			ApplicationsNamespace: getWorkbenches().Status.ApplicationsNamespace,
			WorkbenchNamespace:    getWorkbenches().Status.WorkbenchNamespace,
			RunningNotebook:       runningNotebookName,
			RunningPodUID:         string(pod.UID),
			RunningRestartCount:   restartCount(pod),
			StoppedNotebook:       stoppedNotebookName,
			StoppedAnnotation:     annotation,
		})
	})

	It("keeps Notebook workloads across the upgrade", func() {
		if phase != phaseVerify {
			Skip("UPGRADE_PHASE is not verify")
		}

		before := readSnapshot()
		waitForTargetOperator()
		waitForReadyConditions()

		wb := getWorkbenches()
		Expect(wb.Status.ObservedGeneration).To(Equal(wb.Generation))
		Expect(wb.Status.ApplicationsNamespace).To(Equal(before.ApplicationsNamespace))
		Expect(wb.Status.WorkbenchNamespace).To(Equal(before.WorkbenchNamespace))
		Expect(wb.Status.ApplicationsNamespace).To(Equal(appsNS))
		Expect(wb.Status.WorkbenchNamespace).To(Equal(legacyWorkbenchNamespace))

		Eventually(func(g Gomega) {
			deploymentRolledOut(g, appsNS, notebookControllerDeployment)
			deploymentRolledOut(g, appsNS, odhNotebookControllerManager)
			controllersRunning(g)
		}, timeout, interval).Should(Succeed())
		waitForWebhookEndpoints()

		// The new controllers are up. Hold the sample long enough for a reconcile
		// to recreate the Notebook pod if the upgrade was going to.
		Consistently(func(g Gomega) {
			controllersRunning(g)
			operatorRunning(g)

			pod, err := findNotebookPod(before.RunningNotebook)
			g.Expect(err).NotTo(HaveOccurred())
			failIfPodCrashed(pod)
			g.Expect(string(pod.UID)).To(Equal(before.RunningPodUID))
			g.Expect(restartCount(pod)).To(BeNumerically("<=", before.RunningRestartCount))
			g.Expect(podReady(pod)).To(BeTrue())

			nb, nbErr := getNotebook(before.RunningNotebook)
			g.Expect(nbErr).NotTo(HaveOccurred())
			g.Expect(nb.GetAnnotations()[stoppedAnnotationKey]).To(BeEmpty())
		}, 45*time.Second, interval).Should(Succeed())

		Expect(waitForStoppedNotebook(before.StoppedNotebook)).To(Equal(before.StoppedAnnotation))

		assertConnectionInjection()
	})

	It("leaves applied objects still after the upgrade", func() {
		if phase != phaseVerify {
			Skip("UPGRADE_PHASE is not verify")
		}

		// Sample only after the target process is the one writing. A failure here
		// stays in this spec so the Notebook checks above still report.
		waitForTargetOperator()
		waitForReadyConditions()
		assertAppliedObjectsSettled()
	})
})

func waitForTargetOperator() {
	expectedImage := os.Getenv("EXPECTED_OPERATOR_IMAGE")
	Expect(expectedImage).NotTo(BeEmpty(), "EXPECTED_OPERATOR_IMAGE is required")

	Eventually(func(g Gomega) {
		g.Expect(managerImage(g)).To(Equal(expectedImage))
		deploymentRolledOut(g, operatorNS, operatorDeploymentName)
		operatorRunning(g)
	}, timeout, interval).Should(Succeed())
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return fallback
}

func ensureWorkbenches() {
	wb := &componentsv1alpha1.Workbenches{
		ObjectMeta: metav1.ObjectMeta{
			Name: componentsv1alpha1.WorkbenchesInstanceName,
		},
		Spec: componentsv1alpha1.WorkbenchesSpec{
			ManagementState:    "Managed",
			WorkbenchNamespace: legacyWorkbenchNamespace,
			Platform:           "OpenDataHub",
		},
	}

	err := k8sClient.Create(ctx, wb)
	if k8serrors.IsAlreadyExists(err) {
		return
	}

	Expect(err).NotTo(HaveOccurred())
}

func getWorkbenches() *componentsv1alpha1.Workbenches {
	wb := &componentsv1alpha1.Workbenches{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name: componentsv1alpha1.WorkbenchesInstanceName,
	}, wb)).To(Succeed())

	return wb
}

func waitForReadyConditions() {
	for _, condType := range []string{"ProvisioningSucceeded", "DeploymentsAvailable", "Ready"} {
		waitForCondition(condType)
	}
}

func waitForCondition(condType string) {
	Eventually(func(g Gomega) {
		operatorRunning(g)

		wb := &componentsv1alpha1.Workbenches{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: componentsv1alpha1.WorkbenchesInstanceName,
		}, wb)).To(Succeed())

		cond := meta.FindStatusCondition(wb.Status.Conditions, condType)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	}, timeout, interval).Should(Succeed())
}

func waitForDeployment(namespace, name string) {
	Eventually(func(g Gomega) {
		deploymentRolledOut(g, namespace, name)
		failIfControllerCrashed(g, namespace, name)
	}, timeout, interval).Should(Succeed())
}

func controllersRunning(g Gomega) {
	failIfControllerCrashed(g, appsNS, notebookControllerDeployment)
	failIfControllerCrashed(g, appsNS, odhNotebookControllerManager)
}

func operatorRunning(g Gomega) {
	failIfControllerCrashed(g, operatorNS, operatorDeploymentName)
}

// failIfControllerCrashed aborts the poll when a controller pod is crash-looping.
// A pod can report Ready and then exit, and waiting out the Notebook timeout hides that.
func failIfControllerCrashed(g Gomega, namespace, name string) {
	deploy := &appsv1.Deployment{}
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name:      name,
		Namespace: namespace,
	}, deploy)).To(Succeed())

	if deploy.Spec.Selector == nil {
		return
	}

	pods := &corev1.PodList{}
	g.Expect(k8sClient.List(ctx, pods,
		client.InNamespace(namespace),
		client.MatchingLabels(deploy.Spec.Selector.MatchLabels),
	)).To(Succeed())

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}

		failIfPodCrashed(pod)
	}
}

func failIfPodCrashed(pod *corev1.Pod) {
	reason := crashReason(pod)
	if reason == "" {
		return
	}

	StopTrying(fmt.Sprintf("%s/%s crashed: %s", pod.Namespace, pod.Name, reason)).
		Attach("recent log", recentControllerLog(pod.Namespace, pod.Name)).
		Now()
}

func crashReason(pod *corev1.Pod) string {
	for _, container := range pod.Status.ContainerStatuses {
		waiting := container.State.Waiting
		if waiting == nil {
			continue
		}

		switch waiting.Reason {
		case "CrashLoopBackOff", "Error", "OOMKilled", "RunContainerError", "ErrImageNeverPull":
			detail := waiting.Message
			exitCode := "unknown"

			if term := container.LastTerminationState.Terminated; term != nil {
				exitCode = strconv.Itoa(int(term.ExitCode))
				if term.Message != "" {
					detail = term.Message
				} else if detail == "" {
					detail = term.Reason
				}
			}

			return fmt.Sprintf("%s %s restarts=%d exit=%s %s", container.Name, waiting.Reason, container.RestartCount, exitCode, detail)
		}
	}

	return ""
}

func recentControllerLog(namespace, podName string) string {
	if kubeClient == nil {
		return ""
	}

	tail := int64(20)
	limit := int64(64 << 10)

	for _, previous := range []bool{true, false} {
		req := kubeClient.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
			TailLines:  &tail,
			LimitBytes: &limit,
			Previous:   previous,
		})

		stream, err := req.Stream(ctx)
		if err != nil {
			continue
		}

		buf, readErr := io.ReadAll(io.LimitReader(stream, limit))
		_ = stream.Close()

		if readErr != nil || len(buf) == 0 {
			continue
		}

		return string(buf)
	}

	return ""
}

func deploymentRolledOut(g Gomega, namespace, name string) {
	deploy := &appsv1.Deployment{}
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name:      name,
		Namespace: namespace,
	}, deploy)).To(Succeed())

	replicas := int32(1)
	if deploy.Spec.Replicas != nil {
		replicas = *deploy.Spec.Replicas
	}

	g.Expect(deploy.Status.ObservedGeneration).To(Equal(deploy.Generation))
	g.Expect(deploy.Status.UpdatedReplicas).To(Equal(replicas))
	g.Expect(deploy.Status.ReadyReplicas).To(Equal(replicas))
}

func managerImage(g Gomega) string {
	deploy := &appsv1.Deployment{}
	g.Expect(k8sClient.Get(ctx, types.NamespacedName{
		Name:      operatorDeploymentName,
		Namespace: operatorNS,
	}, deploy)).To(Succeed())

	for _, container := range deploy.Spec.Template.Spec.Containers {
		if container.Name == "manager" {
			return container.Image
		}
	}

	return ""
}

func waitForWebhookEndpoints() {
	Eventually(func(g Gomega) {
		operatorRunning(g)

		sliceList := &discoveryv1.EndpointSliceList{}
		g.Expect(k8sClient.List(ctx, sliceList,
			client.InNamespace(operatorNS),
			client.MatchingLabelsSelector{
				Selector: labels.SelectorFromSet(labels.Set{
					discoveryv1.LabelServiceName: webhookServiceName,
				}),
			},
		)).To(Succeed())

		ready := false

		for _, slice := range sliceList.Items {
			for _, ep := range slice.Endpoints {
				if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
					ready = true
				}
			}
		}

		g.Expect(ready).To(BeTrue())
	}, timeout, interval).Should(Succeed())
}

func ensureNamespace(name string) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := k8sClient.Create(ctx, ns)
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

func replaceNotebook(name string, annotations map[string]string) {
	existing := newNotebook(name, nil)
	err := k8sClient.Delete(ctx, existing)
	if err != nil && !k8serrors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred())
	}

	if err == nil {
		Eventually(func() bool {
			getErr := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: workloadNS}, existing)

			return k8serrors.IsNotFound(getErr)
		}, timeout, interval).Should(BeTrue())
	}

	Expect(k8sClient.Create(ctx, newNotebook(name, annotations))).To(Succeed())
}

func stopNotebook(name string) {
	Eventually(func(g Gomega) {
		nb, err := getNotebook(name)
		g.Expect(err).NotTo(HaveOccurred())

		annotations := nb.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}

		annotations[stoppedAnnotationKey] = "true"
		nb.SetAnnotations(annotations)
		g.Expect(k8sClient.Update(ctx, nb)).To(Succeed())
	}, timeout, interval).Should(Succeed())
}

func newNotebook(name string, annotations map[string]string) *unstructured.Unstructured {
	nb := &unstructured.Unstructured{}
	nb.SetGroupVersionKind(gvk.Notebook)
	nb.SetName(name)
	nb.SetNamespace(workloadNS)

	if annotations != nil {
		nb.SetAnnotations(annotations)
	}

	Expect(unstructured.SetNestedField(nb.Object, map[string]any{
		"spec": map[string]any{
			"containers": []any{
				map[string]any{
					"name":  name,
					"image": notebookImage,
				},
			},
		},
	}, "spec", "template")).To(Succeed())

	return nb
}

func getNotebook(name string) (*unstructured.Unstructured, error) {
	nb := &unstructured.Unstructured{}
	nb.SetGroupVersionKind(gvk.Notebook)

	err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: workloadNS}, nb)
	if err != nil {
		return nil, err
	}

	return nb, nil
}

func waitForRunningNotebook(name string) {
	Eventually(func(g Gomega) {
		controllersRunning(g)

		sts := &appsv1.StatefulSet{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: workloadNS}, sts)).To(Succeed())
		g.Expect(sts.Status.ReadyReplicas).To(BeNumerically(">=", 1))

		nb, err := getNotebook(name)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(nb.GetAnnotations()[stoppedAnnotationKey]).To(BeEmpty())

		pod, podErr := findNotebookPod(name)
		if podErr == nil {
			failIfPodCrashed(pod)
		}

		g.Expect(podErr).NotTo(HaveOccurred())
		g.Expect(podReady(pod)).To(BeTrue())
	}, timeout, interval).Should(Succeed())
}

func waitForStoppedNotebook(name string) string {
	var annotation string

	Eventually(func(g Gomega) {
		controllersRunning(g)

		sts := &appsv1.StatefulSet{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: workloadNS}, sts)).To(Succeed())

		specReplicas := int32(1)
		if sts.Spec.Replicas != nil {
			specReplicas = *sts.Spec.Replicas
		}

		g.Expect(specReplicas).To(Equal(int32(0)))

		nb, err := getNotebook(name)
		g.Expect(err).NotTo(HaveOccurred())

		annotation = nb.GetAnnotations()[stoppedAnnotationKey]
		g.Expect(annotation).NotTo(BeEmpty())
		g.Expect(annotation).NotTo(Equal(reconciliationLock))
	}, timeout, interval).Should(Succeed())

	return annotation
}

func findNotebookPod(name string) (*corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := k8sClient.List(ctx, list,
		client.InNamespace(workloadNS),
		client.MatchingLabels{notebookNameLabel: name},
	); err != nil {
		return nil, err
	}

	for i := range list.Items {
		pod := &list.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}

		return pod, nil
	}

	return nil, k8serrors.NewNotFound(corev1.Resource("pods"), name)
}

func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}

	return false
}

func restartCount(pod *corev1.Pod) int32 {
	var total int32

	for _, status := range pod.Status.ContainerStatuses {
		total += status.RestartCount
	}

	return total
}

func assertConnectionInjection() {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      probeSecretName,
			Namespace: workloadNS,
			Labels: map[string]string{
				metadata.ManagedAnnotation: metadata.LabelTrue,
			},
		},
		StringData: map[string]string{
			"API_KEY": "test-value",
		},
	}

	err := k8sClient.Create(ctx, secret)
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}

	replaceNotebook(probeNotebookName, map[string]string{
		metadata.ConnectionAnnotation: workloadNS + "/" + probeSecretName,
	})

	Eventually(func(g Gomega) {
		operatorRunning(g)
		controllersRunning(g)

		nb := &unstructured.Unstructured{}
		nb.SetGroupVersionKind(gvk.Notebook)
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      probeNotebookName,
			Namespace: workloadNS,
		}, nb)).To(Succeed())

		containers, found, nestedErr := unstructured.NestedSlice(nb.Object, "spec", "template", "spec", "containers")
		g.Expect(nestedErr).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue())
		g.Expect(containers).NotTo(BeEmpty())

		container, ok := containers[0].(map[string]any)
		g.Expect(ok).To(BeTrue())

		envFrom, envFound, envErr := unstructured.NestedSlice(container, "envFrom")
		g.Expect(envErr).NotTo(HaveOccurred())
		g.Expect(envFound).To(BeTrue())

		matched := false

		for _, item := range envFrom {
			entry, entryOK := item.(map[string]any)
			if !entryOK {
				continue
			}

			secretRef, refFound, refErr := unstructured.NestedMap(entry, "secretRef")
			if refErr != nil || !refFound {
				continue
			}

			if secretRef["name"] == probeSecretName {
				matched = true
			}
		}

		g.Expect(matched).To(BeTrue())
	}, timeout, interval).Should(Succeed())
}

// operandStabilityGVKs are the kinds applyObjects can emit. resourceVersion is
// the wrong signal for most of them: another controller can bump it without
// this operator writing. The field manager timestamp moves when our apply does.
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

func assertAppliedObjectsSettled() {
	admin := []string{
		"notebook-controller-kubeflow-notebooks-admin",
		"odh-notebook-controller-notebooks-admin",
	}
	edit := []string{
		"notebook-controller-kubeflow-notebooks-edit",
		"odh-notebook-controller-notebooks-edit",
	}

	for _, name := range admin {
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

	adminRVs := clusterRoleResourceVersions(admin...)
	editRVs := clusterRoleResourceVersions(edit...)
	generation := getWorkbenches().Generation
	managerTimes := operandFieldManagerTimes(Default)
	Expect(managerTimes).NotTo(BeEmpty())
	Expect(managerTimes).To(HaveKey(operandObjectKey("ClusterRole", "", admin[0])))
	Expect(managerTimes).To(HaveKey(operandObjectKey("ClusterRole", "", admin[1])))

	Consistently(func(g Gomega) {
		g.Expect(workbenchesGeneration(g)).To(Equal(generation),
			"Workbenches generation changed during the stability window")

		adminChanges := clusterRoleResourceVersionDiff(g, adminRVs)
		editChanges := clusterRoleResourceVersionDiff(g, editRVs)
		timeChanges := fieldManagerTimeDiff(managerTimes, operandFieldManagerTimes(g))
		changes := make([]string, 0, len(adminChanges)+len(editChanges)+len(timeChanges))

		for _, line := range adminChanges {
			changes = append(changes, "resourceVersion "+line)
		}

		for _, line := range editChanges {
			changes = append(changes, "resourceVersion "+line)
		}

		changes = append(changes, timeChanges...)

		g.Expect(changes).To(BeEmpty(),
			"applied objects still changing after upgrade:\n%s",
			strings.Join(changes, "\n"))
	}, appliedObjectStability, interval).Should(Succeed())
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
			changes = append(changes, fmt.Sprintf("managedFields %s: added %s", key, now))
		case !hasAfter:
			changes = append(changes, fmt.Sprintf("managedFields %s: removed (was %s)", key, was))
		case was != now:
			changes = append(changes, fmt.Sprintf("managedFields %s: %s -> %s", key, was, now))
		}
	}

	return changes
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

func writeSnapshot(snap upgradeSnapshot) {
	Expect(os.MkdirAll(filepath.Dir(snapshotRelPath), 0o750)).To(Succeed())

	payload, err := json.MarshalIndent(snap, "", "  ")
	Expect(err).NotTo(HaveOccurred())
	Expect(os.WriteFile(snapshotRelPath, append(payload, '\n'), 0o600)).To(Succeed())
}

func readSnapshot() upgradeSnapshot {
	payload, err := os.ReadFile(snapshotRelPath)
	Expect(err).NotTo(HaveOccurred())

	var snap upgradeSnapshot
	Expect(json.Unmarshal(payload, &snap)).To(Succeed())
	Expect(snap.RunningPodUID).NotTo(BeEmpty())
	Expect(snap.ApplicationsNamespace).NotTo(BeEmpty())

	return snap
}
