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

// --- Regression: decodes booth-storage's real provider response, including location fields. --
//
// booth-storage's real s3-kind provider (internal/credentialbroker/provider.go's s3CredentialBody)
// already returns endpoint/region/bucket/keyPrefix/pathStyle alongside the keys, on every lease.
// This test's JSON is hand-copied from that struct's own field list and tags, the same discipline
// used for the postgres-kind regression test (postgres_test.go) and for the same reason: a fixture
// shaped to match this package's own struct would never catch a real drift from the provider's
// actual response.
func TestS3Credential_DecodesBoothStoragesRealResponseShape(t *testing.T) {
	raw := []byte(`{
		"endpoint": "https://booth-storage-minio.booth-storage.svc:9000",
		"region": "us-east-1",
		"bucket": "booth-ws-0123456789abcdef01234567",
		"keyPrefix": "lakehouse/",
		"pathStyle": true,
		"accessKeyId": "AKIAMINTEDKEY",
		"secretAccessKey": "s3cr3t-minted-key",
		"sessionToken": "minted-session-token"
	}`)

	var cred S3Credential
	if err := decodeStrict(raw, &cred); err != nil {
		t.Fatalf("decoding booth-storage's real response shape: %v", err)
	}
	if cred.Endpoint != "https://booth-storage-minio.booth-storage.svc:9000" {
		t.Errorf("Endpoint = %q", cred.Endpoint)
	}
	if cred.Region != "us-east-1" {
		t.Errorf("Region = %q", cred.Region)
	}
	if cred.Bucket != "booth-ws-0123456789abcdef01234567" {
		t.Errorf("Bucket = %q", cred.Bucket)
	}
	if cred.KeyPrefix != "lakehouse/" {
		t.Errorf("KeyPrefix = %q", cred.KeyPrefix)
	}
	if !cred.PathStyle {
		t.Errorf("PathStyle = %v, want true", cred.PathStyle)
	}
	if cred.AccessKeyID != "AKIAMINTEDKEY" {
		t.Errorf("AccessKeyID = %q", cred.AccessKeyID)
	}
	if cred.SecretAccessKey != "s3cr3t-minted-key" {
		t.Errorf("SecretAccessKey = %q", cred.SecretAccessKey)
	}
	if cred.SessionToken != "minted-session-token" {
		t.Errorf("SessionToken = %q", cred.SessionToken)
	}
}

// Before ADR 0095's third amendment, S3Credential declared only accessKeyId/secretAccessKey/
// sessionToken. Since OnRenew already used decodeStrict (DisallowUnknownFields), a real
// booth-storage response's endpoint/region/bucket/keyPrefix/pathStyle fields would have made every
// real lease unparseable — this pins that the full field set is now declared, not just the keys.
func TestS3Credential_RealResponseShapeDoesNotErrorOnUnknownFields(t *testing.T) {
	raw := []byte(`{"endpoint":"https://minio:9000","region":"us-east-1","bucket":"b","keyPrefix":"p/","pathStyle":false,"accessKeyId":"a","secretAccessKey":"b"}`)
	var cred S3Credential
	if err := decodeStrict(raw, &cred); err != nil {
		t.Fatalf("a real provider response with all its documented fields must decode cleanly: %v", err)
	}
}

func TestS3FileWriter_WritesACompanionConfigFileWhenTheLeaseHasAnEndpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aws-credentials")
	w := NewS3FileWriter(path, "")
	w.OnRenew(credentialbroker.Response{
		LeaseID: "lease-1", ExpiresAt: time.Now().Add(time.Minute),
		Credential: mustJSON(t, S3Credential{
			AccessKeyID: "a", SecretAccessKey: "b",
			Endpoint: "https://booth-storage-minio.booth-storage.svc:9000", Region: "us-east-1",
		}),
	})

	got, err := os.ReadFile(path + ".config")
	if err != nil {
		t.Fatalf("reading companion config file: %v", err)
	}
	s := string(got)
	for _, want := range []string{"[default]", "endpoint_url = https://booth-storage-minio.booth-storage.svc:9000", "region = us-east-1"} {
		if !strings.Contains(s, want) {
			t.Errorf("config file missing %q; got:\n%s", want, s)
		}
	}
}

func TestS3FileWriter_ConfigFileUsesProfilePrefixForANonDefaultProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aws-credentials")
	w := NewS3FileWriter(path, "lakehouse")
	w.OnRenew(credentialbroker.Response{
		LeaseID: "lease-1", ExpiresAt: time.Now().Add(time.Minute),
		Credential: mustJSON(t, S3Credential{AccessKeyID: "a", SecretAccessKey: "b", Endpoint: "https://minio:9000"}),
	})

	got, err := os.ReadFile(path + ".config")
	if err != nil {
		t.Fatalf("reading companion config file: %v", err)
	}
	if !strings.Contains(string(got), "[profile lakehouse]") {
		t.Errorf("config file section missing AWS's required \"profile \" prefix for a non-default profile: %s", got)
	}
}

func TestS3FileWriter_OmitsTheConfigFileEntirelyForRealAWSS3(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aws-credentials")
	w := NewS3FileWriter(path, "")
	w.OnRenew(credentialbroker.Response{
		LeaseID: "lease-1", ExpiresAt: time.Now().Add(time.Minute),
		Credential: mustJSON(t, S3Credential{AccessKeyID: "a", SecretAccessKey: "b"}), // no Endpoint: real AWS S3
	})

	if _, err := os.Stat(path + ".config"); !os.IsNotExist(err) {
		t.Errorf("a lease with no endpoint (real AWS S3) must not produce a config file at all, got err=%v", err)
	}
}

func TestS3FileWriter_RemovesAStaleConfigFileIfALaterLeaseHasNoEndpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aws-credentials")
	w := NewS3FileWriter(path, "")
	w.OnRenew(credentialbroker.Response{
		LeaseID: "1", ExpiresAt: time.Now().Add(time.Minute),
		Credential: mustJSON(t, S3Credential{AccessKeyID: "a", SecretAccessKey: "b", Endpoint: "https://minio:9000"}),
	})
	if _, err := os.Stat(path + ".config"); err != nil {
		t.Fatalf("test setup: config file should exist after the first, self-hosted lease: %v", err)
	}

	w.OnRenew(credentialbroker.Response{
		LeaseID: "2", ExpiresAt: time.Now().Add(time.Minute),
		Credential: mustJSON(t, S3Credential{AccessKeyID: "a2", SecretAccessKey: "b2"}),
	})
	if _, err := os.Stat(path + ".config"); !os.IsNotExist(err) {
		t.Errorf("a later lease with no endpoint must remove the previous lease's now-stale config file, got err=%v", err)
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
