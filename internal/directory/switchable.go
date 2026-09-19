package directory

import (
	"context"
	"sync/atomic"
)

// Switchable is a Store whose backing store can be replaced while the process runs. Core
// starts serving on an in-memory store and switches to PostgreSQL once its own database has
// been provisioned (ADR 0053), which can take a while on a fresh install (the bundled server
// has to start first) and shouldn't hold up the HTTP server.
//
// Swapping discards whatever the previous store held. That's deliberate and safe for the
// in-memory fallback: entries are opportunistic and repopulate on each user's next request.
type Switchable struct {
	cur atomic.Pointer[storeBox]
}

type storeBox struct{ Store }

// NewSwitchable starts with initial as the backing store.
func NewSwitchable(initial Store) *Switchable {
	s := &Switchable{}
	s.cur.Store(&storeBox{initial})
	return s
}

// Swap makes next the backing store for all subsequent calls.
func (s *Switchable) Swap(next Store) { s.cur.Store(&storeBox{next}) }

func (s *Switchable) Upsert(ctx context.Context, u User) error { return s.cur.Load().Upsert(ctx, u) }

func (s *Switchable) Get(ctx context.Context, sub, workspace string) (User, bool, error) {
	return s.cur.Load().Get(ctx, sub, workspace)
}

func (s *Switchable) Search(ctx context.Context, workspace, q string, limit int) ([]User, error) {
	return s.cur.Load().Search(ctx, workspace, q, limit)
}
