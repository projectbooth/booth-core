package auth

import (
	"errors"
	"reflect"
	"testing"
)

func TestDeriveMemberships(t *testing.T) {
	groups := []string{
		"/workspaces/acme-analytics/owner",
		"/workspaces/acme-marketing/viewer",
		"/some/unrelated/idp/group",
		"/workspaces/bad-role/superadmin",
		"/workspaces/UPPERCASE/owner",
	}

	got := DeriveMemberships(groups)
	want := []Membership{
		{Workspace: "acme-analytics", Role: RoleOwner},
		{Workspace: "acme-marketing", Role: RoleViewer},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DeriveMemberships() = %#v, want %#v", got, want)
	}
}

// ADR 0124 point 1 ("project carries no authorization meaning") is confirmed by this test, not
// just asserted in a doc comment: a groups entry shaped like a project grant must produce no
// membership at all, the same way any other unrelated IdP group is silently ignored — there is
// no /projects/<slug>/<role> grammar for DeriveMemberships to match, on purpose, so a future
// mistake (an IdP, or a module, emitting such a claim) can't accidentally become a role grant.
func TestDeriveMemberships_IgnoresAProjectShapedGroupEntry(t *testing.T) {
	groups := []string{
		"/workspaces/acme-analytics/owner",
		"/projects/finance/owner",
		"/projects/finance-dev/editor",
	}

	got := DeriveMemberships(groups)
	want := []Membership{{Workspace: "acme-analytics", Role: RoleOwner}}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DeriveMemberships() = %#v, want %#v (project-shaped entries must grant nothing)", got, want)
	}
}

// ADR 0094: /platform/operator is a workspace-independent, literal-string claim, checked as an
// exact match — not matched by DeriveMemberships' /workspaces/<slug>/<role> grammar, and not
// derivable from any role.
func TestIsOperator(t *testing.T) {
	for _, tc := range []struct {
		name   string
		groups []string
		want   bool
	}{
		{"present", []string{"/workspaces/acme/owner", "/platform/operator"}, true},
		{"absent", []string{"/workspaces/acme/owner"}, false},
		{"empty groups", nil, false},
		{"similar but not exact", []string{"/platform/operator/extra", "platform/operator"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsOperator(tc.groups); got != tc.want {
				t.Errorf("IsOperator(%v) = %v, want %v", tc.groups, got, tc.want)
			}
		})
	}
}

func TestRoleFor(t *testing.T) {
	memberships := []Membership{
		{Workspace: "acme-analytics", Role: RoleOwner},
		{Workspace: "acme-marketing", Role: RoleViewer},
	}

	if role, ok := RoleFor(memberships, "acme-analytics"); !ok || role != RoleOwner {
		t.Fatalf("RoleFor(acme-analytics) = %v, %v; want owner, true", role, ok)
	}

	if _, ok := RoleFor(memberships, "nonexistent"); ok {
		t.Fatalf("RoleFor(nonexistent) returned ok=true, want false")
	}
}

func TestResolveActiveWorkspace(t *testing.T) {
	memberships := []Membership{
		{Workspace: "acme-analytics", Role: RoleOwner},
	}

	active, err := ResolveActiveWorkspace(memberships, "acme-analytics")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if active.Role != RoleOwner {
		t.Fatalf("active.Role = %v, want owner", active.Role)
	}

	_, err = ResolveActiveWorkspace(memberships, "not-a-member-here")
	if !errors.Is(err, ErrNoMembership) {
		t.Fatalf("expected ErrNoMembership, got %v", err)
	}
}

func TestRoleIsAdmin(t *testing.T) {
	cases := []struct {
		role Role
		want bool
	}{
		{RoleOwner, true},
		{RoleEditor, false},
		{RoleViewer, false},
	}
	for _, c := range cases {
		if got := c.role.IsAdmin(); got != c.want {
			t.Errorf("%s.IsAdmin() = %v, want %v", c.role, got, c.want)
		}
	}
}

// LesserRole is what caps a workload token's role at its owner's (ADR 0056). An unknown role on
// either side must yield no role at all, never a default.
func TestLesserRole(t *testing.T) {
	roles := []Role{RoleViewer, RoleEditor, RoleOwner}
	for i, a := range roles {
		for j, b := range roles {
			want := roles[min(i, j)]
			if got := LesserRole(a, b); got != want {
				t.Errorf("LesserRole(%s, %s) = %q, want %q", a, b, got, want)
			}
		}
		for _, bad := range []Role{"", "admin", "OWNER"} {
			if got := LesserRole(a, bad); got != "" {
				t.Errorf("LesserRole(%s, %q) = %q, want no role", a, bad, got)
			}
			if got := LesserRole(bad, a); got != "" {
				t.Errorf("LesserRole(%q, %s) = %q, want no role", bad, a, got)
			}
		}
	}
}
