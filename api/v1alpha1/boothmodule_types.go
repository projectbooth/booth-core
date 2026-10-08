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

	// Events declares which event-bus subjects this module may publish and subscribe to
	// (ADR 0049). Core derives the module's NATS credentials — and their per-subject
	// publish permissions — from exactly this, and provisions them as a Secret in the
	// module's namespace. A module that omits it gets no bus credentials at all, i.e. it
	// cannot connect to the bus. Proposed manifest addition, pending an architecture ADR
	// (see docs/decisions/0006-event-bus-authentication.md).
	// +optional
	Events *EventBusAccess `json:"events,omitempty"`

	// Database signals that this module needs its own PostgreSQL database (ADR 0053).
	// Core then creates a database and role scoped to this module on the shared cluster and
	// delivers the connection details as a Secret in the module's namespace, ahead of the
	// module needing them. Omit it and the module gets nothing. Core provisions the
	// database only; what a module puts in it (schema, migrations) is the module's own.
	// Proposed manifest addition, pending an architecture ADR (see
	// docs/decisions/0008-shared-postgres.md).
	// +optional
	Database *DatabaseRequirement `json:"database,omitempty"`

	// WorkloadIdentity signals that this module runs unattended work (a scheduled job, no
	// human present) that must call other modules on a workspace's behalf (ADR 0056). Core
	// then provisions a minting credential as the Secret `booth-workload-minting-credentials`
	// in the module's namespace, which the module presents to
	// POST /api/internal/workload-tokens. Omit it and the module never receives a credential
	// and cannot mint anything.
	// +optional
	WorkloadIdentity *WorkloadIdentityRequirement `json:"workloadIdentity,omitempty"`

	// ProvidesCredentials declares which native-protocol credential kind(s) (e.g. "s3",
	// "postgres") this module provides through the credential broker (ADR 0080). Core provisions
	// a broker-calling credential as the Secret `booth-credential-broker-provider-credentials`
	// in the module's namespace, and routes an authorized `POST /api/credentials` request for a
	// declared kind to this module. Omit it and the module never receives that Secret and is
	// never routed to. A module that only ever *consumes* a credential (calls the broker as
	// itself, or on behalf of a task holding a workload token) needs no manifest declaration at
	// all — the broker's authorization is ordinary identity/workspace/role, the same as any
	// other authenticated route.
	// +optional
	ProvidesCredentials *CredentialProviderSpec `json:"providesCredentials,omitempty"`

	// PublicRoutes declares module-relative path prefixes core's gateway proxies without a
	// platform login, at /modules/{id}/public/<prefix> (ADR 0101). Core performs no
	// authentication on these routes, strips any inbound X-Booth-Workspace/X-Booth-Role/
	// X-Booth-Identity and sets none of them, and never writes to the user directory for them —
	// the module authenticates callers itself and returns its own 401/403. Omit it and the
	// module gets no public route at all; the ordinary /modules/{id}/* route is unaffected
	// either way.
	// +optional
	PublicRoutes *PublicRoutesSpec `json:"publicRoutes,omitempty"`
}

// PublicRoutesSpec is the manifest-level declaration of a module's unauthenticated route
// prefixes (ADR 0101). A struct rather than a bare list so it can grow (e.g. per-prefix rate
// limits) without another manifest field.
type PublicRoutesSpec struct {
	// PathPrefixes lists module-relative path prefixes proxied without a platform login. Each
	// must start and end with "/" (e.g. "/v1/") — contracts/module-manifest.md's exact grammar.
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^/.*/$`
	PathPrefixes []string `json:"pathPrefixes"`
}

// WorkloadIdentityRequirement is the manifest-level signal that a module may mint workload
// tokens. A struct rather than a bare boolean so it can grow without another manifest field.
type WorkloadIdentityRequirement struct {
	// Mint requests a minting credential for this module.
	Mint bool `json:"mint"`
}

// CredentialProviderSpec is the manifest-level signal that a module provides one or more
// native-protocol credential kinds through the broker (ADR 0080).
type CredentialProviderSpec struct {
	// Kinds lists the credential kind(s) this module provides (e.g. "s3", "postgres"). At most
	// one module may declare a given kind fleet-wide — core refuses to route a kind with more
	// than one registered provider rather than guessing.
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^[a-z][a-z0-9-]*$`
	Kinds []string `json:"kinds"`
}

// DatabaseRequirement is the manifest-level signal that a module needs a database. It's a
// struct rather than a bare boolean so it can grow without another manifest field.
type DatabaseRequirement struct {
	// Enabled requests a database for this module.
	Enabled bool `json:"enabled"`
}

// EventBusAccess lists the event types a module may use, as dotted patterns matching
// docs/decisions/0002's event-type grammar (e.g. "dashboard.created", "dashboard.*").
// Each is applied across all workspaces (booth.*.<pattern>): modules serve every
// workspace, so NATS permissions can't usefully be narrower than that.
type EventBusAccess struct {
	// Publish lists event types this module may publish.
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^[a-z][a-z0-9]*(\.([a-z][a-z0-9]*|\*))+$`
	Publish []string `json:"publish,omitempty"`

	// Subscribe lists event types this module consumes. Grants access to the
	// JetStream consumer API for the shared events stream.
	// +optional
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^[a-z][a-z0-9]*(\.([a-z][a-z0-9]*|\*))+$`
	Subscribe []string `json:"subscribe,omitempty"`
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
