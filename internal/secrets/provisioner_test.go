package secrets

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestProvisionSecret_CreatesThenUpdates(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	p := NewProvisioner(c)
	owner := metav1.OwnerReference{APIVersion: "v1", Kind: "Namespace", Name: "booth-storage", UID: "abc"}

	ctx := context.Background()
	if err := p.ProvisionSecret(ctx, "booth-storage", "storage-creds", map[string]string{"key": "v1"}, owner); err != nil {
		t.Fatalf("first ProvisionSecret: %v", err)
	}

	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "booth-storage", Name: "storage-creds"}, &secret); err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	if secret.StringData["key"] != "v1" {
		t.Fatalf("StringData[key] = %q, want v1", secret.StringData["key"])
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Name != "booth-storage" {
		t.Fatalf("OwnerReferences = %v, want owner set", secret.OwnerReferences)
	}

	// Re-provisioning with new data should update in place, not error on already-exists.
	if err := p.ProvisionSecret(ctx, "booth-storage", "storage-creds", map[string]string{"key": "v2"}, owner); err != nil {
		t.Fatalf("second ProvisionSecret: %v", err)
	}

	if err := c.Get(ctx, types.NamespacedName{Namespace: "booth-storage", Name: "storage-creds"}, &secret); err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if secret.StringData["key"] != "v2" {
		t.Fatalf("StringData[key] after update = %q, want v2", secret.StringData["key"])
	}
}

func TestProvisionConfigMap_CreatesThenUpdates(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	p := NewProvisioner(c)
	owner := metav1.OwnerReference{APIVersion: "v1", Kind: "Namespace", Name: "booth-storage", UID: "abc"}

	ctx := context.Background()
	if err := p.ProvisionConfigMap(ctx, "booth-storage", "storage-config", map[string]string{"endpoint": "v1"}, owner); err != nil {
		t.Fatalf("first ProvisionConfigMap: %v", err)
	}

	if err := p.ProvisionConfigMap(ctx, "booth-storage", "storage-config", map[string]string{"endpoint": "v2"}, owner); err != nil {
		t.Fatalf("second ProvisionConfigMap: %v", err)
	}

	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: "booth-storage", Name: "storage-config"}, &cm); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if cm.Data["endpoint"] != "v2" {
		t.Fatalf("Data[endpoint] = %q, want v2", cm.Data["endpoint"])
	}
}
