package v1alpha1

// ServiceNamespace returns the namespace of the Service backing this module: the declared
// serviceRef.namespace, or — when that is unset — the namespace of the BoothModule resource
// itself, as contracts/module-manifest.md and the field's own documentation promise.
//
// It's a method (rather than each caller repeating the fallback) because everything that
// needs to *reach* a module — the health check, the gateway, the database and event-bus
// credential Secrets — must agree on it. Each of those once carried its own copy of this
// rule, and two of them forgot it, which left every module without an explicit namespace
// "Unreachable" at an address like `svc..svc.cluster.local`.
//
// resourceNamespace is the BoothModule's own metadata.namespace. A CRD schema can't default a
// field from the object's own metadata, so the default has to be applied here, in code.
func (s BoothModuleSpec) ServiceNamespace(resourceNamespace string) string {
	if s.ServiceRef.Namespace != "" {
		return s.ServiceRef.Namespace
	}
	return resourceNamespace
}
