package directory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/projectbooth/booth-core/internal/auth"
)

// countingStore wraps a Store to count writes and optionally fail them.
type countingStore struct {
	Store
	upserts int
	err     error
}

func (c *countingStore) Upsert(ctx context.Context, u User) error {
	c.upserts++
	if c.err != nil {
		return c.err
	}
	return c.Store.Upsert(ctx, u)
}

func claimsFor(sub, username, email string) *auth.Claims {
	return &auth.Claims{Subject: sub, PreferredUsername: username, Name: "Some Name", Email: email}
}

var acmeOwner = []auth.Membership{{Workspace: "acme", Role: auth.RoleOwner}}

func newTestRecorder(store Store) (*Recorder, *time.Time) {
	r := NewRecorder(store)
	now := time.Now()
	r.Now = func() time.Time { return now }
	return r, &now
}

func TestRecorder_RecordsIdentityFromVerifiedToken(t *testing.T) {
	mem := NewMemoryStore()
	r, _ := newTestRecorder(mem)

	r.Observe(context.Background(), claimsFor("u1", "alice", "a@example.com"),
		[]auth.Membership{{Workspace: "acme", Role: auth.RoleOwner}, {Workspace: "labs", Role: auth.RoleViewer}})

	got, ok, _ := mem.Get(context.Background(), "u1", "labs")
	if !ok || got.PreferredUsername != "alice" || got.Email != "a@example.com" {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}

// Every authenticated request passes through Observe; unchanged identities must not turn
// into a database write per request.
func TestRecorder_DebouncesUnchangedIdentity(t *testing.T) {
	cs := &countingStore{Store: NewMemoryStore()}
	r, now := newTestRecorder(cs)
	c := claimsFor("u1", "alice", "a@example.com")

	for i := 0; i < 50; i++ {
		r.Observe(context.Background(), c, acmeOwner)
	}
	if cs.upserts != 1 {
		t.Fatalf("upserts = %d after 50 identical requests, want 1", cs.upserts)
	}

	*now = now.Add(r.Interval + time.Second)
	r.Observe(context.Background(), c, acmeOwner)
	if cs.upserts != 2 {
		t.Fatalf("upserts = %d after the interval elapsed, want 2 (refreshes last-seen)", cs.upserts)
	}
}

func TestRecorder_WritesImmediatelyWhenClaimsOrMembershipsChange(t *testing.T) {
	cs := &countingStore{Store: NewMemoryStore()}
	r, _ := newTestRecorder(cs)

	r.Observe(context.Background(), claimsFor("u1", "alice", "a@example.com"), acmeOwner)
	r.Observe(context.Background(), claimsFor("u1", "alice-renamed", "a@example.com"), acmeOwner)
	if cs.upserts != 2 {
		t.Fatalf("upserts = %d after a claim change, want 2", cs.upserts)
	}
	r.Observe(context.Background(), claimsFor("u1", "alice-renamed", "a@example.com"),
		append(acmeOwner, auth.Membership{Workspace: "labs", Role: auth.RoleViewer}))
	if cs.upserts != 3 {
		t.Fatalf("upserts = %d after a membership change, want 3 (visibility depends on it)", cs.upserts)
	}
}

func TestRecorder_MembershipOrderDoesNotDefeatDebounce(t *testing.T) {
	cs := &countingStore{Store: NewMemoryStore()}
	r, _ := newTestRecorder(cs)
	c := claimsFor("u1", "alice", "a@example.com")
	a := auth.Membership{Workspace: "acme", Role: auth.RoleOwner}
	b := auth.Membership{Workspace: "labs", Role: auth.RoleViewer}

	r.Observe(context.Background(), c, []auth.Membership{a, b})
	r.Observe(context.Background(), c, []auth.Membership{b, a})
	if cs.upserts != 1 {
		t.Fatalf("upserts = %d, want 1: token group ordering isn't a real change", cs.upserts)
	}
}

// A directory outage must cost a log line, never a login — and must be retried.
func TestRecorder_StoreFailureIsSwallowedAndRetried(t *testing.T) {
	cs := &countingStore{Store: NewMemoryStore(), err: errors.New("database down")}
	r, _ := newTestRecorder(cs)
	c := claimsFor("u1", "alice", "a@example.com")

	r.Observe(context.Background(), c, acmeOwner) // must not panic or return anything
	cs.err = nil
	r.Observe(context.Background(), c, acmeOwner)

	if cs.upserts != 2 {
		t.Fatalf("upserts = %d, want 2: a failed write must not be remembered as done", cs.upserts)
	}
	if _, ok, _ := cs.Store.Get(context.Background(), "u1", "acme"); !ok {
		t.Fatal("identity was never recorded after the store recovered")
	}
}

func TestRecorder_IgnoresTokensWithoutASubject(t *testing.T) {
	cs := &countingStore{Store: NewMemoryStore()}
	r, _ := newTestRecorder(cs)
	r.Observe(context.Background(), &auth.Claims{}, acmeOwner)
	r.Observe(context.Background(), nil, acmeOwner)
	if cs.upserts != 0 {
		t.Fatalf("upserts = %d, want 0", cs.upserts)
	}
}

// Core starts on memory and switches to Postgres later. The new store starts empty, so after
// Reset a user is re-recorded on their very next request rather than after the debounce.
func TestSwitchable_SwapAndResetRepopulateImmediately(t *testing.T) {
	sw := NewSwitchable(NewMemoryStore())
	r, _ := newTestRecorder(sw)
	c := claimsFor("u1", "alice", "a@example.com")

	r.Observe(context.Background(), c, acmeOwner)
	if _, ok, _ := sw.Get(context.Background(), "u1", "acme"); !ok {
		t.Fatal("not recorded in the initial store")
	}

	second := NewMemoryStore()
	sw.Swap(second)
	if _, ok, _ := sw.Get(context.Background(), "u1", "acme"); ok {
		t.Fatal("swap should present the new (empty) store")
	}

	// Without Reset the debounce would suppress this write.
	r.Observe(context.Background(), c, acmeOwner)
	if _, ok, _ := second.Get(context.Background(), "u1", "acme"); ok {
		t.Fatal("expected the debounce to suppress the write until Reset (documenting why Reset exists)")
	}

	r.Reset()
	r.Observe(context.Background(), c, acmeOwner)
	if _, ok, _ := second.Get(context.Background(), "u1", "acme"); !ok {
		t.Fatal("user not re-recorded into the swapped-in store after Reset")
	}
}

// ADR 0056 caps a run's role at its owner's, read from the directory. So the directory has to
// learn of a demotion at once — not after the debounce interval, during which a scheduled job
// could still mint at the old, higher role.
func TestRecorder_RecordsRolesAndAPromptDemotion(t *testing.T) {
	mem := NewMemoryStore()
	r, _ := newTestRecorder(mem) // the clock never advances: only a *change* can cause a write
	c := claimsFor("u1", "alice", "a@example.com")

	r.Observe(context.Background(), c, []auth.Membership{{Workspace: "acme", Role: auth.RoleOwner}})
	got, _, _ := mem.Get(context.Background(), "u1", "acme")
	if got.Roles["acme"] != "owner" {
		t.Fatalf("role = %q, want owner", got.Roles["acme"])
	}

	r.Observe(context.Background(), c, []auth.Membership{{Workspace: "acme", Role: auth.RoleViewer}})
	got, _, _ = mem.Get(context.Background(), "u1", "acme")
	if got.Roles["acme"] != "viewer" {
		t.Errorf("role = %q right after a demotion, want viewer (debounce must not hide a role change)", got.Roles["acme"])
	}
}

// When one token lists two roles for a workspace, ADR 0025 doesn't say which wins. The recorded
// role caps what a run may do, so it must be the lesser one.
func TestRecorder_AmbiguousRolesRecordTheLeastPrivileged(t *testing.T) {
	for _, order := range [][]auth.Role{
		{auth.RoleOwner, auth.RoleViewer},
		{auth.RoleViewer, auth.RoleOwner},
	} {
		mem := NewMemoryStore()
		r, _ := newTestRecorder(mem)
		var ms []auth.Membership
		for _, role := range order {
			ms = append(ms, auth.Membership{Workspace: "acme", Role: role})
		}
		r.Observe(context.Background(), claimsFor("u1", "alice", ""), ms)
		got, _, _ := mem.Get(context.Background(), "u1", "acme")
		if got.Roles["acme"] != "viewer" {
			t.Errorf("order %v: role = %q, want viewer", order, got.Roles["acme"])
		}
	}
}
