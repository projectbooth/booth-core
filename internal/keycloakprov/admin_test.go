package keycloakprov

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnsureAdminPassword_StableAcrossCalls(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().Build()

	first, err := EnsureAdminPassword(ctx, c, "booth-system")
	if err != nil || len(first) < 32 {
		t.Fatalf("first = %q, %v", first, err)
	}
	again, err := EnsureAdminPassword(ctx, c, "booth-system")
	if err != nil || again != first {
		t.Fatalf("not stable across calls: %q vs %q (%v)", again, first, err)
	}
}

func TestEnsureAdminPassword_UsernameIsFixed(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().Build()

	if _, err := EnsureAdminPassword(ctx, c, "booth-system"); err != nil {
		t.Fatal(err)
	}

	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "booth-system", Name: AdminSecretName}, &s); err != nil {
		t.Fatal(err)
	}
	if got := secretString(&s, AdminUsernameKey); got != AdminUsername {
		t.Errorf("username = %q, want %q", got, AdminUsername)
	}
}
