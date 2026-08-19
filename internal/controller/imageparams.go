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

package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// lookupEnv resolves RELATED_IMAGE_* (and other) environment variables.
// Overridable in unit tests.
var lookupEnv = os.Getenv

// paramsEnvFiles are the params files kustomize consumes for image injection.
// Controllers use params.env; notebook ImageStreams also use params-latest.env
// for the rolling "-n" / latest tags (product builds ship those as "dummy"
// until RELATED_IMAGE_* is applied).
var paramsEnvFiles = []string{"params.env", "params-latest.env"}

// params.env / params-latest.env keys referenced from tests often enough to
// trip goconst when left as bare string literals in imageParamMap.
const paramODHNotebookControllerImage = "odh-notebook-controller-image"

// imageParamMap maps keys in params.env / params-latest.env to RELATED_IMAGE_*
// environment variables injected onto the module-operator pod by the platform
// (opendatahub-operator injectModuleEnv). Ported from the former in-tree
// workbenches component Init() ApplyParams maps.
//
// Keep this map aligned with upstream params keys and with the platform module
// handler relatedImages list — see DEPENDENCIES.md ("Upgrading Upstream Manifests").
var imageParamMap = map[string]string{
	// Notebook controllers
	paramODHNotebookControllerImage:    "RELATED_IMAGE_ODH_NOTEBOOK_CONTROLLER_IMAGE",
	"kube-rbac-proxy":                  "RELATED_IMAGE_ODH_KUBE_RBAC_PROXY_IMAGE",
	"odh-kf-notebook-controller-image": "RELATED_IMAGE_ODH_KF_NOTEBOOK_CONTROLLER_IMAGE",

	// CodeServer workbench (latest / -n tag)
	"odh-workbench-codeserver-datascience-cpu-py312-ubi9-n": "RELATED_IMAGE_ODH_WORKBENCH_CODESERVER_DATASCIENCE_CPU_PY312_IMAGE",

	// Jupyter workbenches (latest / -n tags)
	"odh-workbench-jupyter-datascience-cpu-py312-ubi9-n":            "RELATED_IMAGE_ODH_WORKBENCH_JUPYTER_DATASCIENCE_CPU_PY312_IMAGE",
	"odh-workbench-jupyter-minimal-cpu-py312-ubi9-n":                "RELATED_IMAGE_ODH_WORKBENCH_JUPYTER_MINIMAL_CPU_PY312_IMAGE",
	"odh-workbench-jupyter-minimal-cuda-py312-ubi9-n":               "RELATED_IMAGE_ODH_WORKBENCH_JUPYTER_MINIMAL_CUDA_PY312_IMAGE",
	"odh-workbench-jupyter-minimal-rocm-py312-ubi9-n":               "RELATED_IMAGE_ODH_WORKBENCH_JUPYTER_MINIMAL_ROCM_PY312_IMAGE",
	"odh-workbench-jupyter-pytorch-cuda-py312-ubi9-n":               "RELATED_IMAGE_ODH_WORKBENCH_JUPYTER_PYTORCH_CUDA_PY312_IMAGE",
	"odh-workbench-jupyter-pytorch-rocm-py312-ubi9-n":               "RELATED_IMAGE_ODH_WORKBENCH_JUPYTER_PYTORCH_ROCM_PY312_IMAGE",
	"odh-workbench-jupyter-tensorflow-cuda-py312-ubi9-n":            "RELATED_IMAGE_ODH_WORKBENCH_JUPYTER_TENSORFLOW_CUDA_PY312_IMAGE",
	"odh-workbench-jupyter-tensorflow-rocm-py312-ubi9-n":            "RELATED_IMAGE_ODH_WORKBENCH_JUPYTER_TENSORFLOW_ROCM_PY312_IMAGE",
	"odh-workbench-jupyter-trustyai-cpu-py312-ubi9-n":               "RELATED_IMAGE_ODH_WORKBENCH_JUPYTER_TRUSTYAI_CPU_PY312_IMAGE",
	"odh-workbench-jupyter-pytorch-llmcompressor-cuda-py312-ubi9-n": "RELATED_IMAGE_ODH_WORKBENCH_JUPYTER_PYTORCH_LLMCOMPRESSOR_CUDA_PY312_IMAGE",

	// Pipeline runtimes (latest / -n tags)
	"odh-pipeline-runtime-datascience-cpu-py312-ubi9-n":            "RELATED_IMAGE_ODH_PIPELINE_RUNTIME_DATASCIENCE_CPU_PY312_IMAGE",
	"odh-pipeline-runtime-minimal-cpu-py312-ubi9-n":                "RELATED_IMAGE_ODH_PIPELINE_RUNTIME_MINIMAL_CPU_PY312_IMAGE",
	"odh-pipeline-runtime-tensorflow-cuda-py312-ubi9-n":            "RELATED_IMAGE_ODH_PIPELINE_RUNTIME_TENSORFLOW_CUDA_PY312_IMAGE",
	"odh-pipeline-runtime-tensorflow-rocm-py312-ubi9-n":            "RELATED_IMAGE_ODH_PIPELINE_RUNTIME_TENSORFLOW_ROCM_PY312_IMAGE",
	"odh-pipeline-runtime-pytorch-cuda-py312-ubi9-n":               "RELATED_IMAGE_ODH_PIPELINE_RUNTIME_PYTORCH_CUDA_PY312_IMAGE",
	"odh-pipeline-runtime-pytorch-rocm-py312-ubi9-n":               "RELATED_IMAGE_ODH_PIPELINE_RUNTIME_PYTORCH_ROCM_PY312_IMAGE",
	"odh-pipeline-runtime-pytorch-llmcompressor-cuda-py312-ubi9-n": "RELATED_IMAGE_ODH_PIPELINE_RUNTIME_PYTORCH_LLMCOMPRESSOR_CUDA_PY312_IMAGE",

	// Workspaces controller
	"WORKBENCH_CONTROLLER_IMAGE": "RELATED_IMAGE_ODH_WORKBENCHES_CONTROLLER_IMAGE",
}

