package directory

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryStore keeps the directory in process memory. It's what core uses when no
// PostgreSQL DSN is configured: entries are lost on restart, but they repopulate on their
// own as users make authenticated requests, so it degrades gracefully rather than breaking.
type MemoryStore struct {
	mu    sync.RWMutex
	users map[string]User
	now   func() time.Time
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{users: map[string]User{}, now: time.Now}
}

func (m *MemoryStore) Upsert(_ context.Context, u User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, exists := m.users[u.Sub]
	m.users[u.Sub] = merge(old, u, exists, m.now())
	return nil
}

func (m *MemoryStore) Get(_ context.Context, sub, workspace string) (User, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[sub]
	if !ok || !inWorkspace(u, workspace) {
		return User{}, false, nil
	}
	return u, true, nil
}

func (m *MemoryStore) Search(_ context.Context, workspace, q string, limit int) ([]User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	needle := strings.ToLower(q)
	var out []User
	for _, u := range m.users {
		if !inWorkspace(u, workspace) {
			continue
		}
		if needle == "" || containsFold(u.Name, needle) || containsFold(u.PreferredUsername, needle) || containsFold(u.Email, needle) {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].DisplayName()), strings.ToLower(out[j].DisplayName())
		if a != b {
			return a < b
		}
		return out[i].Sub < out[j].Sub
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func inWorkspace(u User, workspace string) bool {
	for _, w := range u.Workspaces {
		if w == workspace {
			return true
		}
	}
	return false
}

func containsFold(hay, lowerNeedle string) bool {
	return strings.Contains(strings.ToLower(hay), lowerNeedle)
}
