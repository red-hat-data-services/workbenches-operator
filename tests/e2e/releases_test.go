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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func registerReleaseMetadataTests() {
	Context("Release metadata", Label("lifecycle"), func() {
		It("Should expose non-empty status.releases entries", func() {
			Eventually(func(g Gomega) {
				wb := getWorkbenches()

				populated := 0

				for _, release := range wb.Status.Releases {
					if strings.TrimSpace(release.Name) != "" &&
						strings.TrimSpace(release.Version) != "" &&
						strings.TrimSpace(release.RepoURL) != "" {
						populated++
					}
				}

				g.Expect(populated).To(BeNumerically(">", 0),
					"expected at least one release with name, version, and repoUrl")
			}, timeout, interval).Should(Succeed())
		})

		It("Should include Kubeflow Notebook Controller release metadata", func() {
			wb := getWorkbenches()

			var found bool

			for _, release := range wb.Status.Releases {
				if release.Name == "Kubeflow Notebook Controller" {
					found = true
					Expect(release.Version).NotTo(BeEmpty())
					Expect(release.RepoURL).To(ContainSubstring("kubeflow"))

					break
				}
			}

			Expect(found).To(BeTrue(), "Kubeflow Notebook Controller release should be reported")
		})
	})
}
