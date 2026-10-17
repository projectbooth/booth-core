package serviceaccount

import (
	"context"
	"errors"
	"testing"
)

func TestUnavailable_EveryCallRefusesRatherThanServingFromMemory(t *testing.T) {
	ctx := context.Background()
	var s Store = Unavailable{}

	if err := s.Create(ctx, Account{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Create = %v, want ErrUnavailable", err)
	}
	if _, _, err := s.Get(ctx, "acme", "bot"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Get = %v, want ErrUnavailable", err)
	}
	if _, err := s.List(ctx, "acme"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("List = %v, want ErrUnavailable", err)
	}
	if _, err := s.Update(ctx, "acme", "bot", Patch{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Update = %v, want ErrUnavailable", err)
	}
	if err := s.Delete(ctx, "acme", "bot"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Delete = %v, want ErrUnavailable", err)
	}
}

// Switchable starts refusing and, once swapped, serves for real — the shape core's startup
// needs while its own database is still provisioning (cmd/core/main.go).
func TestSwitchable_SwapsFromUnavailableToARealStore(t *testing.T) {
	ctx := context.Background()
	sw := NewSwitchable(Unavailable{})

	if _, _, err := sw.Get(ctx, "acme", "bot"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("before swap: Get = %v, want ErrUnavailable", err)
	}

	real := newTestStore(t)
	sw.Swap(real)

	if err := sw.Create(ctx, bot("acme")); err != nil {
		t.Fatalf("after swap: Create = %v, want success", err)
	}
	if _, ok, err := sw.Get(ctx, "acme", "nfl-bot"); err != nil || !ok {
		t.Fatalf("after swap: Get = _, %v, %v, want true, nil", ok, err)
	}
}
