package auth

import (
	"fmt"
	"regexp"
)

// Role is a workspace membership role, per ADR 0008 and
// docs/decisions/0001-workspace-role-claim-shape.md. Exactly three values in v0 — no
// finer-grained roles.
type Role string

const (
	RoleOwner  Role = "owner"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

// RoleRank orders roles by privilege (higher is more privileged); an unknown role ranks 0, below
// every real one. It exists so "the lesser of two roles" (ADR 0056) is written once.
func RoleRank(r Role) int {
	switch r {
	case RoleOwner:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

// LesserRole returns whichever of a and b is less privileged. If either is not a real role the
// result is "" — callers must treat that as no access, never as a default.
func LesserRole(a, b Role) Role {
	if RoleRank(a) == 0 || RoleRank(b) == 0 {
		return ""
	}
	if RoleRank(a) <= RoleRank(b) {
		return a
	}
	return b
}

// IsAdmin reports whether this role is the one ADR 0023's adminNavPath gating checks
// for.
func (r Role) IsAdmin() bool {
	return r == RoleOwner
}

// Membership is one workspace/role pair derived from a token's groups claim.
type Membership struct {
	Workspace string `json:"workspace"`
	Role      Role   `json:"role"`
}

var groupEntryPattern = regexp.MustCompile(`^/workspaces/([a-z0-9-]+)/(owner|editor|viewer)$`)

// DeriveMemberships parses a token's groups claim into workspace memberships per
// docs/decisions/0001-workspace-role-claim-shape.md's grammar
// (`/workspaces/<slug>/<role>`). Non-matching entries are ignored rather than rejected —
// a user may belong to unrelated IdP groups.
func DeriveMemberships(groups []string) []Membership {
	memberships := make([]Membership, 0, len(groups))
	for _, g := range groups {
		m := groupEntryPattern.FindStringSubmatch(g)
		if m == nil {
			continue
		}
		memberships = append(memberships, Membership{
			Workspace: m[1],
			Role:      Role(m[2]),
		})
	}
	return memberships
}

// RoleFor returns the caller's role in the given workspace, if any.
func RoleFor(memberships []Membership, workspace string) (Role, bool) {
	for _, m := range memberships {
		if m.Workspace == workspace {
			return m.Role, true
		}
	}
	return "", false
}

// ErrNoMembership is returned when a caller has no role in the requested workspace.
var ErrNoMembership = fmt.Errorf("caller has no membership in the requested workspace")

// ResolveActiveWorkspace validates that the caller holds a role in the requested
// workspace slug, per decision 0001's "read from the JWT, not a side table" rule. It is
// the authoritative check the auth middleware runs before forwarding
// X-Booth-Workspace/X-Booth-Role to a module.
func ResolveActiveWorkspace(memberships []Membership, requested string) (Membership, error) {
	role, ok := RoleFor(memberships, requested)
	if !ok {
		return Membership{}, ErrNoMembership
	}
	return Membership{Workspace: requested, Role: role}, nil
}
