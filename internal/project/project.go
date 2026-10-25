// Package project implements ADR 0124's projects: a core-stored, workspace-scoped navigation
// label applied to resources across modules, plus bindings that map an external name (a
// Postgres schema, an Iceberg namespace, a Storage prefix) to a project so existing data groups
// without hand-tagging. A project grants nothing, denies nothing, and isolates nothing — it must
// never be read by any authorization decision (ADR 0124 point 1; see internal/auth and
// internal/natsauth, neither of which reads it, confirmed by test).
//
// Like internal/serviceaccount (ADR 0120) and unlike internal/directory, this package has no
// in-memory fallback. A project is an explicit, owner-authored record — losing one or inventing
// one across a store swap is a real bug, not a degraded-mode inconvenience. See Unavailable in
// switchable.go.
package project

import (
	"context"
	"errors"
	"regexp"
	"time"
)

var (
	// ErrNotFound means no project exists by that workspace/slug.
	ErrNotFound = errors.New("project not found")

	// ErrAlreadyExists means a create was attempted for a workspace/slug pair that already has a
	// project. The slug is immutable once created, so this is the only way a duplicate can arise.
	ErrAlreadyExists = errors.New("a project with that slug already exists in this workspace")

	// ErrUnavailable means core has no real, persistent store to serve this call from right now.
	// See Unavailable.
	ErrUnavailable = errors.New("projects are temporarily unavailable")

	// ErrInvalidProject wraps a validation failure in Create/Update (HTTP 400).
	ErrInvalidProject = errors.New("invalid project")

	// ErrParentNotFound means the referenced parent slug does not exist in the same workspace (or
	// is archived — an archived project is not a valid new parent).
	ErrParentNotFound = errors.New("parent project not found")

	// ErrParentHasAParent is ADR 0124 point 5's two-level limit: a project whose own parent is
	// non-null can never itself be named as someone else's parent.
	ErrParentHasAParent = errors.New("the parent project must itself be top-level (no grandparents in v0)")

	// ErrHasChildren refuses giving a project a parent while it already has children of its own —
	// the symmetric half of ErrParentHasAParent; both together keep the hierarchy at exactly two
	// levels.
	ErrHasChildren = errors.New("a project with children cannot itself be given a parent")

	// ErrBindingNotFound means no binding exists by that id in the workspace.
	ErrBindingNotFound = errors.New("binding not found")

	// ErrBindingConflict means a binding already exists for that (workspace, kind, backendId,
	// value) — one external name binds to at most one project.
	ErrBindingConflict = errors.New("a binding already exists for that external name")

	// ErrInvalidBinding wraps a validation failure in CreateBinding (HTTP 400).
	ErrInvalidBinding = errors.New("invalid binding")
)

// SlugPattern is the project-slug grammar: the same shape core already uses for a workspace slug
// and a service-account name (internal/serviceaccount's namePattern) — lowercase, hyphenated,
// short, immutable once created. Exported so internal/api can validate the optional
// ?project=<slug> passthrough on GET /api/modules/{id}/iframe-url against the same rule without
// this package's other, heavier validation.
var SlugPattern = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// Project is a navigation label (ADR 0124). Parent, when non-empty, must name another project in
// the same workspace whose own Parent is empty — nesting is limited to two levels in v0.
type Project struct {
	Workspace   string
	Slug        string
	Name        string
	Parent      string // "" = top-level
	Description string
	Color       string
	Icon        string
	Archived    bool
	CreatedBy   string
	CreatedAt   time.Time
}

const (
	maxNameLength        = 200
	maxDescriptionLength = 4000
	maxColorIconLength   = 64 // opaque to core; a generous cap against abuse, not a format check
)

// ValidateNew checks a new project's caller-supplied fields before Create. Workspace, CreatedBy
// and CreatedAt are the store's job to fill in, not the caller's. Parent existence/depth is
// checked by the Store (it needs to look the parent up), not here.
func ValidateNew(slug, name string) error {
	switch {
	case !SlugPattern.MatchString(slug):
		return errors.New("slug must match " + SlugPattern.String())
	case name == "" || len(name) > maxNameLength:
		return errors.New("name must be 1-200 characters")
	}
	return nil
}

// Patch describes a partial update (PATCH /api/projects/{slug}): a nil field is left unchanged.
// Parent is a *string (not a plain string) so "leave the parent alone" and "clear it back to
// top-level" are distinguishable — a non-nil Parent pointing at "" means the latter; Slug is
// deliberately absent, it is immutable.
type Patch struct {
	Name        *string
	Parent      *string
	Description *string
	Color       *string
	Icon        *string
	Archived    *bool
}

