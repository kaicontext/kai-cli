package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kai/internal/ai"
)

// RecorderMode controls how Recorder behaves on each Complete call.
//
//   - ModeReplay (default): load a saved fixture; error if missing.
//     Tests are deterministic and offline. CI runs in this mode.
//   - ModeRecord: call the wrapped Completer (a real Kailab/Anthropic
//     client) and overwrite the fixture with whatever comes back.
//     Costs real tokens; only run when prompts/behavior change.
//   - ModeMixed: load from fixture if present, else record. Useful
//     during local development when you want to add a new test
//     without re-recording everything else.
//
// Mode is selected by the KAI_LLM_FIXTURES env var; default is
// replay so accidental token spend is hard.
type RecorderMode string

const (
	ModeReplay RecorderMode = "replay"
	ModeRecord RecorderMode = "record"
	ModeMixed  RecorderMode = "mixed"

	// envFixtureMode is the env var that picks the mode.
	envFixtureMode = "KAI_LLM_FIXTURES"

	// fixtureSchema is bumped when the on-disk format changes
	// incompatibly. Loader rejects unknown schemas to prevent
	// silent corruption.
	fixtureSchema = "kai-llm-fixture-v1"
)

// Recorder is a Completer that either replays a saved response or
// captures a fresh one. Wrap your real provider in it for tests:
//
//	rec := planner.NewRecorder(real, "testdata/fixtures/planner", "TestPlan_X")
//	resp, err := rec.Complete(system, msgs, maxTokens)
//
// In record mode the real provider does the work; in replay mode the
// wrapped provider can be nil. See package doc for the full contract.
type Recorder struct {
	// Wrapped is the real provider used in record mode. Nil is
	// allowed in replay mode (and is encouraged for tests that should
	// never make real API calls regardless of env settings).
	Wrapped Completer
	// FixtureDir is the directory fixtures live in; created on demand
	// when recording. Tests typically point this at testdata/fixtures.
	FixtureDir string
	// Label is a human-readable prefix for the fixture filename so
	// developers can spot which test owns which file. Use the test
	// function name (`t.Name()` works).
	Label string
	// Mode overrides the env-var default. Tests that want to assert
	// a specific mode (e.g. always-replay) set this directly.
	Mode RecorderMode
}

// NewRecorder constructs a Recorder. Mode comes from KAI_LLM_FIXTURES
// unless explicitly set on the returned struct afterward.
func NewRecorder(wrapped Completer, fixtureDir, label string) *Recorder {
	return &Recorder{
		Wrapped:    wrapped,
		FixtureDir: fixtureDir,
		Label:      label,
	}
}

// Complete satisfies the Completer interface. Looks up or records a
// fixture per Mode rules.
func (r *Recorder) Complete(system string, messages []ai.Message, maxTokens int) (string, error) {
	mode := r.resolveMode()

	hash, err := canonicalHash(system, messages, maxTokens)
	if err != nil {
		return "", err
	}
	path := r.fixturePath(hash)

	if mode == ModeReplay || mode == ModeMixed {
		fx, err := loadFixture(path)
		switch {
		case err == nil && fx.matchesRequest(system, messages, maxTokens):
			return fx.Response, nil
		case errors.Is(err, errFixtureMissing):
			if mode == ModeReplay {
				return "", fmt.Errorf("planner recorder: no fixture at %s\n\trun again with %s=record to capture one (requires `kai auth login`)",
					path, envFixtureMode)
			}
			// fall through to record
		case err != nil:
			return "", fmt.Errorf("planner recorder: loading fixture %s: %w", path, err)
		default:
			// fixture exists but request doesn't match — drift.
			// Bail loudly so the developer knows their prompt changed.
			if mode == ModeReplay {
				return "", fmt.Errorf("planner recorder: fixture %s exists but request differs from what was recorded — re-record with %s=record after auditing the prompt change",
					path, envFixtureMode)
			}
			// In mixed mode, drift means re-record.
		}
	}

	if r.Wrapped == nil {
		return "", fmt.Errorf("planner recorder: cannot %s without a wrapped Completer", mode)
	}

	resp, err := r.Wrapped.Complete(system, messages, maxTokens)
	if err != nil {
		return "", err
	}

	if mode == ModeRecord || mode == ModeMixed {
		if saveErr := saveFixture(path, r.Label, system, messages, maxTokens, resp); saveErr != nil {
			// We have a real response; saving is best-effort. Log
			// but don't fail — the test can still proceed against
			// the in-memory response.
			fmt.Fprintf(os.Stderr, "warning: planner recorder: saving fixture: %v\n", saveErr)
		}
	}
	return resp, nil
}

