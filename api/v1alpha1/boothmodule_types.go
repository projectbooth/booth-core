package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UIIntegrationMode mirrors contracts/module-manifest.md's uiIntegrationMode field,
// resolved to exactly these two values by ADR 0005.
type UIIntegrationMode string

const (
	UIIntegrationModeNative      UIIntegrationMode = "native"
	UIIntegrationModeIframeProxy UIIntegrationMode = "iframe-proxy"
)

// NavGroup mirrors the shell nav taxonomy fixed by ADR 0017.
type NavGroup string

const (
	NavGroupBuild  NavGroup = "build"
	NavGroupView   NavGroup = "view"
	NavGroupManage NavGroup = "manage"
)

// ServiceReference points at the in-cluster Service backing a module's health check
// and gateway routing. This is not part of contracts/module-manifest.md — that contract
// only covers what a module declares about itself. A module's Helm chart adds this
// alongside the manifest fields so core's gateway has somewhere to actually route to.
// +kubebuilder:object:generate=true
type ServiceReference struct {
	// Name of the Kubernetes Service backing this module.
	Name string `json:"name"`
	// Namespace the Service lives in. Defaults to the BoothModule resource's own
	// namespace if empty.
	Namespace string `json:"namespace,omitempty"`
	// Port the Service listens on for both the health check and gateway routing.
	Port int32 `json:"port"`
}

// BoothModuleSpec is the Kubernetes-native transport for contracts/module-manifest.md.
// Field-for-field, this must match that contract's table.
// +kubebuilder:object:generate=true
type BoothModuleSpec struct {
	// ID is the stable, unique module id, e.g. "storage", "catalog". Matches the
	// manifest contract's `id` field.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]*$`
	ID string `json:"id"`

	// DisplayName is the human-readable name shown in the shell UI.
	// +kubebuilder:validation:Required
	DisplayName string `json:"displayName"`

	// Icon is a free-form icon identifier. Unrecognized values fall back to a
	// default glyph in the shell.
	// +optional
	Icon string `json:"icon,omitempty"`

	// Version is the module's own release version (semver).
	// +kubebuilder:validation:Required
	Version string `json:"version"`

	// ContractVersion is the module-manifest.md + core-platform-api.md contract
	// version this module targets. Core supports current and previous major.
	// +kubebuilder:validation:Required
	ContractVersion string `json:"contractVersion"`

	// HasOwnUI declares whether this module ships a UI to surface in the shell.
	HasOwnUI bool `json:"hasOwnUi"`

	// UIIntegrationMode is required when HasOwnUI is true: native or iframe-proxy
	// (ADR 0005).
	// +kubebuilder:validation:Enum=native;iframe-proxy
	// +optional
	UIIntegrationMode UIIntegrationMode `json:"uiIntegrationMode,omitempty"`

	// HealthCheckPath is the path core polls (via ServiceRef) to determine module
	// health/availability.
	// +kubebuilder:validation:Required
	HealthCheckPath string `json:"healthCheckPath"`

	// RequiredScopes lists auth scopes the module needs core to grant it access to.
	// +optional
	RequiredScopes []string `json:"requiredScopes,omitempty"`

	// NavPath is where this module attaches in the shell's navigation. Required
	// when HasOwnUI is true.
	// +optional
	NavPath string `json:"navPath,omitempty"`

	// NavGroup determines which of the shell's three nav sections (build/view/
	// manage) the module's entry appears under (ADR 0017). Required when HasOwnUI
	// is true.
	// +kubebuilder:validation:Enum=build;view;manage
	// +optional
	NavGroup NavGroup `json:"navGroup,omitempty"`

	// AdminNavPath is an optional distinct route the shell renders for a user with
	// an admin/owner-level workspace role, alongside NavPath for everyone else
	// (ADR 0023). Most modules should omit this.
	// +optional
	AdminNavPath string `json:"adminNavPath,omitempty"`

	// ServiceRef points at the in-cluster Service backing this module. Not part of
	// the manifest contract itself — infrastructure the chart adds so the gateway
	// and health-check reconciler have somewhere to route to.
	// +kubebuilder:validation:Required
	ServiceRef ServiceReference `json:"serviceRef"`
}

// ModulePhase summarizes a module's observed health, derived from polling
// HealthCheckPath.
type ModulePhase string

const (
	ModulePhaseUnknown     ModulePhase = "Unknown"
	ModulePhaseHealthy     ModulePhase = "Healthy"
	ModulePhaseUnhealthy   ModulePhase = "Unhealthy"
	ModulePhaseUnreachable ModulePhase = "Unreachable"
)

// BoothModuleStatus is populated by booth-core's registry controller, never by the
// module itself.
// +kubebuilder:object:generate=true
type BoothModuleStatus struct {
	// Phase is the last-observed health status.
	// +optional
	Phase ModulePhase `json:"phase,omitempty"`

	// Message carries human-readable detail, e.g. a health-check error.
	// +optional
	Message string `json:"message,omitempty"`

	// LastHealthCheckTime is when Phase was last updated.
	// +optional
	LastHealthCheckTime *metav1.Time `json:"lastHealthCheckTime,omitempty"`

	// ObservedGeneration lets a reader tell whether Status reflects the most
	// recent Spec.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Display Name",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// BoothModule is the Kubernetes-native transport for a module's manifest
// (contracts/module-manifest.md), per ADR 0019. One instance per installed module,
// created/updated by that module's Helm chart on install, removed on uninstall.
type BoothModule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BoothModuleSpec   `json:"spec,omitempty"`
	Status BoothModuleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BoothModuleList contains a list of BoothModule.
type BoothModuleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BoothModule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BoothModule{}, &BoothModuleList{})
}
