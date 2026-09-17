# booth-core decision 0003: Local-dev registry fallback

Status: informational — this is a booth-core-internal convenience, not a cross-cutting
contract change, so unlike decisions 0001 and 0002 it doesn't need promotion to a
`booth-architecture` ADR. Recorded here because `agent-briefs/core.md` calls it out as an
open question worth a documented answer.

## Context

ADR 0019 fixed BoothModule CRD watching as the only production discovery mechanism, but
flagged that local development or non-Kubernetes testing of a module in isolation needs
some lightweight way to fake manifest presence, without requiring every module developer
to stand up a `kind`/`k3d` cluster just to see their module show up in core's registry.

## Decision

`BOOTH_DEV_REGISTRY_PATH`, when set, makes `booth-core` load module entries from a static
YAML file (`internal/devregistry`) instead of starting the real CRD controller
(`internal/registry/controller.go`). The file format mirrors
`contracts/module-manifest.md`'s fields plus a `host` field (a directly-dialable
`host:port`) replacing the real `ServiceRef`'s Kubernetes Service coordinates — see
`hack/dev-registry.example.yaml` for the shape.

Modules loaded this way are marked `Healthy` unconditionally and never re-checked — dev
mode trusts the file rather than polling, since the entire point is running without the
cluster DNS the real health-check URL construction assumes. There's no status
subresource to update either, since there's no real `BoothModule` object.

This is strictly a development convenience: `BOOTH_DEV_REGISTRY_PATH` is not read by any
Helm chart template and has no equivalent in a real deployment. A module agent working
locally runs their module's process directly (e.g. `go run ./cmd/mymodule` or `npm run
dev`), lists it with a `host: localhost:PORT` entry in their own copy of a dev registry
file, and points their local `booth-core` at it — enough to exercise the gateway routing
and iframe-proxy flow against a real running module backend without any Kubernetes
involved at all.

## Consequences

- A module agent can develop against a real `booth-core` binary before their module ships
  a Helm chart or BoothModule CRD template.
- This intentionally does not exercise ADR 0019's actual CRD reconciliation, ADR 0020's
  secrets provisioning, or the health-polling logic in `internal/registry/controller.go`
  — those still need the real-cluster integration test layer
  (`contracts/testing-strategy.md` layer 3) to be verified for real. Dev mode is for fast
  local iteration on the gateway/API surface, not a substitute for that layer.
