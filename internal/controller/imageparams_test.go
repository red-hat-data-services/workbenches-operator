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
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// Tests that stub the package-level lookupEnv variable must not use t.Parallel():
// concurrent tests would race on the shared function pointer.

func TestRelatedImagesFromEnv(t *testing.T) {
	orig := lookupEnv
	t.Cleanup(func() { lookupEnv = orig })

	lookupEnv = func(key string) string {
		switch key {
		case "RELATED_IMAGE_ODH_NOTEBOOK_CONTROLLER_IMAGE":
			return "registry.redhat.io/rhoai/odh-notebook-controller-rhel9@sha256:abc"
		case "RELATED_IMAGE_ODH_KF_NOTEBOOK_CONTROLLER_IMAGE":
			return "  registry.redhat.io/rhoai/odh-kf-notebook-controller-rhel9@sha256:def  "
		case "RELATED_IMAGE_ODH_WORKBENCH_JUPYTER_MINIMAL_CPU_PY312_IMAGE":
			return "registry.redhat.io/rhoai/odh-workbench-jupyter-minimal-cpu-py312-rhel9@sha256:ghi"
		case "RELATED_IMAGE_ODH_KUBE_RBAC_PROXY_IMAGE":
			return "registry.redhat.io/rhoai/odh-kube-rbac-proxy-rhel9@sha256:proxy"
		default:
			return ""
		}
	}

	got := relatedImagesFromEnv()

	want := map[string]string{
		paramODHNotebookControllerImage:                  "registry.redhat.io/rhoai/odh-notebook-controller-rhel9@sha256:abc",
		"odh-kf-notebook-controller-image":               "registry.redhat.io/rhoai/odh-kf-notebook-controller-rhel9@sha256:def",
		"odh-workbench-jupyter-minimal-cpu-py312-ubi9-n": "registry.redhat.io/rhoai/odh-workbench-jupyter-minimal-cpu-py312-rhel9@sha256:ghi",
		"kube-rbac-proxy":                                "registry.redhat.io/rhoai/odh-kube-rbac-proxy-rhel9@sha256:proxy",
		"KUBE_RBAC_PROXY_IMAGE":                          "registry.redhat.io/rhoai/odh-kube-rbac-proxy-rhel9@sha256:proxy",
	}

	if len(got) != len(want) {
		t.Fatalf("relatedImagesFromEnv() len = %d, want %d: %#v", len(got), len(want), got)
	}

	for k, v := range want {
		if got[k] != v {
			t.Errorf("relatedImagesFromEnv()[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestApplyRelatedImageParamsUpdatesControllersAndLatest(t *testing.T) {
	orig := lookupEnv
	t.Cleanup(func() { lookupEnv = orig })

	lookupEnv = func(key string) string {
		switch key {
		case "RELATED_IMAGE_ODH_NOTEBOOK_CONTROLLER_IMAGE":
			return "registry.redhat.io/rhoai/odh-notebook-controller-rhel9@sha256:ctrl"
		case "RELATED_IMAGE_ODH_KUBE_RBAC_PROXY_IMAGE":
			return "registry.redhat.io/rhoai/odh-kube-rbac-proxy-rhel9@sha256:proxy"
		case "RELATED_IMAGE_ODH_WORKBENCH_JUPYTER_MINIMAL_CPU_PY312_IMAGE":
			return "registry.redhat.io/rhoai/odh-workbench-jupyter-minimal-cpu-py312-rhel9@sha256:nb"
		default:
			return ""
		}
	}

	fSys := filesys.MakeFsInMemory()
	dir := "/manifests"

	if err := fSys.Mkdir(dir); err != nil {
		t.Fatal(err)
	}

	paramsEnv := "odh-notebook-controller-image=quay.io/opendatahub/odh-notebook-controller:main\n" +
		"# keep this comment\n" +
		"kube-rbac-proxy=quay.io/opendatahub/odh-kube-auth-proxy@sha256:old\n" +
		"gateway-url=\n" +
		"mlflow-enabled=false\n"

	paramsLatest := "odh-workbench-jupyter-minimal-cpu-py312-ubi9-n=dummy\n" +
		"odh-workbench-jupyter-datascience-cpu-py312-ubi9-n=dummy\n"

	if err := fSys.WriteFile(filepath.Join(dir, "params.env"), []byte(paramsEnv)); err != nil {
		t.Fatal(err)
	}

	if err := fSys.WriteFile(filepath.Join(dir, "params-latest.env"), []byte(paramsLatest)); err != nil {
		t.Fatal(err)
	}

	if err := applyRelatedImageParams(fSys, dir); err != nil {
		t.Fatalf("applyRelatedImageParams() error = %v", err)
	}

	gotParams, err := fSys.ReadFile(filepath.Join(dir, "params.env"))
	if err != nil {
		t.Fatal(err)
	}

	gotLatest, err := fSys.ReadFile(filepath.Join(dir, "params-latest.env"))
	if err != nil {
		t.Fatal(err)
	}

	paramsContent := string(gotParams)
	latestContent := string(gotLatest)

	if !strings.Contains(paramsContent, "odh-notebook-controller-image=registry.redhat.io/rhoai/odh-notebook-controller-rhel9@sha256:ctrl") {
		t.Errorf("params.env controller image not updated:\n%s", paramsContent)
	}

	if !strings.Contains(paramsContent, "kube-rbac-proxy=registry.redhat.io/rhoai/odh-kube-rbac-proxy-rhel9@sha256:proxy") {
		t.Errorf("params.env kube-rbac-proxy not updated:\n%s", paramsContent)
	}

	if !strings.Contains(paramsContent, "# keep this comment") {
		t.Errorf("params.env comment was not preserved:\n%s", paramsContent)
	}

	if !strings.Contains(paramsContent, "gateway-url=") {
		t.Errorf("params.env gateway-url was lost:\n%s", paramsContent)
	}

	if !strings.Contains(latestContent, "odh-workbench-jupyter-minimal-cpu-py312-ubi9-n=registry.redhat.io/rhoai/odh-workbench-jupyter-minimal-cpu-py312-rhel9@sha256:nb") {
		t.Errorf("params-latest.env minimal image not updated:\n%s", latestContent)
	}

	// Unmapped / unset RELATED_IMAGE keys must stay as bundled defaults.
	if !strings.Contains(latestContent, "odh-workbench-jupyter-datascience-cpu-py312-ubi9-n=dummy") {
		t.Errorf("params-latest.env datascience dummy was changed unexpectedly:\n%s", latestContent)
	}
}

func TestApplyRelatedImageParamsUpdatesWorkspacesGatewaySidecar(t *testing.T) {
	orig := lookupEnv
	t.Cleanup(func() { lookupEnv = orig })

	lookupEnv = func(key string) string {
		switch key {
		case "RELATED_IMAGE_ODH_KUBE_RBAC_PROXY_IMAGE":
			return "registry.redhat.io/rhoai/odh-kube-rbac-proxy-rhel9@sha256:proxy"
		case "RELATED_IMAGE_ODH_WORKBENCHES_CONTROLLER_IMAGE":
			return "registry.redhat.io/rhoai/odh-workbenches-controller-rhel9@sha256:ctrl"
		default:
			return ""
		}
	}

	fSys := filesys.MakeFsInMemory()
	dir := "/manifests"

	if err := fSys.Mkdir(dir); err != nil {
		t.Fatal(err)
	}

	// Mirrors workbenches/workspaces-controller/overlays/gateway/params.env.
	paramsEnv := "USE_KUBE_GATEWAY=true\n" +
		"KUBE_GATEWAY_NAME=data-science-gateway\n" +
		"KUBE_GATEWAY_NAMESPACE=openshift-ingress\n" +
		"CLUSTER_DOMAIN=cluster.local\n" +
		"KUBE_RBAC_PROXY_IMAGE=quay.io/opendatahub/odh-kube-auth-proxy@sha256:old\n" +
		"WORKBENCH_CONTROLLER_IMAGE=quay.io/opendatahub/odh-workbenches-controller:odh-stable\n"

	if err := fSys.WriteFile(filepath.Join(dir, "params.env"), []byte(paramsEnv)); err != nil {
		t.Fatal(err)
	}

	if err := applyRelatedImageParams(fSys, dir); err != nil {
		t.Fatalf("applyRelatedImageParams() error = %v", err)
	}

	got, err := fSys.ReadFile(filepath.Join(dir, "params.env"))
	if err != nil {
		t.Fatal(err)
	}

	content := string(got)

	if !strings.Contains(content, "KUBE_RBAC_PROXY_IMAGE=registry.redhat.io/rhoai/odh-kube-rbac-proxy-rhel9@sha256:proxy") {
		t.Errorf("gateway params.env KUBE_RBAC_PROXY_IMAGE not updated:\n%s", content)
	}

	if !strings.Contains(content, "WORKBENCH_CONTROLLER_IMAGE=registry.redhat.io/rhoai/odh-workbenches-controller-rhel9@sha256:ctrl") {
		t.Errorf("gateway params.env WORKBENCH_CONTROLLER_IMAGE not updated:\n%s", content)
	}

	if !strings.Contains(content, "USE_KUBE_GATEWAY=true") {
		t.Errorf("gateway params.env USE_KUBE_GATEWAY was lost:\n%s", content)
	}
}

func TestApplyRelatedImageParamsNoopWhenEnvUnset(t *testing.T) {
	orig := lookupEnv
	t.Cleanup(func() { lookupEnv = orig })

	lookupEnv = func(string) string { return "" }

	fSys := filesys.MakeFsInMemory()
	dir := "/manifests"

	if err := fSys.Mkdir(dir); err != nil {
		t.Fatal(err)
	}

	original := "odh-notebook-controller-image=quay.io/opendatahub/odh-notebook-controller:main\n"
	path := filepath.Join(dir, "params.env")

	if err := fSys.WriteFile(path, []byte(original)); err != nil {
		t.Fatal(err)
	}

	if err := applyRelatedImageParams(fSys, dir); err != nil {
		t.Fatalf("applyRelatedImageParams() error = %v", err)
	}

	got, err := fSys.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != original {
		t.Errorf("params.env changed when no RELATED_IMAGE set\ngot:  %q\nwant: %q", string(got), original)
	}
}

func TestApplyRelatedImageParamsMissingFilesOK(t *testing.T) {
	orig := lookupEnv
	t.Cleanup(func() { lookupEnv = orig })

	lookupEnv = func(key string) string {
		if key == "RELATED_IMAGE_ODH_NOTEBOOK_CONTROLLER_IMAGE" {
			return "registry.redhat.io/rhoai/odh-notebook-controller-rhel9@sha256:ctrl"
		}

		return ""
	}

	fSys := filesys.MakeFsInMemory()
	dir := "/empty"

	if err := fSys.Mkdir(dir); err != nil {
		t.Fatal(err)
	}

	if err := applyRelatedImageParams(fSys, dir); err != nil {
		t.Fatalf("applyRelatedImageParams() with missing params files error = %v", err)
	}
}

func TestOverlayExistingParamsRejectsControlCharacters(t *testing.T) {
	fSys := filesys.MakeFsInMemory()
	dir := "/manifests"

	if err := fSys.Mkdir(dir); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "params.env")
	if err := fSys.WriteFile(path, []byte("odh-notebook-controller-image=old\n")); err != nil {
		t.Fatal(err)
	}

	err := overlayExistingParams(fSys, path, map[string]string{
		paramODHNotebookControllerImage: "bad\nvalue",
	})
	if err == nil {
		t.Fatal("expected error for control characters in related image value")
	}

	err = overlayExistingParams(fSys, path, map[string]string{
		paramODHNotebookControllerImage: "bad\rvalue",
	})
	if err == nil {
		t.Fatal("expected error for \\r control characters in related image value")
	}

	// Unused override keys with bad values must not fail — only applied keys are validated.
	if err := overlayExistingParams(fSys, path, map[string]string{
		"not-in-file": "bad\nvalue",
	}); err != nil {
		t.Fatalf("unused key with control characters should be ignored: %v", err)
	}
}

func TestOverlayExistingParamsPreservesTrailingBlankLines(t *testing.T) {
	fSys := filesys.MakeFsInMemory()
	dir := "/manifests"

	if err := fSys.Mkdir(dir); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "params.env")
	// Two intentional trailing blank lines after the final entry.
	original := "odh-notebook-controller-image=old\n\n\n"
	if err := fSys.WriteFile(path, []byte(original)); err != nil {
		t.Fatal(err)
	}

	if err := overlayExistingParams(fSys, path, map[string]string{
		paramODHNotebookControllerImage: "registry.example.com/nb@sha256:abc",
	}); err != nil {
		t.Fatalf("overlayExistingParams() error = %v", err)
	}

	got, err := fSys.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	want := "odh-notebook-controller-image=registry.example.com/nb@sha256:abc\n\n\n"
	if string(got) != want {
		t.Errorf("trailing blank lines not preserved\ngot:  %q\nwant: %q", string(got), want)
	}
}

func TestOverlayExistingParamsValueContainingEquals(t *testing.T) {
	fSys := filesys.MakeFsInMemory()
	dir := "/manifests"

	if err := fSys.Mkdir(dir); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "params.env")
	if err := fSys.WriteFile(path, []byte("odh-notebook-controller-image=old=value\n")); err != nil {
		t.Fatal(err)
	}

	want := "registry.example.com/nb@sha256:ab=cd"
	if err := overlayExistingParams(fSys, path, map[string]string{
		paramODHNotebookControllerImage: want,
	}); err != nil {
		t.Fatalf("overlayExistingParams() error = %v", err)
	}

	got, err := fSys.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(got), "odh-notebook-controller-image="+want) {
		t.Errorf("value containing '=' not preserved:\n%s", got)
	}
}

