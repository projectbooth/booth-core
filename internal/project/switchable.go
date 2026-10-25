package project

import (
	"context"
	"sync/atomic"
)

// Unavailable is a Store that refuses every call with ErrUnavailable. It's what core serves the
// project routes from before a real, persistent store exists — while no database is configured
// at all, or while core's own database is still provisioning — instead of an opportunistic
// in-memory fallback. See the package doc comment: silently losing or re-inventing a project is
// a correctness bug for this resource, not a degraded mode.
type Unavailable struct{}

func (Unavailable) Create(context.Context, Project) error { return ErrUnavailable }
func (Unavailable) Get(context.Context, string, string) (Project, bool, error) {
	return Project{}, false, ErrUnavailable
}
func (Unavailable) List(context.Context, string) ([]Project, error) { return nil, ErrUnavailable }
func (Unavailable) Update(context.Context, string, string, Patch) (Project, error) {
	return Project{}, ErrUnavailable
}
func (Unavailable) CreateBinding(context.Context, Binding) (Binding, error) {
	return Binding{}, ErrUnavailable
}
func (Unavailable) ListBindings(context.Context, string, BindingFilter) ([]Binding, error) {
	return nil, ErrUnavailable
}
func (Unavailable) DeleteBinding(context.Context, string, string) error { return ErrUnavailable }

// Switchable is a Store whose backing store can be replaced while the process runs — core starts
// serving Unavailable{} and swaps in a real PostgresStore once core's own database has been
// provisioned (mirroring internal/serviceaccount.Switchable's lifecycle). Meant to be swapped at
// most once in a process's life — from Unavailable to a real store — not swapped back and forth.
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

func (s *Switchable) Create(ctx context.Context, p Project) error {
	return s.cur.Load().Create(ctx, p)
}

func (s *Switchable) Get(ctx context.Context, workspace, slug string) (Project, bool, error) {
	return s.cur.Load().Get(ctx, workspace, slug)
}

func (s *Switchable) List(ctx context.Context, workspace string) ([]Project, error) {
	return s.cur.Load().List(ctx, workspace)
}

func (s *Switchable) Update(ctx context.Context, workspace, slug string, patch Patch) (Project, error) {
	return s.cur.Load().Update(ctx, workspace, slug, patch)
}

func (s *Switchable) CreateBinding(ctx context.Context, b Binding) (Binding, error) {
	return s.cur.Load().CreateBinding(ctx, b)
}

func (s *Switchable) ListBindings(ctx context.Context, workspace string, filter BindingFilter) ([]Binding, error) {
	return s.cur.Load().ListBindings(ctx, workspace, filter)
}

func (s *Switchable) DeleteBinding(ctx context.Context, workspace, id string) error {
	return s.cur.Load().DeleteBinding(ctx, workspace, id)
}
