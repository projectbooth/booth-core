package sidecar

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/projectbooth/booth-core/internal/credentialbroker"
)

// S3Credential is the expected shape of an s3-kind broker response's Credential field — not fixed
// by contracts/credential-broker.md (kind-specific, opaque to the broker), but matching exactly
// what booth-storage's own provider actually returns (`internal/credentialbroker/provider.go`'s
// `s3CredentialBody`, booth-storage's docs/decisions/0006, cross-referenced from booth-core's
// docs/decisions/0014/0015): `accessKeyId`, `secretAccessKey`, an optional `sessionToken` —
// omitted entirely, not sent empty, when the request asked for the bare-2-tuple shape Lakekeeper's
// static-key credential needs — plus the resolved real-world location fields (ADR 0095's third
// amendment, 2026-10-05) this struct must also declare even though not every field is written to
// disk: decodeStrict (internal/sidecar/broker.go) refuses any field the real provider sends that
// this struct doesn't know about, so Bucket/KeyPrefix/PathStyle are declared here for that reason
// alone, not because S3FileWriter writes them anywhere.
type S3Credential struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken,omitempty"`

	// Endpoint and Region are written to the shared config file (write, below) when Endpoint is
	// non-empty (a self-hosted backend, e.g. MinIO) — omitted entirely for real AWS S3, which has
	// no endpoint to set. Bucket/KeyPrefix/PathStyle are decoded but not written anywhere by this
	// mode: a consuming engine's own bucket/path (PyIceberg's warehouse location) is resolved
	// separately, via booth-lakehouse's GET /api/warehouse, not from this file.
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket"`
	KeyPrefix string `json:"keyPrefix,omitempty"`
	PathStyle bool   `json:"pathStyle"`
}

// S3FileWriter is the `--kind=s3` mode: on every lease (the first one, and every renewal), writes
// a standard AWS shared-credentials-file to Path, atomically (contracts/credential-sidecar.md: "a
// temp file and rename ... so a reader never observes a partially-written file"). Profile is the
// credentials-file section name a reader's AWS_PROFILE (or SDK default, "default") selects.
type S3FileWriter struct {
	Path    string
	Profile string
}

// NewS3FileWriter builds a writer; an empty profile defaults to "default", matching every AWS SDK's
// own default profile name so a consumer needs no extra configuration beyond pointing
// AWS_SHARED_CREDENTIALS_FILE (or boto3/DuckDB/PyIceberg's equivalent) at Path.
func NewS3FileWriter(path, profile string) *S3FileWriter {
	if profile == "" {
		profile = "default"
	}
	return &S3FileWriter{Path: path, Profile: profile}
}

// OnRenew is a Renewer.OnRenew callback: parses resp.Credential as an S3Credential and writes it.
// A parse or write failure is logged, never panics or exits — the previous file (if any) is left
// in place untouched, matching the contract's "a failed renewal leaves the previous file in place
// until one succeeds or the lease expires."
func (w *S3FileWriter) OnRenew(resp credentialbroker.Response) {
	var cred S3Credential
	if err := decodeStrict(resp.Credential, &cred); err != nil {
		log.Printf("sidecar: s3 credential from lease %s is unparseable, keeping the previous file: %v", resp.LeaseID, err)
		return
	}
	if err := w.write(cred); err != nil {
		log.Printf("sidecar: writing credentials file from lease %s failed, keeping the previous file: %v", resp.LeaseID, err)
	}
}

func (w *S3FileWriter) write(cred S3Credential) error {
	var body string
	body += fmt.Sprintf("[%s]\n", w.Profile)
	body += fmt.Sprintf("aws_access_key_id = %s\n", cred.AccessKeyID)
	body += fmt.Sprintf("aws_secret_access_key = %s\n", cred.SecretAccessKey)
	if cred.SessionToken != "" {
		body += fmt.Sprintf("aws_session_token = %s\n", cred.SessionToken)
	}
	if err := atomicWriteFile(w.Path, []byte(body), 0o600); err != nil {
		return err
	}
	return w.writeConfig(cred)
}

// configPath is the base --credentials-file path's companion AWS shared-config file (ADR 0095's
// third amendment, 2026-10-05, contracts/credential-sidecar.md's `--credentials-file` row): a
// standard AWS shared-credentials-file and a standard AWS shared-config-file are deliberately kept
// as two separate files, matching AWS's own convention (endpoint/region never live in the
// credentials file) — not a booth-core invention.
func (w *S3FileWriter) configPath() string {
	return w.Path + ".config"
}

// writeConfig writes (or, for a lease with no endpoint, removes) the companion config file. Real
// AWS S3 has no endpoint to set, so that case omits the file entirely rather than writing one with
// an empty endpoint_url — including removing a previous self-hosted backend's leftover config if
// this writer's backend ever changes kind across a renewal.
func (w *S3FileWriter) writeConfig(cred S3Credential) error {
	path := w.configPath()
	if cred.Endpoint == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing stale config file %s: %w", path, err)
		}
		return nil
	}

	// AWS's shared config file (unlike the credentials file) names a non-default profile's
	// section "[profile <name>]", not "[<name>]" — botocore and every mainstream SDK require this
	// exact distinction to actually find the section.
	section := w.Profile
	if section != "default" {
		section = "profile " + section
	}
	var body string
	body += fmt.Sprintf("[%s]\n", section)
	body += fmt.Sprintf("endpoint_url = %s\n", cred.Endpoint)
	if cred.Region != "" {
		body += fmt.Sprintf("region = %s\n", cred.Region)
	}
	// addressing_style is botocore's own existing key (ADR 0095's fourth amendment,
	// 2026-10-05): DuckDB's S3 client needs path-style addressing against a self-hosted
	// backend (measured against a real MinIO by booth-notebooks) and doesn't read it from
	// anywhere else. Written unconditionally to "path" for every self-hosted lease — gated on
	// the same cred.Endpoint presence as endpoint_url/region above, not on cred.PathStyle
	// (which reflects booth-storage's own bucket-addressing choice, a separate question from
	// what this config file needs to hand a consuming engine).
	body += "addressing_style = path\n"
	return atomicWriteFile(path, []byte(body), 0o600)
}

// atomicWriteFile writes data to a temp file in the same directory as path, then renames it over
// path — rename is atomic on the same filesystem (POSIX and Windows NTFS both guarantee this),
// so a concurrent reader either sees the old complete file or the new complete file, never a
// partial write. The temp file is created with the final permissions directly (not chmod'd after
// the fact) since this file holds a live, unrevocable-until-expiry credential.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".credential-sidecar-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Any early return below must still attempt cleanup of a temp file that never got renamed —
	// a no-op once the rename below has already succeeded, since the path no longer exists.
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("setting permissions on %s: %w", tmpPath, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpPath, path, err)
	}
	return nil
}
