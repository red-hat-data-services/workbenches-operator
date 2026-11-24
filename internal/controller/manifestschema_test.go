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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apivalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// schemaGVK identifies one served CRD version.
type schemaGVK struct {
	group   string
	version string
	kind    string
}

// crdSchema is the OpenAPI and CEL view of one served CRD version.
type crdSchema struct {
	source     string
	structural *structuralschema.Structural
	openAPI    apivalidation.SchemaValidator
	cel        *cel.Validator
}

// TestBundledCustomResourcesMatchCRDSchemas checks custom resources under
// opt/manifests against the CRD schema shipped next to them. Operator-owned
// manifests (workspacekinds/) are kept across upstream refresh, so a CRD bump
// can reject them while kustomize render still succeeds. Upstream samples are
// skipped: commented-out sections there decode as null and those files are not
// applied.
func TestBundledCustomResourcesMatchCRDSchemas(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..", "opt", "manifests")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("opt/manifests not found — run 'make manifests-fetch' and commit the result: %v", err)
	}

	schemas, err := loadBundledCRDSchemas(root)
	if err != nil {
		t.Fatalf("loading CRD schemas: %v", err)
	}

	if len(schemas) == 0 {
		t.Fatal("no CRD schemas found under opt/manifests")
	}

	validated := 0

	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if entry.IsDir() || !isYAML(entry.Name()) || skipSchemaPath(path) {
			return nil
		}

		objects, decodeErr := decodeYAMLObjects(path)
		if decodeErr != nil {
			t.Errorf("%s: %v", relManifest(root, path), decodeErr)

			return nil
		}

		for i, obj := range objects {
			gvk, ok := objectGVK(obj)
			if !ok {
				continue
			}

			schema, found := schemas[gvk]
			if !found {
				continue
			}

			validated++

			problems := validateObject(t.Context(), schema, obj)
			if len(problems) == 0 {
				continue
			}

			t.Errorf("%s document %d (%s %s): %s", relManifest(root, path), i, gvk.kind, objName(obj), strings.Join(problems, "; "))
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking manifests: %v", err)
	}

	if validated == 0 {
		t.Fatal("no custom resources were validated; the CRD index did not match any bundled objects")
	}
}

// TestManifestSchemaValidatorRejectsDriftedFields locks the failure mode this
// check exists for: a required field renamed out from under a bundled object,
// leaving the old field to be pruned on apply.
func TestManifestSchemaValidatorRejectsDriftedFields(t *testing.T) {
	t.Parallel()

	props := &apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"spec": {
				Type:     "object",
				Required: []string{"volumeMountPaths"},
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"volumeMountPaths": {Type: "object"},
					"match":            {Type: "array"},
				},
			},
		},
	}

	schema, err := compileCRDSchema("fixture", props)
	if err != nil {
		t.Fatalf("compile fixture schema: %v", err)
	}

	obj := map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "Widget",
		"spec": map[string]any{
			"volumeMounts": map[string]any{"home": "/opt/app-root/src"},
			"match":        map[string]any{},
		},
	}

	problems := validateObject(t.Context(), schema, obj)
	joined := strings.Join(problems, "\n")

	if !strings.Contains(joined, "spec.volumeMounts") {
		t.Errorf("problems = %q, want dropped spec.volumeMounts", joined)
	}

	if !strings.Contains(joined, "volumeMountPaths") {
		t.Errorf("problems = %q, want missing volumeMountPaths", joined)
	}

	if !strings.Contains(joined, "match") {
		t.Errorf("problems = %q, want match type failure", joined)
	}
}

