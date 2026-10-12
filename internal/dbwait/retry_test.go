package dbwait

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"strings"
	"testing"
	"time"
)

// captureLog redirects the standard logger for the duration of fn and returns what it wrote,
// mirroring internal/lifecycle/identity_test.go's helper of the same name and purpose.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)
	fn()
	return buf.String()
}

// TestRetry_ListenerComesUpAfterDelay is the real-world shape item 66 found: nothing is
// listening on the address yet (the exact "connection refused" the bundled PostgreSQL gives a
// pod that connects in its first few seconds), then a listener starts accepting shortly after.
// Retry must ride out the refusals and succeed once it does, without the caller ever treating
// the first refusal as fatal.
func TestRetry_ListenerComesUpAfterDelay(t *testing.T) {
	// Reserve a free port, then close it immediately so nothing is listening -- the first
	// dial(s) below hit a real "connection refused", not a hypothetical one.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()

	started := make(chan struct{})
	go func() {
		time.Sleep(700 * time.Millisecond)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("starting delayed listener: %v", err)
			return
		}
		close(started)
		defer l.Close()
		conn, err := l.Accept()
		if err == nil {
			conn.Close()
		}
	}()

	attempts := 0
	dial := func() error {
		attempts++
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return err
		}
		return conn.Close()
	}

	start := time.Now()
	err = Retry(context.Background(), "connecting to test listener", 5*time.Second, dial)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Retry() = %v, want nil once the listener came up", err)
	}
	if attempts < 2 {
		t.Errorf("attempts = %d, want at least 2 (the listener wasn't up on the first try)", attempts)
	}
	if elapsed < 700*time.Millisecond {
		t.Errorf("elapsed = %s, want at least 700ms (succeeded before the listener could have started)", elapsed)
	}
	select {
	case <-started:
	default:
		t.Error("the delayed listener never reported starting")
	}
}

// TestRetry_SucceedsOnFirstTry confirms the common case does one attempt and no waiting.
func TestRetry_SucceedsOnFirstTry(t *testing.T) {
	attempts := 0
	err := Retry(context.Background(), "x", time.Second, func() error {
		attempts++
		return nil
	})
	if err != nil {
		t.Fatalf("Retry() = %v, want nil", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1", attempts)
	}
}

// TestRetry_GivesUpAfterMaxElapsed confirms a connection that never comes up still fails --
// loudly, with the underlying error wrapped in -- rather than retrying forever.
func TestRetry_GivesUpAfterMaxElapsed(t *testing.T) {
	wantErr := errors.New("connection refused")
	attempts := 0
	err := Retry(context.Background(), "connecting to nowhere", 1200*time.Millisecond, func() error {
		attempts++
		return wantErr
	})
	if err == nil {
		t.Fatal("Retry() = nil, want an error once maxElapsed has passed")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Retry() = %v, want it to wrap %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "connecting to nowhere") {
		t.Errorf("Retry() = %v, want it to name what it was trying to do", err)
	}
	if attempts < 2 {
		t.Errorf("attempts = %d, want more than 1 before giving up", attempts)
	}
}

// TestRetry_LogsEachRetry confirms an operator watching logs at boot sees the wait, per the
// ADR 0113-adjacent convention (identity_test.go's captureLog) of asserting log lines are
// actually emitted, not just assumed.
func TestRetry_LogsEachRetry(t *testing.T) {
	logged := captureLog(t, func() {
		_ = Retry(context.Background(), "connecting to test db", 600*time.Millisecond, func() error {
			return errors.New("connection refused")
		})
	})
	if !strings.Contains(logged, "connecting to test db") {
		t.Errorf("log output = %q, want it to mention what Retry was trying to do", logged)
	}
	if !strings.Contains(logged, "retrying in") {
		t.Errorf("log output = %q, want it to report a retry", logged)
	}
}

// TestRetry_StopsOnContextCancellation confirms Retry doesn't keep sleeping past a canceled
// context, even if maxElapsed hasn't been reached yet.
func TestRetry_StopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempts := 0
	start := time.Now()
	err := Retry(ctx, "x", time.Minute, func() error {
		attempts++
		return errors.New("connection refused")
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Retry() = %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Errorf("elapsed = %s, want Retry to return promptly once ctx is canceled", elapsed)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1 (fn runs once before the first ctx check)", attempts)
	}
}
