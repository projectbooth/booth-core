// Package directory is booth-core's minimal user directory (ADR 0047): a mapping from a
// verified token's `sub` to the display claims it most recently presented, so a module can
// show "who" (an owner, an assignee) instead of an opaque identifier.
//
// It's populated opportunistically from tokens core's auth path already verifies — there's
// no registration step, no IdP sync, and no entry for anyone who hasn't signed in here.
// It's display data, not a permissions concept.
//
// One deliberate addition beyond ADR 0047's text: entries remember which workspaces the
// user was a member of when last seen, and reads are scoped to the *caller's active
// workspace*. Without that, any authenticated user in any workspace could enumerate every
// user of every other workspace on a multi-tenant deployment. See
// docs/decisions/0007-user-directory.md.
package directory

import (
	"context"
	"time"
)

// User is one directory entry.
type User struct {
	Sub               string
	PreferredUsername string
	Name              string
	Email             string
	// Workspaces are the workspace slugs the user belonged to the last time a token was
	// seen for them. Used only to scope who may see the entry; never returned to callers.
	Workspaces  []string
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// DisplayName is the best human-readable label available for the user.
func (u User) DisplayName() string {
	for _, s := range []string{u.Name, u.PreferredUsername, u.Email, u.Sub} {
		if s != "" {
			return s
		}
	}
	return ""
}

// Store persists directory entries. Reads take the caller's workspace and only ever return
// entries whose Workspaces include it.
type Store interface {
	// Upsert records u, creating or updating the entry for u.Sub. A display claim that is
	// empty in u never overwrites a non-empty stored value: a token from a client with a
	// narrower scope (no email, say) shouldn't erase what an earlier token supplied.
	Upsert(ctx context.Context, u User) error

	// Get returns the entry for sub if it exists and shares workspace with the caller.
	Get(ctx context.Context, sub, workspace string) (User, bool, error)

	// Search returns entries visible in workspace whose display name, username, or email
	// contains q (case-insensitive; empty q matches everyone), ordered by display name,
	// at most limit of them.
	Search(ctx context.Context, workspace, q string, limit int) ([]User, error)
}

// merge applies ADR 0047's "most recent display claims" rule plus the never-erase rule above.
func merge(old User, in User, exists bool, now time.Time) User {
	out := in
	out.FirstSeenAt = now
	if exists {
		out.FirstSeenAt = old.FirstSeenAt
		keep := func(newVal, oldVal string) string {
			if newVal != "" {
				return newVal
			}
			return oldVal
		}
		out.PreferredUsername = keep(in.PreferredUsername, old.PreferredUsername)
		out.Name = keep(in.Name, old.Name)
		out.Email = keep(in.Email, old.Email)
	}
	out.LastSeenAt = now
	return out
}
