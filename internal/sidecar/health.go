package sidecar

import (
	"context"
	"net/http"
)

// ServeHealthz runs a small HTTP server exposing GET /healthz (contracts/credential-sidecar.md),
// for `--kind=s3`, which has no wire-protocol listener of its own to piggyback health onto the way
// `postgres` mode does (see PostgresProxy.handleConn's HTTP-sniffing). addr must be a loopback
// address — enforced by the caller/chart, not re-validated here (matching PostgresProxy.Listen's
// own posture).
func ServeHealthz(ctx context.Context, addr string, ready func() bool) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if ready() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
