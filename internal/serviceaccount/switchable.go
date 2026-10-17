package serviceaccount

import (
	"context"
	"sync/atomic"
)

// Unavailable is a Store that refuses every call with ErrUnavailable. It's what core serves
// service-account routes and `svc:` mints from before a real, persistent store exists — while no
// database is configured at all, or while core's own database is still provisioning — instead of
// the opportunistic in-memory fallback internal/directory.Switchable uses for its own, lossy-by-
// design entries. See the package doc comment: silently losing or re-inventing a service account
// is a correctness bug for this resource, not a degraded mode.
type Unavailable struct{}

func (Unavailable) Create(context.Context, Account) error { return ErrUnavailable }
func (Unavailable) Get(context.Context, string, string) (Account, bool, error) {
	return Account{}, false, ErrUnavailable
}
func (Unavailable) List(context.Context, string) ([]Account, error) { return nil, ErrUnavailable }
func (Unavailable) Update(context.Context, string, string, Patch) (Account, error) {
	return Account{}, ErrUnavailable
}
func (Unavailable) Delete(context.Context, string, string) error { return ErrUnavailable }

// Switchable is a Store whose backing store can be replaced while the process runs — core starts
// serving Unavailable{} and swaps in a real PostgresStore once core's own database has been
// provisioned (mirroring internal/directory.Switchable's lifecycle, but never swapping to or from
// an in-memory store: see the package doc comment). Unlike the directory's version, this is meant
// to be swapped at most once in a process's life — from Unavailable to a real store — not swapped
// back and forth.
type Switchable struct {
	cur atomic.Pointer[storeBox]
}

type storeBox struct{ Store }

// NewSwitchable starts with initial as the backing store (typically Unavailable{}).
func NewSwitchable(initial Store) *Switchable {
	s := &Switchable{}
	s.cur.Store(&storeBox{initial})
	return s
}

// Swap makes next the backing store for all subsequent calls.
func (s *Switchable) Swap(next Store) { s.cur.Store(&storeBox{next}) }

func (s *Switchable) Create(ctx context.Context, a Account) error {
	return s.cur.Load().Create(ctx, a)
}

func (s *Switchable) Get(ctx context.Context, workspace, name string) (Account, bool, error) {
	return s.cur.Load().Get(ctx, workspace, name)
}

func (s *Switchable) List(ctx context.Context, workspace string) ([]Account, error) {
	return s.cur.Load().List(ctx, workspace)
}

func (s *Switchable) Update(ctx context.Context, workspace, name string, patch Patch) (Account, error) {
	return s.cur.Load().Update(ctx, workspace, name, patch)
}

func (s *Switchable) Delete(ctx context.Context, workspace, name string) error {
	return s.cur.Load().Delete(ctx, workspace, name)
}
