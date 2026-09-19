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
