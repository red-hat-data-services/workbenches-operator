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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func registerImageStreamStatusTests() {
	Context("ImageStream status", Label("lifecycle"), func() {
		It("Should report ImageStreamsAvailable in status conditions", func() {
			Eventually(func(g Gomega) {
				wb := getWorkbenches()
				cond := meta.FindStatusCondition(wb.Status.Conditions, "ImageStreamsAvailable")
				g.Expect(cond).NotTo(BeNil(),
					"ImageStreamsAvailable condition should exist when ImageStream API is available")
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue),
					"ImageStreamsAvailable should be True when managed ImageStreams are healthy")
			}, timeout, interval).Should(Succeed())
		})
	})
}
