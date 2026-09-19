package directory

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/projectbooth/booth-core/internal/auth"
)

const (
	// DefaultRecordInterval is how often an unchanged identity is re-written. Every
	// authenticated request passes through Observe, so without this it would be a
	// database write per request; with it, it's one per user per interval (plus one
	// immediately whenever their claims or memberships actually change).
	DefaultRecordInterval = 5 * time.Minute

	recordTimeout = 2 * time.Second
)

// Recorder upserts directory entries from verified tokens (ADR 0047's "opportunistic"
// population). Its Observe method is an auth.ClaimsObserver.
type Recorder struct {
	Store    Store
	Interval time.Duration
	Now      func() time.Time

	mu   sync.Mutex
	seen map[string]seenEntry
}

type seenEntry struct {
	fingerprint string
	at          time.Time
}

func NewRecorder(store Store) *Recorder {
	return &Recorder{Store: store, Interval: DefaultRecordInterval, Now: time.Now, seen: map[string]seenEntry{}}
}

// Observe records the identity behind a verified token. It never fails the request: a
// directory that's down or slow costs a log line, not a login.
func (r *Recorder) Observe(ctx context.Context, claims *auth.Claims, memberships []auth.Membership) {
	if claims == nil || claims.Subject == "" {
		return
	}

	u := User{
		Sub:               claims.Subject,
		PreferredUsername: claims.PreferredUsername,
		Name:              claims.Name,
		Email:             claims.Email,
		Workspaces:        workspacesOf(memberships),
	}

	fp := fingerprint(u)
	now := r.Now()
	r.mu.Lock()
	prev, ok := r.seen[u.Sub]
	if ok && prev.fingerprint == fp && now.Sub(prev.at) < r.Interval {
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	// Detached from the request's cancellation: a client hanging up shouldn't abort the
	// write, but it's still bounded so a slow database can't pin the request goroutine.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := r.Store.Upsert(wctx, u); err != nil {
		// Not remembered as seen, so the next request retries.
		log.Printf("user directory: recording %q failed: %v", u.Sub, err)
		return
	}

	r.mu.Lock()
	r.seen[u.Sub] = seenEntry{fingerprint: fp, at: now}
	r.mu.Unlock()
}

func workspacesOf(ms []auth.Membership) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if _, dup := seen[m.Workspace]; dup {
			continue
		}
		seen[m.Workspace] = struct{}{}
		out = append(out, m.Workspace)
	}
	sort.Strings(out)
	return out
}

func fingerprint(u User) string {
	return strings.Join([]string{u.PreferredUsername, u.Name, u.Email, strings.Join(u.Workspaces, ",")}, "\x00")
}
