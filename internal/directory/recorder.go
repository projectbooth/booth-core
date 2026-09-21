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

// Reset forgets which identities have been written, so the next request from each user
// writes them again. Call it after swapping the backing store (see Switchable): the new store
// starts empty, and without this a user wouldn't be re-recorded until the debounce interval
// passed.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = map[string]seenEntry{}
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
		Roles:             rolesOf(memberships),
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

// rolesOf maps each workspace to the caller's role in it. If a token lists several roles for
// one workspace (ambiguous under ADR 0025, which doesn't say which wins), the least
// privileged is recorded: this feeds a cap on what a run may do (ADR 0056), and the cap must
// never round up.
func rolesOf(ms []auth.Membership) map[string]string {
	out := make(map[string]string, len(ms))
	for _, m := range ms {
		if prev, ok := out[m.Workspace]; ok && auth.RoleRank(auth.Role(prev)) <= auth.RoleRank(m.Role) {
			continue
		}
		out[m.Workspace] = string(m.Role)
	}
	return out
}

// fingerprint includes roles, so a demotion is written immediately rather than waiting out
// the debounce interval — a stale higher role is the one thing workload identity must not see.
func fingerprint(u User) string {
	roles := make([]string, 0, len(u.Roles))
	for w, r := range u.Roles {
		roles = append(roles, w+"="+r)
	}
	sort.Strings(roles)
	return strings.Join([]string{u.PreferredUsername, u.Name, u.Email, strings.Join(u.Workspaces, ","), strings.Join(roles, ",")}, "\x00")
}