func TestOverlayExistingParamsEmptyAndCommentsOnlyNoop(t *testing.T) {
	fSys := filesys.MakeFsInMemory()
	dir := "/manifests"

	if err := fSys.Mkdir(dir); err != nil {
		t.Fatal(err)
	}

	overrides := map[string]string{
		paramODHNotebookControllerImage: "registry.example.com/nb@sha256:abc",
	}

	emptyPath := filepath.Join(dir, "empty.env")
	if err := fSys.WriteFile(emptyPath, []byte("")); err != nil {
		t.Fatal(err)
	}

	if err := overlayExistingParams(fSys, emptyPath, overrides); err != nil {
		t.Fatalf("empty file error = %v", err)
	}

	gotEmpty, err := fSys.ReadFile(emptyPath)
	if err != nil {
		t.Fatal(err)
	}

	if string(gotEmpty) != "" {
		t.Errorf("empty file was rewritten: %q", string(gotEmpty))
	}

	commentsPath := filepath.Join(dir, "comments.env")
	comments := "# only a comment\n\n# another\n"
	if err = fSys.WriteFile(commentsPath, []byte(comments)); err != nil {
		t.Fatal(err)
	}

	if err = overlayExistingParams(fSys, commentsPath, overrides); err != nil {
		t.Fatalf("comments-only file error = %v", err)
	}

	gotComments, err := fSys.ReadFile(commentsPath)
	if err != nil {
		t.Fatal(err)
	}

	if string(gotComments) != comments {
		t.Errorf("comments-only file changed\ngot:  %q\nwant: %q", string(gotComments), comments)
	}
}

func TestOverlayExistingParamsSameValueNoop(t *testing.T) {
	fSys := filesys.MakeFsInMemory()
	dir := "/manifests"

	if err := fSys.Mkdir(dir); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "params.env")
	original := "odh-notebook-controller-image=registry.example.com/nb@sha256:abc\n"
	if err := fSys.WriteFile(path, []byte(original)); err != nil {
		t.Fatal(err)
	}

	if err := overlayExistingParams(fSys, path, map[string]string{
		paramODHNotebookControllerImage: "registry.example.com/nb@sha256:abc",
	}); err != nil {
		t.Fatalf("overlayExistingParams() error = %v", err)
	}

	got, err := fSys.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != original {
		t.Errorf("same-value override should be a no-op\ngot:  %q\nwant: %q", string(got), original)
	}
}