// Validate checks the fields of p that are actually set. Parent existence/depth and the
// has-children check are the Store's job (they need to look other rows up).
func (p Patch) Validate() error {
	if p.Name != nil && (*p.Name == "" || len(*p.Name) > maxNameLength) {
		return errors.New("name must be 1-200 characters")
	}
	if p.Description != nil && len(*p.Description) > maxDescriptionLength {
		return errors.New("description must be at most 4000 characters")
	}
	if p.Color != nil && len(*p.Color) > maxColorIconLength {
		return errors.New("color must be at most 64 characters")
	}
	if p.Icon != nil && len(*p.Icon) > maxColorIconLength {
		return errors.New("icon must be at most 64 characters")
	}
	return nil
}

// BindingKind is one of the three external-name kinds a project may bind (ADR 0124 point 4).
type BindingKind string

const (
	BindingPostgresSchema   BindingKind = "postgres_schema"
	BindingIcebergNamespace BindingKind = "iceberg_namespace"
	BindingStoragePrefix    BindingKind = "storage_prefix"
)

func validBindingKind(k BindingKind) bool {
	switch k {
	case BindingPostgresSchema, BindingIcebergNamespace, BindingStoragePrefix:
		return true
	}
	return false
}

// Binding maps an external name to a project, so a module that groups by that name (a Postgres
// schema, an Iceberg namespace, a Storage prefix) can add the project heading without every
// resource carrying the field itself. Resolution — matching a module's own resource names
// against the bindings it fetches — is the owning module's job, never core's: core has no
// visibility into any module's resource list.
//
// BackendID is required for BindingStoragePrefix (a prefix is only meaningful within one named
// Storage backend) and must be empty for the other two kinds, where there is only ever one
// namespace of schema/namespace names to collide within.
type Binding struct {
	ID          string
	Workspace   string
	ProjectSlug string
	Kind        BindingKind
	BackendID   string
	Value       string
	CreatedBy   string
	CreatedAt   time.Time
}

const maxBindingValueLength = 500

// ValidateNewBinding checks a new binding's caller-supplied fields before CreateBinding.
func ValidateNewBinding(kind BindingKind, backendID, value string) error {
	switch {
	case !validBindingKind(kind):
		return errors.New(`kind must be "postgres_schema", "iceberg_namespace" or "storage_prefix"`)
	case value == "" || len(value) > maxBindingValueLength:
		return errors.New("value must be 1-500 characters")
	case kind == BindingStoragePrefix && backendID == "":
		return errors.New("backendId is required for a storage_prefix binding")
	case kind != BindingStoragePrefix && backendID != "":
		return errors.New("backendId must be empty for a " + string(kind) + " binding")
	}
	return nil
}

// BindingFilter narrows ListBindings. Both fields are optional (zero value = unfiltered); at
// least one is expected in practice (an unfiltered, unbounded workspace-wide list is legal but
// unusual), matching the two routes this serves: GET /api/projects/{slug}/bindings (ProjectSlug
// set) and GET /api/projects/bindings?kind=... (Kind set, ProjectSlug empty).
type BindingFilter struct {
	ProjectSlug string
	Kind        BindingKind
}

// Store persists projects and their bindings. Every method may return ErrUnavailable (see
// Unavailable); no implementation here degrades to an in-memory fallback — see the package doc
// comment.
type Store interface {
	// Create adds a new project. p.Workspace, p.Slug, p.Name must already be valid (ValidateNew).
	// If p.Parent is non-empty, it must reference an existing, non-archived, top-level project in
	// the same workspace (ErrParentNotFound / ErrParentHasAParent).
	Create(ctx context.Context, p Project) error

	// Get returns the project named slug in workspace, or (_, false, nil) if there isn't one.
	Get(ctx context.Context, workspace, slug string) (Project, bool, error)

	// List returns every project in workspace (including archived ones — the caller decides how
	// to render that), ordered by slug.
	List(ctx context.Context, workspace string) ([]Project, error)

	// Update applies patch to the named project and returns the result. Returns ErrNotFound if it
	// doesn't exist. patch.Validate() is the caller's responsibility before calling this; Update
	// itself re-validates a changed Parent's existence/depth (ErrParentNotFound /
	// ErrParentHasAParent / ErrHasChildren).
	Update(ctx context.Context, workspace, slug string, patch Patch) (Project, error)

	// CreateBinding adds a new binding. b.Workspace, b.ProjectSlug, b.Kind, b.BackendID, b.Value
	// must already be valid (ValidateNewBinding) and b.ProjectSlug must reference an existing
	// project. Returns the binding with its generated ID. Returns ErrBindingConflict if
	// (workspace, kind, backendId, value) is already bound, ErrNotFound if ProjectSlug doesn't
	// exist.
	CreateBinding(ctx context.Context, b Binding) (Binding, error)

	// ListBindings returns every binding in workspace matching filter, ordered by creation time.
	ListBindings(ctx context.Context, workspace string, filter BindingFilter) ([]Binding, error)

	// DeleteBinding removes the binding named id in workspace. Returns ErrBindingNotFound if it
	// doesn't exist.
	DeleteBinding(ctx context.Context, workspace, id string) error
}
