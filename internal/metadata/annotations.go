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

package metadata

// Annotation keys used for workbenches resources.
const (
	ConnectionAnnotation = "opendatahub.io/connections"

	HardwareProfileNameAnnotation      = "opendatahub.io/hardware-profile-name"
	HardwareProfileNamespaceAnnotation = "opendatahub.io/hardware-profile-namespace"

	AcceleratorNameAnnotation             = "opendatahub.io/accelerator-name"
	AcceleratorProfileNamespaceAnnotation = "opendatahub.io/accelerator-profile-namespace"
	LastSizeSelectionAnnotation           = "notebooks.opendatahub.io/last-size-selection"

	// ManagedAnnotation controls whether the operator fully owns a Deployment.
	// When set to "true" on a live Deployment, container resources and replicas
	// from the rendered manifest are force-applied (user customizations are
	// reverted). When absent or not "true", live resources/replicas are merged
	// onto the rendered manifest before SSA — matching the former in-tree
	// workbenches behavior via opendatahub-operator's MergeDeployments.
	ManagedAnnotation = "opendatahub.io/managed"
)
