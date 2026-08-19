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

func registerUpgradeTests() {
	Context("Distribution upgrade", Label("lifecycle"), func() {
		It("Should align status.distribution after a distribution.version bump", func() {
			upsertPlatformConfig(
				platformconfig.DistributionNameStandalone,
				"2.0.0",
				"e2e-platform-v2",
			)

			waitForDistributionStatus(platformconfig.DistributionNameStandalone, "2.0.0")
			waitForCondition("Ready", metav1.ConditionTrue)
			waitForCondition("DeploymentsAvailable", metav1.ConditionTrue)
		})
	})
}
