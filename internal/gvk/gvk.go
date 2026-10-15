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

// Package gvk defines GroupVersionKind constants for resources the operator interacts with.
package gvk

import (
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Deployment is the GVK for apps Deployment resources.
var Deployment = appsv1.SchemeGroupVersion.WithKind("Deployment")

// Notebook is the GVK for Kubeflow Notebook resources.
var Notebook = schema.GroupVersionKind{
	Group:   "kubeflow.org",
	Version: "v1",
	Kind:    "Notebook",
}

// ImageStream is the GVK for OpenShift ImageStream resources.
var ImageStream = schema.GroupVersionKind{
	Group:   "image.openshift.io",
	Version: "v1",
	Kind:    "ImageStream",
}

// HardwareProfile is the GVK for ODH HardwareProfile resources.
var HardwareProfile = schema.GroupVersionKind{
	Group:   "infrastructure.opendatahub.io",
	Version: "v1",
	Kind:    "HardwareProfile",
}

// Namespace is the GVK for core Namespace resources.
var Namespace = schema.GroupVersionKind{
	Group:   "",
	Version: "v1",
	Kind:    "Namespace",
}
