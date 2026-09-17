# syntax=docker/dockerfile:1
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/booth-core ./cmd/core

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/booth-core /booth-core
USER nonroot:nonroot
ENTRYPOINT ["/booth-core"]
