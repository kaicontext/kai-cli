package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os/exec"
	"testing"

	"github.com/kaicontext/kai-engine/telemetry"
)

func TestErrorClassOf(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{context.DeadlineExceeded, "timeout"},
		{context.Canceled, "cancelled"},
		{&net.DNSError{Err: "no such host"}, "dns"},
		{&net.DNSError{Err: "i/o timeout", IsTimeout: true}, "dns"}, // a lookup that timed out is still dns
		{&net.OpError{Op: "dial", Err: errors.New("connection refused")}, "network"},
		{x509.CertificateInvalidError{Reason: x509.Expired}, "tls"},
		{fmt.Errorf("get: %w", &tls.CertificateVerificationError{Err: x509.CertificateInvalidError{Reason: x509.Expired}}), "tls"}, // as Go's client wraps it
		{&net.OpError{Op: "dial", Err: x509.UnknownAuthorityError{}}, "tls"},                                                       // the trust problem, not the connection
		{fmt.Errorf("wrap: %w", fs.ErrPermission), "permission"},
		{fs.ErrNotExist, "not_found"},
		{exec.ErrNotFound, "tool_missing"},
		{&exec.ExitError{}, "exit_status"},
		{errors.New("database is locked"), "db_locked"},
		{errors.New("Kai not initialized. Run 'kai init' first"), "no_project"},
		{errors.New("remote 'origin' not configured (use 'kai remote set origin <url>')"), "no_remote"},
		{errors.New("server returned 401 Unauthorized"), "auth"},
		{errors.New("HTTP 503 Service Unavailable"), "server"},
		{errors.New("server error: 403 forbidden"), "auth"},
		{errors.New("server unhealthy: status 502"), "server"},
		{errors.New("HTTP 429: slow down"), "rate_limited"},
		{errors.New("server error: 404 not found"), "not_found"},
		{errors.New("server error: 402 payment required"), "no_credit"},
		{errors.New("HTTP 409: conflict"), "http_4xx"}, // a refusal that is none of the named ones
		// Three digits are not a status unless the message says so.
		{errors.New("shadow run verdict: missed (500 false negatives)"), "other"},
		{errors.New("wrote 401 bytes to http://localhost:5000/v1/objects"), "other"},
		{errors.New("post http://127.0.0.1:8429/api: connection reset"), "other"},
		{errors.New("something else entirely"), "other"},
	}
	for _, c := range cases {
		if got := errorClassOf(c.err); got != c.want {
			t.Errorf("errorClassOf(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// A command's returned error becomes result=error with a class; a result
// or class the command set itself (capture's capture_lock) is kept; a
// nil event (telemetry off) and a nil error are no-ops.
func TestApplyResult(t *testing.T) {
	applyResult(nil, errors.New("x")) // must not panic

	te := &telemetry.Event{Result: "ok"}
	applyResult(te, nil)
	if te.Result != "ok" || te.ErrorClass != "" {
		t.Fatalf("nil error changed the event: %+v", te)
	}
	applyResult(te, context.DeadlineExceeded)
	if te.Result != "error" || te.ErrorClass != "timeout" {
		t.Fatalf("want error/timeout, got %s/%s", te.Result, te.ErrorClass)
	}

	locked := &telemetry.Event{Result: "error", ErrorClass: "capture_lock"}
	applyResult(locked, errors.New("another capture is running"))
	if locked.ErrorClass != "capture_lock" {
		t.Fatalf("the command's own class must win, got %s", locked.ErrorClass)
	}
}

// finishCommand closes the event once the result is applied (Finish is
// what stamps the duration), and a result the command set to something
// other than ok or error is kept as-is.
func TestFinishCommandClosesTheEvent(t *testing.T) {
	t.Setenv("KAI_TELEMETRY", "0")      // nothing is sent; Finish still stamps the event
	finishCommand(nil, errors.New("x")) // telemetry off: nothing to close

	te := &telemetry.Event{Result: "ok"}
	finishCommand(te, errors.New("boom"))
	if te.Result != "error" || te.ErrorClass != "other" {
		t.Fatalf("want error/other, got %s/%s", te.Result, te.ErrorClass)
	}
	if te.DurMs == 0 {
		t.Fatal("Finish was not called: the duration is unset")
	}

	partial := &telemetry.Event{Result: "partial"}
	applyResult(partial, errors.New("x"))
	if partial.Result != "partial" || partial.ErrorClass != "other" {
		t.Fatalf("a result the command set must be kept, got %s/%s", partial.Result, partial.ErrorClass)
	}
}
