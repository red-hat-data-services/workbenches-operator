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
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	componentsv1alpha1 "github.com/opendatahub-io/workbenches-operator/api/v1alpha1"
)

var (
	k8sClient  client.Client
	restConfig *rest.Config
	ctx        context.Context
	cancel     context.CancelFunc
	operatorNS string
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Suite")
}

var _ = BeforeSuite(func() {
	ctx, cancel = context.WithCancel(context.Background()) //nolint:fatcontext

	operatorNS = os.Getenv("OPERATOR_NAMESPACE")
	if operatorNS == "" {
		operatorNS = "workbenches-operator-system"
	}

	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" {
		kubeconfigPath = os.Getenv("HOME") + "/.kube/config"
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	Expect(err).NotTo(HaveOccurred())
	restConfig = config

	scheme := runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(componentsv1alpha1.AddToScheme(scheme)).To(Succeed())

	k8sClient, err = client.New(config, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())

	// --- preflight checks ---

	preflightTimeout := 2 * time.Minute
	preflightInterval := 5 * time.Second

	By("Preflight: verifying operator deployment is ready")
	Eventually(func(g Gomega) {
		deploy := &appsv1.Deployment{}
		g.Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name:      "workbenches-operator",
			Namespace: operatorNS,
		}, deploy)).To(Succeed(),
			fmt.Sprintf("operator deployment not found in namespace %q — deploy the operator first or set OPERATOR_NAMESPACE", operatorNS))

		g.Expect(deploy.Status.ReadyReplicas).To(BeNumerically(">=", 1),
			fmt.Sprintf("operator deployment in namespace %q has no ready replicas", operatorNS))
	}, preflightTimeout, preflightInterval).Should(Succeed())

	By("Preflight: verifying operator webhook service has endpoints")
	Eventually(func(g Gomega) {
		sliceList := &discoveryv1.EndpointSliceList{}
		g.Expect(k8sClient.List(ctx, sliceList,
			client.InNamespace(operatorNS),
			client.MatchingLabelsSelector{
				Selector: labels.SelectorFromSet(labels.Set{
					discoveryv1.LabelServiceName: "workbenches-operator-webhook-service",
				}),
			},
		)).To(Succeed(), "failed to list EndpointSlices for webhook service")

		hasReady := false

		for _, slice := range sliceList.Items {
			for _, ep := range slice.Endpoints {
				if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
					hasReady = true

					break
				}
			}
		}

		g.Expect(hasReady).To(BeTrue(),
			"operator webhook service has no ready endpoints — operator pod may not be running")
	}, preflightTimeout, preflightInterval).Should(Succeed())
})

var _ = AfterSuite(func() {
	cancel()
})
