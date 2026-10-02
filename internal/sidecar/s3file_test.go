package sidecar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projectbooth/booth-core/internal/credentialbroker"
)

func TestS3FileWriter_WritesTheDocumentedFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aws-credentials")
	w := NewS3FileWriter(path, "")

	w.OnRenew(credentialbroker.Response{
		LeaseID: "lease-1", ExpiresAt: time.Now().Add(time.Minute),
		Credential: mustJSON(t, S3Credential{AccessKeyID: "AKIA123", SecretAccessKey: "secret-value", SessionToken: "tok"}),
	})

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{"[default]", "aws_access_key_id = AKIA123", "aws_secret_access_key = secret-value", "aws_session_token = tok"} {
		if !strings.Contains(s, want) {
			t.Errorf("file missing %q; got:\n%s", want, s)
		}
	}
}

func TestS3FileWriter_OmitsSessionTokenWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aws-credentials")
	w := NewS3FileWriter(path, "default")
	w.OnRenew(credentialbroker.Response{
		LeaseID: "lease-1", ExpiresAt: time.Now().Add(time.Minute),
		Credential: mustJSON(t, S3Credential{AccessKeyID: "AKIA123", SecretAccessKey: "secret-value"}),
	})
	got, _ := os.ReadFile(path)
	if strings.Contains(string(got), "aws_session_token") {
		t.Errorf("session token line present although none was issued (a bare-pair grant must look exactly like one with no token field, not an empty one): %s", got)
	}
}

func TestS3FileWriter_RespectsACustomProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aws-credentials")
	w := NewS3FileWriter(path, "lakehouse")
	w.OnRenew(credentialbroker.Response{
		LeaseID: "lease-1", ExpiresAt: time.Now().Add(time.Minute),
		Credential: mustJSON(t, S3Credential{AccessKeyID: "a", SecretAccessKey: "b"}),
	})
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "[lakehouse]") {
		t.Errorf("profile section missing: %s", got)
	}
}

// --- The hard requirement the task calls out by name: atomic refresh, old file intact on failure. --

func TestS3FileWriter_RefreshIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aws-credentials")
	w := NewS3FileWriter(path, "")

	w.OnRenew(credentialbroker.Response{LeaseID: "1", ExpiresAt: time.Now().Add(time.Minute), Credential: mustJSON(t, S3Credential{AccessKeyID: "first", SecretAccessKey: "x"})})
	first, _ := os.ReadFile(path)

	w.OnRenew(credentialbroker.Response{LeaseID: "2", ExpiresAt: time.Now().Add(time.Minute), Credential: mustJSON(t, S3Credential{AccessKeyID: "second", SecretAccessKey: "y"})})
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(first), "second") {
		t.Fatal("test setup sanity check failed")
	}
	if !strings.Contains(string(second), "second") || strings.Contains(string(second), "first") {
		t.Errorf("file was not fully replaced by the refresh: %s", second)
	}

	// No leftover temp files: rename, not copy-then-truncate.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory has %d entries, want 1 (the credentials file only): %v", len(entries), names)
	}
}

func TestS3FileWriter_AFailedRenewalLeavesThePreviousFileInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aws-credentials")
	w := NewS3FileWriter(path, "")
	w.OnRenew(credentialbroker.Response{LeaseID: "1", ExpiresAt: time.Now().Add(time.Minute), Credential: mustJSON(t, S3Credential{AccessKeyID: "good", SecretAccessKey: "x"})})
	before, _ := os.ReadFile(path)

	// An unparseable credential (the provider sent something this sidecar's expected shape
	// doesn't match) must not touch the file at all.
	w.OnRenew(credentialbroker.Response{LeaseID: "2", ExpiresAt: time.Now().Add(time.Minute), Credential: json.RawMessage(`"not an object"`)})
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("file changed after an unparseable renewal: before=%q after=%q", before, after)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
