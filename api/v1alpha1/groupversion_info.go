// Package v1alpha1 contains the BoothModule API types. This is the Kubernetes-native
// transport for contracts/module-manifest.md — a module's Helm chart templates one
// BoothModule resource, and booth-core's registry controller watches this GroupVersion
// (ADR 0019). The manifest fields here must stay in lockstep with that contract; a field
// added there needs a matching field here, and vice versa.
// +kubebuilder:object:generate=true
// +groupName=booth.projectbooth.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "booth.projectbooth.io", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