// relatedImagesFromEnv returns params-file key → image ref for every mapped
// RELATED_IMAGE_* env var that is set and non-empty on the operator process.
func relatedImagesFromEnv() map[string]string {
	out := make(map[string]string, len(imageParamMap))

	for paramKey, envName := range imageParamMap {
		if v := strings.TrimSpace(lookupEnv(envName)); v != "" {
			out[paramKey] = v
		}
	}

	return out
}

// applyRelatedImageParams overlays RELATED_IMAGE_* values onto existing keys in
// params.env and params-latest.env under kustomizeDir. Missing files are
// skipped. Keys that are not already present in a file are not added (matches
// former in-tree odhdeploy.ApplyParams behavior).
func applyRelatedImageParams(fSys filesys.FileSystem, kustomizeDir string) error {
	images := relatedImagesFromEnv()
	if len(images) == 0 {
		return nil
	}

	for _, name := range paramsEnvFiles {
		path := filepath.Join(kustomizeDir, name)
		if err := overlayExistingParams(fSys, path, images); err != nil {
			return fmt.Errorf("applying related images to %s: %w", name, err)
		}
	}

	return nil
}

// overlayExistingParams updates values for keys that already exist in the
// params file. Does nothing when the file is absent. Blank lines and comments
// are preserved.
func overlayExistingParams(fSys filesys.FileSystem, paramsPath string, overrides map[string]string) error {
	if !fSys.Exists(paramsPath) {
		return nil
	}

	data, err := fSys.ReadFile(paramsPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", paramsPath, err)
	}

	existing := make(map[string]string)
	updated := false

	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		k, v, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}

		existing[k] = v
	}

	for k, newVal := range overrides {
		old, found := existing[k]
		if !found || old == newVal {
			continue
		}

		// Validate only keys that would actually be written (ignore unused
		// override keys and no-op same-value updates).
		if strings.ContainsAny(newVal, "\n\r") {
			return fmt.Errorf("related image value for key %q contains invalid control characters", k)
		}

		existing[k] = newVal
		updated = true
	}

	if !updated {
		return nil
	}

	var out []string

	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out = append(out, line)
			continue
		}

		k, _, ok := strings.Cut(trimmed, "=")
		if !ok {
			out = append(out, line)
			continue
		}

		if v, ok := existing[k]; ok {
			out = append(out, k+"="+v)
			continue
		}

		out = append(out, line)
	}

	// SplitSeq keeps a trailing empty element when the file ended with '\n',
	// which Join preserves as a terminating newline. Extra trailing blank
	// lines (additional empty elements) are kept as-is. Only append '\n'
	// when the original file had no trailing newline.
	content := strings.Join(out, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	return fSys.WriteFile(paramsPath, []byte(content))
}
