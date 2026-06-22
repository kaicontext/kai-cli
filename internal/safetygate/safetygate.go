// Package safetygate classifies an integration's blast radius and decides
// whether it should auto-promote, queue for review, or be blocked from
// becoming team-visible.
//
// The gate is a pure read of the semantic graph plus a per-repo config.
// It never mutates state. Callers consult the verdict before invoking the
// publish primitive (refMgr.Set on a non-ws.* ref).
package safetygate

import (
	"context"
	"fmt"
	"path"
	"sort"

	"kai/internal/graph"
)

// Grapher is the subset of *graph.DB the gate needs. Defined as an
// interface so unit tests can substitute an in-memory fake without
// standing up SQLite. *graph.DB satisfies it directly.
type Grapher interface {
	GetEdgesToByPath(filePath string, edgeType graph.EdgeType) ([]*graph.Edge, error)
	GetNode(id []byte) (*graph.Node, error)
}

// Verdict is the gate's decision on a single integration.
type Verdict string

const (
	// Auto: blast radius is within the auto threshold. Caller may publish.
	Auto Verdict = "auto"
	// Review: blast radius exceeds auto threshold. Caller must hold for review.
	Review Verdict = "review"
	// Block: change touches a protected path or exceeds the block threshold.
	// Caller must hold; only an explicit human approval can promote.
	Block Verdict = "block"
)

// Decision is the gate's output. All fields are safe to persist on the
// snapshot payload as gateVerdict / gateReasons / gateBlastRadius / gateTouches.
type Decision struct {
	Verdict     Verdict  `json:"verdict"`
	BlastRadius int      `json:"blastRadius"`
	Reasons     []string `json:"reasons,omitempty"`
	Touches     []string `json:"touches,omitempty"`
}

// Config drives the classifier. Loaded from .kai/gate.yaml; see config.go.
type Config struct {
	// AutoThreshold: blast radius <= this → Auto.
	AutoThreshold int `yaml:"auto_threshold"`
	// BlockThreshold: blast radius >= this → Block.
	BlockThreshold int `yaml:"block_threshold"`
	// Protected: glob patterns; any change matching one of these → Block.
	Protected []string `yaml:"protected"`

	// SnapshotID, when set, scopes blast-radius edge queries to that
	// snapshot (only importers/callers that are files in the snapshot
	// count) — see graph.GetEdgesToByPathScoped. Empty = unscoped (the
	// historical behavior). Runtime-only; never read from the config
	// file. The integrate/gate path sets it to the snapshot being gated
	// so a leaf change isn't inflated by stale cross-snapshot edges.
	SnapshotID []byte `yaml:"-"`
}

// scopedGrapher is the optional snapshot-scoped extension of Grapher.
// blastRadius uses it (via type assertion) when a SnapshotID is set, so
// the Grapher interface itself stays minimal and existing fakes don't
// need to implement the scoped method.
type scopedGrapher interface {
	GetEdgesToByPathScoped(filePath string, edgeType graph.EdgeType, snapshotID []byte) ([]*graph.Edge, error)
}

// DefaultConfig is used when no gate.yaml is present. Strict on the auto side
// (only zero-blast changes auto-promote), permissive on the block side
// (nothing is blocked unless explicitly listed in Protected).
func DefaultConfig() Config {
	return Config{
		AutoThreshold:  0,
		BlockThreshold: 1 << 30,
		Protected:      nil,
	}
}

