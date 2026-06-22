package provider

import (
	"errors"
	"fmt"
	"testing"
)

// TestIsTransient_StreamErrorWrap confirms a stream-read error
// wrapped in transientError is classified as transient. Without
// this, the openai/kailab providers' "stream read: context
// deadline exceeded" failures bubble up to the agent runner as
// non-transient, the run dies, and the user has to retry the
// whole task manually. The 2026-05-14 quality-nits-fix dogfood
// killed the run on exactly this — 25+ turns of work lost.
func TestIsTransient_StreamErrorWrap(t *testing.T) {
	underlying := fmt.Errorf("openai provider: stream read: context deadline exceeded")
	wrapped := &transientError{cause: underlying}

	if !IsTransient(wrapped) {
		t.Errorf("expected stream-read error wrapped in transientError to classify as transient")
	}
	// Unwrap should still expose the underlying error for inspection.
	if !errors.Is(wrapped, underlying) {
		// errors.Is checks Unwrap chain; transientError.Unwrap exposes cause.
		// (Sanity check that the wrapper hasn't broken unwrap-chasing.)
		t.Errorf("expected errors.Is to find the underlying error through the wrap")
	}
}

// TestIsTransient_PlainErrorNotTransient confirms non-wrapped
// errors are NOT misclassified as transient. Auth/billing/bad-
// request errors must surface to the user, not get retried.
func TestIsTransient_PlainErrorNotTransient(t *testing.T) {
	plain := fmt.Errorf("openai provider: 401 unauthorized")
	if IsTransient(plain) {
		t.Errorf("expected unwrapped error NOT to classify as transient")
	}
}
