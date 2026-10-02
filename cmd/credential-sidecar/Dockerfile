# syntax=docker/dockerfile:1
#
# ADR 0095 / contracts/credential-sidecar.md: a small, statically-built Go binary (no JVM/
# interpreter startup cost) published as ghcr.io/projectbooth/credential-sidecar, digest-pinned by
# every consuming chart — the same build shape as booth-core's own Dockerfile (same base images,
# same static-build flags), since this binary makes exactly the same "no runtime nothing else uses"
# choice ADR 0079 already made for Lakekeeper.
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/credential-sidecar ./cmd/credential-sidecar

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/credential-sidecar /credential-sidecar
USER nonroot:nonroot
ENTRYPOINT ["/credential-sidecar"]
