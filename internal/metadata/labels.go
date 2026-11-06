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

// Package metadata provides label and annotation constants used across the operator.
package metadata

// Label keys used for workbenches resources.
const (
	OwnedNamespaceLabel = "opendatahub.io/generated-namespace"
	ComponentLabelKey   = "app.opendatahub.io/workbenches"
	PartOfLabelKey      = "app.kubernetes.io/part-of"
	ComponentLabelValue = "workbenches"
	LabelTrue           = "true"

	// ComponentNameLabelKey identifies which component a rendered object
	// belongs to; notebook controller deployments are the consumers of the
	// ingress contract ConfigMap.
	ComponentNameLabelKey = "component.opendatahub.io/name"
)