func loadBundledCRDSchemas(root string) (map[schemaGVK]*crdSchema, error) {
	schemas := map[schemaGVK]*crdSchema{}

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if entry.IsDir() || !isYAML(entry.Name()) || skipSchemaPath(path) {
			return nil
		}

		docs, err := readYAMLDocuments(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}

		for _, doc := range docs {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			if err := yaml.Unmarshal(doc, crd); err != nil || crd.Kind != "CustomResourceDefinition" {
				continue
			}

			if crd.Annotations["crd/fake"] == "true" {
				continue
			}

			for i := range crd.Spec.Versions {
				version := &crd.Spec.Versions[i]
				if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil || !version.Served {
					continue
				}

				gvk := schemaGVK{
					group:   crd.Spec.Group,
					version: version.Name,
					kind:    crd.Spec.Names.Kind,
				}

				compiled, err := compileCRDSchema(path, version.Schema.OpenAPIV3Schema)
				if err != nil {
					return fmt.Errorf("compiling %s %s/%s %s: %w", relManifest(root, path), gvk.group, gvk.version, gvk.kind, err)
				}

				if existing, ok := schemas[gvk]; ok {
					return fmt.Errorf("duplicate CRD for %s/%s %s in %s and %s", gvk.group, gvk.version, gvk.kind, existing.source, path)
				}

				schemas[gvk] = compiled
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return schemas, nil
}

func compileCRDSchema(source string, props *apiextensionsv1.JSONSchemaProps) (*crdSchema, error) {
	internal := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(props, internal, nil); err != nil {
		return nil, fmt.Errorf("converting CRD schema: %w", err)
	}

	structural, err := structuralschema.NewStructural(internal)
	if err != nil {
		return nil, fmt.Errorf("structural schema: %w", err)
	}

	openAPI, _, err := apivalidation.NewSchemaValidator(internal)
	if err != nil {
		return nil, fmt.Errorf("openapi schema: %w", err)
	}

	return &crdSchema{
		source:     source,
		structural: structural,
		openAPI:    openAPI,
		cel:        cel.NewValidator(structural, true, celconfig.PerCallLimit),
	}, nil
}

func validateObject(ctx context.Context, schema *crdSchema, obj map[string]any) []string {
	original := runtime.DeepCopyJSON(obj)
	pruned := runtime.DeepCopyJSON(obj)

	pruning.Prune(pruned, schema.structural, true)

	var problems []string

	for _, path := range append(
		droppedPaths(original["spec"], pruned["spec"], "spec"),
		droppedPaths(original["status"], pruned["status"], "status")...,
	) {
		problems = append(problems, "field "+path+" is not in the CRD schema and would be dropped on apply")
	}

	for _, err := range apivalidation.ValidateCustomResource(field.NewPath(""), pruned, schema.openAPI) {
		problems = append(problems, err.Error())
	}

	if schema.cel != nil {
		celErrs, _ := schema.cel.Validate(ctx, field.NewPath(""), schema.structural, pruned, nil, celconfig.RuntimeCELCostBudget)
		for _, err := range celErrs {
			problems = append(problems, err.Error())
		}
	}

	slices.Sort(problems)

	return problems
}

func droppedPaths(before, after any, prefix string) []string {
	if before == nil {
		return nil
	}

	switch typed := before.(type) {
	case map[string]any:
		afterMap, ok := after.(map[string]any)
		if !ok {
			return []string{prefix}
		}

		var dropped []string

		for key, value := range typed {
			path := prefix + "." + key

			next, exists := afterMap[key]
			if !exists {
				dropped = append(dropped, path)

				continue
			}

			dropped = append(dropped, droppedPaths(value, next, path)...)
		}

		slices.Sort(dropped)

		return dropped
	case []any:
		afterSlice, ok := after.([]any)
		if !ok || len(afterSlice) != len(typed) {
			return []string{prefix}
		}

		var dropped []string

		for i, value := range typed {
			dropped = append(dropped, droppedPaths(value, afterSlice[i], fmt.Sprintf("%s[%d]", prefix, i))...)
		}

		return dropped
	default:
		return nil
	}
}

func decodeYAMLObjects(path string) ([]map[string]any, error) {
	docs, err := readYAMLDocuments(path)
	if err != nil {
		return nil, err
	}

	objects := make([]map[string]any, 0, len(docs))

	for _, doc := range docs {
		jsonBytes, err := yaml.YAMLToJSON(doc)
		if err != nil {
			return nil, fmt.Errorf("converting YAML to JSON: %w", err)
		}

		jsonBytes = bytes.TrimSpace(jsonBytes)
		// Kustomize patches and config files are not custom resources.
		if len(jsonBytes) == 0 || jsonBytes[0] != '{' || bytes.Equal(jsonBytes, []byte("null")) {
			continue
		}

		var probe map[string]any
		if err := json.Unmarshal(jsonBytes, &probe); err != nil {
			return nil, fmt.Errorf("decoding object: %w", err)
		}

		kind, _ := probe["kind"].(string)
		apiVersion, _ := probe["apiVersion"].(string)
		if kind == "" || apiVersion == "" {
			continue
		}

		obj := &unstructured.Unstructured{}
		if err := obj.UnmarshalJSON(jsonBytes); err != nil {
			return nil, fmt.Errorf("decoding object: %w", err)
		}

		if len(obj.Object) == 0 {
			continue
		}

		objects = append(objects, obj.Object)
	}

	return objects, nil
}

func readYAMLDocuments(path string) ([][]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // test reads repository manifests
	if err != nil {
		return nil, err
	}

	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	docs := make([][]byte, 0, 1)

	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return docs, nil
		}

		if err != nil {
			return nil, err
		}

		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}

		docs = append(docs, doc)
	}
}

func objectGVK(obj map[string]any) (schemaGVK, bool) {
	apiVersion, _ := obj["apiVersion"].(string)
	kind, _ := obj["kind"].(string)

	if apiVersion == "" || kind == "" || kind == "CustomResourceDefinition" {
		return schemaGVK{}, false
	}

	group, version, ok := strings.Cut(apiVersion, "/")
	if !ok {
		return schemaGVK{version: group, kind: kind}, true
	}

	return schemaGVK{group: group, version: version, kind: kind}, true
}

func objName(obj map[string]any) string {
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)

	if name == "" {
		return "<unnamed>"
	}

	return name
}

func skipSchemaPath(path string) bool {
	slash := filepath.ToSlash(path)

	return strings.Contains(slash, "/crd/external/") ||
		strings.Contains(slash, "/patches/") ||
		strings.Contains(slash, "/samples/")
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

func relManifest(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}

	return rel
}
