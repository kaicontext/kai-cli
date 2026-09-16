package errors

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Report sends the kind, the severity and the auto-repair flag, and
// nothing from the error itself: LogContext is err.Error() and Headline
// can be raw text, and either can hold a path, a URL or a message. The
// local errors.log is the other half of Report and still gets the raw
// text, which is where it is useful.
func TestReportSendsOnlyTheKind(t *testing.T) {
	type sent struct {
		kind, headline, raw, severity string
		repaired                      bool
		ctx                           map[string]any
	}
	var got []sent
	record := func(kind, headline, raw string, repaired bool, severity string, ctx map[string]any) {
		got = append(got, sent{kind, headline, raw, severity, repaired, ctx})
	}

	// A workspace with a .kai dir, so the local log has somewhere to go.
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, ".kai"), 0o755); err != nil {
		t.Fatal(err)
	}
	secret := "/Users/someone/acme-payroll/src/salaries.go"
	// Two classes: the fallback, whose headline is fixed text, and a
	// gate rule whose headline IS the error's first line — the kind of
	// class the raw text used to reach PostHog through.
	fallback := Classify(errors.New("open " + secret + ": permission denied"))
	fallback.Headline = "Couldn't read " + secret
	gate := Classify(errors.New("the change broke the build: " + secret + "\n" + secret + ":12:3: undefined: salary"))
	if gate.Kind != "gate.build_regression" || !strings.Contains(gate.Headline, secret) {
		t.Fatalf("the build-gate rule must put the first line in the headline, got %+v", gate)
	}
	for _, ue := range []UserError{fallback, gate} {
		got = nil
		report(ws, ue, false, record)
		if len(got) != 1 {
			t.Fatalf("%s: want one telemetry call, got %d", ue.Kind, len(got))
		}
		s := got[0]
		if s.kind != ue.Kind || s.severity != severityName(ue.Severity) || s.repaired {
			t.Errorf("kind/severity/repaired = %q/%q/%v, want %q/%q/false", s.kind, s.severity, s.repaired, ue.Kind, severityName(ue.Severity))
		}
		if s.headline != "" || s.raw != "" || s.ctx != nil {
			t.Errorf("%s: headline/raw/ctx must be empty, got %q/%q/%v", ue.Kind, s.headline, s.raw, s.ctx)
		}
		for _, v := range []string{s.kind, s.headline, s.raw, s.severity} {
			if strings.Contains(v, "acme") || strings.Contains(v, "salaries") {
				t.Fatalf("%s: the error's text reached telemetry: %+v", ue.Kind, s)
			}
		}
	}

	local, err := os.ReadFile(filepath.Join(ws, ".kai", "errors.log"))
	if err != nil {
		t.Fatalf("the local errors.log must still be written: %v", err)
	}
	if strings.Count(string(local), secret) < 2 || !strings.Contains(string(local), "gate.build_regression") || !strings.Contains(string(local), fallback.Kind) {
		t.Fatalf("the local log keeps the raw text and the kind of both errors, got: %s", local)
	}

	// Nothing to report for a nil or "none" classification.
	got = nil
	report("", UserError{Kind: "none"}, false, record)
	report("", UserError{}, false, record)
	if len(got) != 0 {
		t.Fatalf("a non-error must not be reported: %+v", got)
	}
}
