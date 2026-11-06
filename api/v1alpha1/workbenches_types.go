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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Workbenches component constants.
const (
	WorkbenchesComponentName = "workbenches"
	WorkbenchesInstanceName  = "default-" + WorkbenchesComponentName
	WorkbenchesKind          = "Workbenches"

	ManagementStateManaged = "Managed"
	ManagementStateRemoved = "Removed"
)

// WorkbenchesV2Spec configures the workbenches-v2 submodule.
type WorkbenchesV2Spec struct {
	// managementState indicates whether the workbenches-v2 submodule should be deployed.
	// +kubebuilder:default=Removed
	// +kubebuilder:validation:Enum=Managed;Removed
	ManagementState string `json:"managementState,omitempty"`
}

// Ingress identifies a Gateway available for namespace ingress assignment.
type Ingress struct {
	// name is matched against the Namespace's opendatahub.io/ingress-name annotation.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// gatewayName is the name of the parent Gateway.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$"
	GatewayName string `json:"gatewayName"`

	// gatewayNamespace is the namespace of the parent Gateway.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	GatewayNamespace string `json:"gatewayNamespace"`

	// hostname is optional ingress metadata forwarded to the notebook controller.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern="^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*)?$"
	Hostname string `json:"hostname,omitempty"`

	// isDefault selects this entry when the Namespace has no ingress annotation.
	IsDefault bool `json:"isDefault,omitempty"`
}

// ODHNotebookControllerProbeSpec configures workbench liveness and readiness probes.
type ODHNotebookControllerProbeSpec struct {
	// timeoutSeconds is the timeout in seconds enforced on existing liveness and readiness
	// probes of the main workbench container and its injected kube-rbac-proxy sidecar.
	// Startup probes and other probe settings are not changed.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2147483647
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// WorkbenchesSpec defines the desired state of Workbenches.
type WorkbenchesSpec struct {
	// managementState indicates whether this component should be managed by the operator.
	// Set to one of the following values:
	//
	// - "Managed" : the operator is actively managing the component and trying to keep it active.
	//               It will only upgrade the component if it is safe to do so
	//
	// - "Removed" : the operator is actively managing the component and will not install it,
	//               or if it is installed, the operator will try to remove it
	//
	// Valid values are "Managed" and "Removed".
	// +kubebuilder:default=Managed
	// +kubebuilder:validation:Enum=Managed;Removed
	ManagementState string `json:"managementState,omitempty"`

	// workbenchNamespace is a legacy field retained for JupyterHub-era notebook
	// namespaces (for example rhods-notebooks on RHOAI). Notebook-controller
	// operands deploy into the resolved APPLICATIONS_NAMESPACE (falls back by
	// platform to opendatahub or redhat-ods-applications when unset/invalid);
	// this field names the separate legacy notebooks namespace the operator ensures
	// for Notebook CR placement. When unset, defaults to opendatahub (ODH) or
	// rhods-notebooks (SelfManagedRhoai). Immutable after initial creation.
	// +kubebuilder:validation:Pattern="^([a-z0-9]([-a-z0-9]*[a-z0-9])?)?$"
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workbenchNamespace is immutable"
	WorkbenchNamespace string `json:"workbenchNamespace,omitempty"`

	// gatewayDomain is the domain for the data science gateway.
	// Projected by the orchestrator from the platform GatewayConfig.
	// +kubebuilder:validation:MaxLength=253
	GatewayDomain string `json:"gatewayDomain,omitempty"`

	// ingresses are Gateway assignments projected by the orchestrator.
	// An empty list preserves the notebook controller's default Gateway fallback.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:XValidation:rule="self.filter(i, has(i.isDefault) && i.isDefault).size() <= 1",message="at most one ingress can be default"
	Ingresses []Ingress `json:"ingresses,omitempty"`

	// platform identifies the platform type (OpenDataHub, SelfManagedRhoai).
	// Projected by the orchestrator.
	// +kubebuilder:validation:Enum=OpenDataHub;SelfManagedRhoai
	Platform string `json:"platform,omitempty"`

	// mlflowEnabled indicates whether the MLflow integration is active.
	// Projected by the orchestrator from DSC MLflowOperator state.
	MLflowEnabled bool `json:"mlflowEnabled,omitempty"`

	// odhNotebookControllerProbe configures the notebook controller workbench probe policy.
	// When omitted, the notebook controller enforces the Kubernetes default timeout of 1 second.
	// +optional
	ODHNotebookControllerProbe *ODHNotebookControllerProbeSpec `json:"odhNotebookControllerProbe,omitempty"`

	// workbenchesV2 configures the workbenches-v2 submodule.
	// When omitted, workbenches-v2 is not deployed (equivalent to managementState: Removed).
	// +optional
	WorkbenchesV2 *WorkbenchesV2Spec `json:"workbenchesV2,omitempty"`
}

// IsWorkbenchesV2Managed reports whether workbenches-v2 is actively managed.
func (s *WorkbenchesSpec) IsWorkbenchesV2Managed() bool {
	return s.WorkbenchesV2 != nil && s.WorkbenchesV2.ManagementState == ManagementStateManaged
}

// ComponentRelease tracks release metadata for a deployed component.
type ComponentRelease struct {
	// +required
	// +kubebuilder:validation:Required
	Name    string `json:"name"              yaml:"name"`
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
	RepoURL string `json:"repoUrl,omitempty" yaml:"repoUrl,omitempty"`
}

// Distribution reports the distribution context the module has reconciled against.
type Distribution struct {
	// name is the distribution name (e.g. OpenDataHub, SelfManagedRHOAI, Standalone).
	// +kubebuilder:validation:Enum=OpenDataHub;SelfManagedRHOAI;Standalone
	// +kubebuilder:validation:MaxLength=64
	Name string `json:"name,omitempty"`

	// version is the distribution version the module has reconciled against.
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version,omitempty"`
}

// WorkbenchesStatus defines the observed state of Workbenches.
type WorkbenchesStatus struct {
	// conditions represent the latest available observations of the component's state.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// releases tracks the deployed component versions.
	// +patchMergeKey=name
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=name
	Releases []ComponentRelease `json:"releases,omitempty"`

	// observedGeneration is the most recent generation observed by the controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// distribution reflects the distribution context the module has reconciled against.
	Distribution Distribution `json:"distribution,omitempty"`

	// phase is the overall lifecycle phase of the component.
	// +kubebuilder:validation:Enum=Pending;Initializing;Ready;Upgrading;Degraded;Failed
	Phase string `json:"phase,omitempty"`

	// applicationsNamespace is the resolved namespace where notebook-controller
	// operands and the platform ConfigMap are deployed: APPLICATIONS_NAMESPACE
	// when set and DNS-1123 valid; otherwise the platform default (opendatahub
	// for OpenDataHub, redhat-ods-applications for SelfManagedRhoai).
	// +kubebuilder:validation:MaxLength=63
	ApplicationsNamespace string `json:"applicationsNamespace,omitempty"`

	// workbenchNamespace echoes spec.workbenchNamespace (legacy JupyterHub-era
	// notebooks namespace). It is not the operand deploy target; see
	// status.applicationsNamespace.
	WorkbenchNamespace string `json:"workbenchNamespace,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default-workbenches'",message="Workbenches name must be default-workbenches"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Ready"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,description="Reason"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`,description="Phase"

// Workbenches is the Schema for the workbenches API.
type Workbenches struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkbenchesSpec   `json:"spec,omitempty"`
	Status WorkbenchesStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkbenchesList contains a list of Workbenches.
type WorkbenchesList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Workbenches `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Workbenches{}, &WorkbenchesList{})
}