// Classify computes the verdict for a set of paths the workspace modified
// (relative to its base snapshot). It walks the graph at depth 1 to count
// callers and importers — the same primitive `kai impact` uses.
//
// The graph traversal uses `GetEdgesToByPath` (inbound IMPORTS/CALLS), which
// answers "who is affected by changing this file." That is exactly what
// blast radius means; outbound dependencies (what we depend on) are not
// part of blast radius and are intentionally excluded.
func Classify(ctx context.Context, wsModified []string, g Grapher, cfg Config) (Decision, error) {
	if len(wsModified) == 0 {
		return Decision{Verdict: Auto, BlastRadius: 0}, nil
	}

	// Protected paths are checked first: a single match is sufficient to
	// block, regardless of radius.
	var protectedHits []string
	for _, p := range wsModified {
		for _, pat := range cfg.Protected {
			ok, err := path.Match(pat, p)
			if err != nil {
				return Decision{}, fmt.Errorf("invalid protected pattern %q: %w", pat, err)
			}
			if ok {
				protectedHits = append(protectedHits, p)
				break
			}
			// Also match a "**" glob form: stdlib path.Match has no recursive
			// wildcard, so we approximate by trimming trailing /** and
			// checking prefix.
			if matchDoubleStar(pat, p) {
				protectedHits = append(protectedHits, p)
				break
			}
		}
	}

	// Compute blast radius regardless — the developer wants to see it
	// even when blocked, since the protected reason is one input but the
	// radius is informative on its own.
	radius, touches, err := blastRadius(g, wsModified, cfg.SnapshotID)
	if err != nil {
		return Decision{}, err
	}

	d := Decision{
		BlastRadius: radius,
		Touches:     touches,
	}

	if len(protectedHits) > 0 {
		d.Verdict = Block
		sort.Strings(protectedHits)
		for _, h := range protectedHits {
			d.Reasons = append(d.Reasons, fmt.Sprintf("touches protected path: %s", h))
		}
		return d, nil
	}

	switch {
	case radius >= cfg.BlockThreshold:
		d.Verdict = Block
		d.Reasons = append(d.Reasons, fmt.Sprintf("blast radius %d ≥ block threshold %d", radius, cfg.BlockThreshold))
	case radius <= cfg.AutoThreshold:
		d.Verdict = Auto
	default:
		d.Verdict = Review
		d.Reasons = append(d.Reasons, fmt.Sprintf("blast radius %d > auto threshold %d", radius, cfg.AutoThreshold))
	}
	return d, nil
}

// blastRadius returns the count of unique paths affected at depth 1
// (callers + importers across all changed paths) and the sorted list of
// those paths for surfacing in `kai review`.
func blastRadius(g Grapher, changed []string, snapshotID []byte) (int, []string, error) {
	if g == nil {
		return 0, nil, nil
	}
	// When a snapshot is in scope and the grapher supports it, restrict
	// edges to that snapshot's files so stale cross-snapshot edges don't
	// inflate the radius (the edge-accumulation bug). Falls back to the
	// unscoped query otherwise.
	scoped, _ := g.(scopedGrapher)
	changedSet := make(map[string]bool, len(changed))
	for _, p := range changed {
		changedSet[p] = true
	}

	affected := make(map[string]bool)
	for _, p := range changed {
		for _, et := range []graph.EdgeType{graph.EdgeImports, graph.EdgeCalls} {
			var edges []*graph.Edge
			var err error
			if scoped != nil && len(snapshotID) > 0 {
				edges, err = scoped.GetEdgesToByPathScoped(p, et, snapshotID)
			} else {
				edges, err = g.GetEdgesToByPath(p, et)
			}
			if err != nil {
				return 0, nil, fmt.Errorf("querying %s edges into %s: %w", et, p, err)
			}
			for _, e := range edges {
				node, err := g.GetNode(e.Src)
				if err != nil || node == nil {
					continue
				}
				srcPath, _ := node.Payload["path"].(string)
				if srcPath == "" || changedSet[srcPath] {
					continue
				}
				affected[srcPath] = true
			}
		}
	}

	out := make([]string, 0, len(affected))
	for p := range affected {
		out = append(out, p)
	}
	sort.Strings(out)
	return len(out), out, nil
}

// matchDoubleStar handles glob patterns containing "**". Stdlib path.Match
// does not support recursive wildcards. For "a/b/**" we treat it as "any
// path under a/b/". For "**/x.go" we treat it as "any path ending in x.go".
// Anything more exotic is left to path.Match (called by the parent).
func matchDoubleStar(pattern, p string) bool {
	const ds = "/**"
	// prefix form: "a/b/**"
	if n := len(pattern); n >= len(ds) && pattern[n-len(ds):] == ds {
		prefix := pattern[:n-len(ds)] + "/"
		return len(p) >= len(prefix) && p[:len(prefix)] == prefix
	}
	// suffix form: "**/x.go"
	const sd = "**/"
	if len(pattern) >= len(sd) && pattern[:len(sd)] == sd {
		suffix := pattern[len(sd):]
		return len(p) >= len(suffix) && p[len(p)-len(suffix):] == suffix
	}
	return false
}
