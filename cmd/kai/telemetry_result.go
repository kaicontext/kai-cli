package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io/fs"
	"net"
	"os/exec"
	"regexp"
	"strings"

	"github.com/kaicontext/kai-engine/telemetry"
)

// finishCommand closes a command's telemetry event with the result the
// command actually returned. Every command used to `defer te.Finish()`,
// which reports "ok" whatever happened next — so init, push, fetch and
// pull have never once recorded a failure. A command that already set a
// more specific result or class (capture's capture_lock) keeps it.
func finishCommand(te *telemetry.Event, err error) {
	if te == nil {
		return
	}
	applyResult(te, err)
	te.Finish()
}

// applyResult is finishCommand without the send, so it can be tested
// against a bare event. It reads te's fields without the event's mutex,
// as the commands that set them do (capture's te.Result = "error"): the
// event is created, written and finished on the command's goroutine, and
// the engine exports the fields with no getter. Keep te on that goroutine.
func applyResult(te *telemetry.Event, err error) {
	if te == nil || err == nil {
		return
	}
	if te.Result == "" || te.Result == "ok" {
		te.SetResult("error")
	}
	if te.ErrorClass == "" {
		te.SetErrorClass(errorClassOf(err))
	}
}

// errorClassOf reduces an error to a coarse, bounded class for telemetry —
// a taxonomy id, never the message. Type checks first; the string matches
// at the end cover errors that reach us already flattened: sqlite busy,
// the "not initialized" and "not configured" refusals, and an HTTP status
// the message presents as one (httpStatusRe). A bare three-digit number is
// not a status: a port, a byte count or a file count must not land in
// auth or server.
func errorClassOf(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	// DNS before the timeout check: a lookup that timed out reports
	// Timeout() true, and dns is the bucket someone chasing resolution
	// trouble looks in.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	// Every way a certificate fails — unknown authority, wrong host, expired
	// or otherwise invalid — and the wrapper Go's TLS client puts around
	// them; before the net.OpError check, which can wrap a handshake. The
	// x509 targets are values because crypto/x509 returns them by value;
	// anything the TLS client itself returns is caught by the wrapper.
	var certVerifyErr *tls.CertificateVerificationError
	var authorityErr x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalidErr x509.CertificateInvalidError
	if errors.As(err, &certVerifyErr) || errors.As(err, &authorityErr) || errors.As(err, &hostErr) || errors.As(err, &invalidErr) {
		return "tls"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "network"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return "exit_status"
	}
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "permission"
	case errors.Is(err, fs.ErrNotExist):
		return "not_found"
	case errors.Is(err, exec.ErrNotFound):
		return "tool_missing"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "database is locked"), strings.Contains(msg, "sqlite_busy"):
		return "db_locked"
	case strings.Contains(msg, "not initialized"):
		return "no_project"
	case strings.Contains(msg, "not configured"):
		return "no_remote"
	}
	if m := httpStatusRe.FindStringSubmatch(msg); m != nil {
		switch code := m[1]; {
		case code == "401", code == "403":
			return "auth"
		case code == "402":
			return "no_credit"
		case code == "404":
			return "not_found"
		case code == "429":
			return "rate_limited"
		case code[0] == '4':
			return "http_4xx"
		case code[0] == '5':
			return "server"
		}
	}
	switch {
	case strings.Contains(msg, "unauthorized"):
		return "auth"
	case strings.Contains(msg, "rate limit"):
		return "rate_limited"
	}
	return "other"
}

// httpStatusRe finds a status code the message presents as one — "server
// error: 401 …", "HTTP 503: …", "status 502", "returned 429", the forms
// kai-engine/remote uses — and nothing else that has three digits in it.
var httpStatusRe = regexp.MustCompile(`(?:\bhttp\b|\bstatus\b|\breturned\b|server error)\s*:?\s*([1-5]\d\d)\b`)