// resolveMode picks the active mode: explicit struct override wins,
// otherwise read the env var, otherwise default to replay.
func (r *Recorder) resolveMode() RecorderMode {
	if r.Mode != "" {
		return r.Mode
	}
	switch RecorderMode(os.Getenv(envFixtureMode)) {
	case ModeRecord:
		return ModeRecord
	case ModeMixed:
		return ModeMixed
	default:
		return ModeReplay
	}
}

// fixturePath builds the on-disk filename. Truncated hash keeps the
// name readable; full hash is stored inside the fixture and verified
// on load to catch the (astronomical) chance of a 12-char collision
// between two distinct labels.
func (r *Recorder) fixturePath(hash string) string {
	short := hash
	if len(short) > 12 {
		short = short[:12]
	}
	name := fmt.Sprintf("%s_%s.json", sanitizeLabel(r.Label), short)
	return filepath.Join(r.FixtureDir, name)
}

// sanitizeLabel turns a Go test name (which can contain `/`, spaces,
// commas, etc.) into a filename-safe slug. Doesn't try to be
// reversible — only the prefix's job is human readability.
func sanitizeLabel(s string) string {
	if s == "" {
		return "fixture"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// fixture is the on-disk record format. Schema is versioned so we
// can evolve fixture shape without silently corrupting old files.
type fixture struct {
	Schema     string         `json:"schema"`
	RecordedAt string         `json:"recorded_at"`
	Label      string         `json:"label,omitempty"`
	Hash       string         `json:"hash"`
	Request    fixtureRequest `json:"request"`
	Response   string         `json:"response"`
}

// fixtureRequest mirrors the inputs to Complete. Stored verbatim so
// developers can read fixtures and see what was asked.
type fixtureRequest struct {
	System    string       `json:"system"`
	Messages  []ai.Message `json:"messages"`
	MaxTokens int          `json:"max_tokens"`
}

// matchesRequest re-canonicalizes the loaded request and compares
// against the live request. Defense against the rare hash-collision
// case AND against fixture files edited by hand.
func (f *fixture) matchesRequest(system string, msgs []ai.Message, maxTokens int) bool {
	want, err := canonicalHash(system, msgs, maxTokens)
	if err != nil {
		return false
	}
	return f.Hash == want
}

// canonicalHash returns a stable sha256-hex of the request payload.
// Stable across Go runtime details (json marshals struct fields in
// definition order, not random map order). Sensitive to anything
// that semantically affects the LLM call:
//
//   - system prompt text
//   - message role + content (verbatim)
//   - max_tokens
//
// Intentionally NOT in the hash:
//
//   - model: server may pick a different one post-record (e.g.
//     fallback). Captured in fixture metadata for diagnostics.
//   - timestamps, run id, anything that drifts run-to-run.
//
// If you change agentprompt.Build or the planner's system-prompt
// composition, this hash changes and fixtures miss. That's correct —
// re-record after auditing the prompt change.
func canonicalHash(system string, messages []ai.Message, maxTokens int) (string, error) {
	body, err := json.Marshal(struct {
		System    string       `json:"system"`
		Messages  []ai.Message `json:"messages"`
		MaxTokens int          `json:"max_tokens"`
	}{System: system, Messages: messages, MaxTokens: maxTokens})
	if err != nil {
		return "", fmt.Errorf("canonicalHash: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// errFixtureMissing distinguishes "no file" from "couldn't parse" —
// caller branches on it for the missing-fixture-in-replay-mode error.
var errFixtureMissing = errors.New("fixture not found")

// loadFixture reads, parses, and schema-checks a fixture file.
// Returns errFixtureMissing if the file doesn't exist; any other
// error indicates the file exists but is unusable (which we surface
// rather than silently re-recording).
func loadFixture(path string) (*fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errFixtureMissing
		}
		return nil, err
	}
	var fx fixture
	if err := json.Unmarshal(data, &fx); err != nil {
		return nil, fmt.Errorf("parsing fixture: %w", err)
	}
	if fx.Schema != fixtureSchema {
		return nil, fmt.Errorf("unknown fixture schema %q (expected %s)", fx.Schema, fixtureSchema)
	}
	return &fx, nil
}

// saveFixture writes the fixture atomically (temp + rename) so a
// crashed test run doesn't leave a half-written file.
func saveFixture(path, label, system string, messages []ai.Message, maxTokens int, response string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	hash, err := canonicalHash(system, messages, maxTokens)
	if err != nil {
		return err
	}
	fx := fixture{
		Schema:     fixtureSchema,
		RecordedAt: time.Now().UTC().Format(time.RFC3339),
		Label:      label,
		Hash:       hash,
		Request: fixtureRequest{
			System:    system,
			Messages:  messages,
			MaxTokens: maxTokens,
		},
		Response: response,
	}
	body, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
