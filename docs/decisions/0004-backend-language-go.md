# booth-core decision 0004: Backend language and framework — Go

Status: informational — an implementation choice internal to this repo, not a
cross-cutting contract. Recorded for the same reason `contracts/testing-strategy.md`
leaves per-language framework choice "left to each module": booth-architecture doesn't
mandate a backend language for any repo, `booth-design`'s frontend stack (ADR 0009) is
the only stack decision that came from the coordinator.

## Context

`agent-briefs/core.md` and `ARCHITECTURE.md` don't specify a language for booth-core's
backend. The instruction to "start with repo scaffolding... before diving into any one
subsystem" implied choosing one before writing code.

## Decision

Go, using:

- `sigs.k8s.io/controller-runtime` for the BoothModule CRD watch/reconcile loop
  (ADR 0019) and the Secret/ConfigMap provisioning reconciler (ADR 0020).
- `github.com/coreos/go-oidc` for OIDC discovery/JWKS-based JWT verification
  (ADR 0004).
- `github.com/nats-io/nats.go` (with its `jetstream` package) for the event bus
  (ADR 0021).
- `github.com/go-chi/chi` for the HTTP router (gateway + core's own small API).
- Standard library `net/http/httputil.ReverseProxy` for the gateway's actual proxying.

## Why

ADR 0003 already frames booth-core's central job as fundamentally a reconcile-loop
problem (module install/uninstall lifecycle, and now also the BoothModule CRD watch and
Secret/ConfigMap provisioning). The Kubernetes controller ecosystem
(`controller-runtime`, `client-go`, `controller-gen` for CRD/deepcopy generation) is
Go-native and is what nearly every real-world Kubernetes controller/operator is written
in — reaching for a different language here would mean either giving up that tooling or
reimplementing informal equivalents of it. NATS, OIDC, and reverse-proxying all also have
mature, idiomatic Go support, so there's no subsystem pulling toward a different runtime.

## Consequences

- Every other module agent is free to pick their own backend language/framework — nothing
  about booth-core's implementation language is part of any contract another repo builds
  against. `contracts/core-platform-api.md` and `contracts/module-manifest.md` are
  language-agnostic by design (HTTP, JWTs, Kubernetes CRDs, NATS subjects) specifically so
  this holds.
- CI (`contracts/testing-strategy.md`) uses `go test`/`go vet`/`gofmt` for booth-core's own
  layer 1–2 checks; this is booth-core-specific CI configuration, not a cross-repo
  standard.
