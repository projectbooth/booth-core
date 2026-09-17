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
