package sidecar

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/projectbooth/booth-core/internal/credentialbroker"
)

// S3Credential is the expected shape of an s3-kind broker response's Credential field — not fixed
// by contracts/credential-broker.md (kind-specific, opaque to the broker), but matching exactly
// what booth-storage's own provider-side design already settled on (booth-storage's docs/
// decisions/0006, cross-referenced from booth-core's docs/decisions/0014): `accessKeyId`,
// `secretAccessKey`, and an optional `sessionToken` — omitted entirely, not sent empty, when the
// request asked for the bare-2-tuple shape Lakekeeper's static-key credential needs.
//
// Endpoint/bucket/keyPrefix/region/pathStyle (also part of that same provider response, for a
// caller that needs to construct its own client against a non-AWS-default endpoint) are NOT
// written anywhere by this mode — the credentials file format has no field for them, and ADR 0095
// only asks this binary to make the credential/connection-string layer invisible, not to configure
// an engine's endpoint. A consuming chart that needs those sets them itself (e.g. as ordinary env
// vars the chart already knows, since it's the one that declared the broker scope in the first
// place).
type S3Credential struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken,omitempty"`
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
	if err := json.Unmarshal(resp.Credential, &cred); err != nil {
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
	return atomicWriteFile(w.Path, []byte(body), 0o600)
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
