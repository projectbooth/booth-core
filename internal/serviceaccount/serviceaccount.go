// Package serviceaccount implements ADR 0120's service accounts: core-stored, workspace-scoped,
// non-human identities that may be named as the `owner` of a workload-token mint (ADR 0056,
// internal/workload), so an unattended run's access doesn't lapse just because no person has
// signed in recently (ADR 0058's ownerMaxAge, which applies to person owners only).
//
// Unlike internal/directory, whose entries are an opportunistic display cache that's fine to lose
// and let repopulate, a service account is an explicit, owner-authored role grant: silently losing
// one, or having one reappear un-disabled after a store swap, is a real security-relevant bug, not
// a degraded-mode inconvenience. So this package deliberately has no in-memory fallback — see
// Unavailable in switchable.go. A deployment either has a real, persistent store, or every call
// refuses with ErrUnavailable until it does.
package serviceaccount

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/projectbooth/booth-core/internal/auth"
)

var (
	// ErrNotFound means no account exists by that workspace/name.
	ErrNotFound = errors.New("service account not found")

	// ErrAlreadyExists means a create was attempted for a workspace/name pair that already has an
	// account. Names are not reused even after a delete, so a stale caller can't silently start
	// acting through what it believes is the account it created earlier.
	ErrAlreadyExists = errors.New("a service account with that name already exists in this workspace")

	// ErrMustDisableBeforeDelete is the coordinator's v0 ruling in place of a usage registry
	// (ADR 0120 addendum, point 5): there is no tracking of which pipelines name an account, so
	// deletion is gated on an explicit, visible precondition instead — the account must already be
	// disabled. Disabling alone already fails every future mint closed, so by the time a delete is
	// possible no run can be depending on the account still working.
	ErrMustDisableBeforeDelete = errors.New("a service account must be disabled before it can be deleted")

	// ErrUnavailable means core has no real, persistent store to serve this call from right now
	// (no database configured, or core's own database is still provisioning). See Unavailable.
	ErrUnavailable = errors.New("service accounts are temporarily unavailable")

	// ErrInvalidAccount wraps a validation failure in Create/Update (HTTP 400).
	ErrInvalidAccount = errors.New("invalid service account")
)

// namePattern is the account-name grammar: the same slug shape core already uses for a workspace
// slug (internal/workload.workspacePattern), so an account name reads and validates the same way.
var namePattern = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// Account is one service account.
type Account struct {
	Workspace   string
	Name        string
	Role        auth.Role // RoleViewer or RoleEditor only — never RoleOwner (ADR 0120 #1, #6)
	Description string
	CreatedBy   string // the creating owner's sub
	CreatedAt   time.Time
	Disabled    bool
}

// ID is the account's owner-field identity, `svc:<workspace>:<name>` (ADR 0120 #1), the exact
// shape internal/workload.Service.ownerRole looks for.
func (a Account) ID() string { return "svc:" + a.Workspace + ":" + a.Name }

// validRole reports whether r is one of the two roles a service account may hold in v0.
func validRole(r auth.Role) bool { return r == auth.RoleViewer || r == auth.RoleEditor }

// ValidateNew checks a new account's caller-supplied fields before Create. Workspace, CreatedBy
// and CreatedAt are the store's job to fill in, not the caller's.
func ValidateNew(name string, role auth.Role) error {
	switch {
	case !namePattern.MatchString(name):
		return errors.New("name must match " + namePattern.String())
	case !validRole(role):
		return errors.New(`role must be "viewer" or "editor"`)
	}
	return nil
}

// Patch describes a partial update (PATCH /api/service-accounts/{name}): a nil field is left
// unchanged. Disabled is a *bool (not a bool) for the same reason — "omitted" and "set to false"
// must be distinguishable.
type Patch struct {
	Role        *auth.Role
	Description *string
	Disabled    *bool
}

// Validate checks the fields of p that are actually set.
func (p Patch) Validate() error {
	if p.Role != nil && !validRole(*p.Role) {
		return errors.New(`role must be "viewer" or "editor"`)
	}
	return nil
}

// Store persists service accounts. Every method may return ErrUnavailable (see Unavailable); no
// implementation here degrades to an in-memory fallback — see the package doc comment.
type Store interface {
	// Create adds a new account. a.Workspace, a.Name, a.Role must already be valid
	// (ValidateNew); CreatedBy and CreatedAt are the caller's to set from the request's
	// identity and clock. Returns ErrAlreadyExists if the (workspace, name) pair is taken,
	// including by a deleted account's former name.
	Create(ctx context.Context, a Account) error

	// Get returns the account named name in workspace, or (_, false, nil) if there isn't one.
	Get(ctx context.Context, workspace, name string) (Account, bool, error)

	// List returns every account in workspace, ordered by name.
	List(ctx context.Context, workspace string) ([]Account, error)

	// Update applies patch to the named account and returns the result. Returns ErrNotFound if it
	// doesn't exist. patch.Validate() is the caller's responsibility before calling this.
	Update(ctx context.Context, workspace, name string, patch Patch) (Account, error)

	// Delete removes the named account. Returns ErrNotFound if it doesn't exist, or
	// ErrMustDisableBeforeDelete if it exists but Disabled is false.
	Delete(ctx context.Context, workspace, name string) error
}
