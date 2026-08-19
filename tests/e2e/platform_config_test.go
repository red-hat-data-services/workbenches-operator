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

	"github.com/opendatahub-io/workbenches-operator/internal/platformconfig"
)

func registerPlatformConfigTests() {
	Context("Platform config handshake", Label("lifecycle"), func() {
		It("Should record platformVersion in status.releases", func() {
			upsertPlatformConfig(
				platformconfig.DistributionNameStandalone,
				"1.0.0",
				"e2e-platform-v1",
			)

			waitForPlatformReleaseVersion("e2e-platform-v1")
			waitForCondition("Ready", metav1.ConditionTrue)
		})

		It("Should transition platformVersion in status.releases without losing operand health", func() {
			upsertPlatformConfig(
				platformconfig.DistributionNameStandalone,
				"1.0.0",
				"e2e-platform-v2",
			)

			waitForPlatformReleaseVersion("e2e-platform-v2")
			waitForCondition("Ready", metav1.ConditionTrue)
			waitForCondition("DeploymentsAvailable", metav1.ConditionTrue)
		})
	})
}
