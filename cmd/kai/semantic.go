package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kaicontext/kai-engine/contract"
	"github.com/kaicontext/kai-engine/verify"
)

const maxSemanticDiffBytes = 60 * 1024

// kitBinary returns the path to the kit binary (the graph-grounded verifier),
// or "" if it isn't on PATH.
func kitBinary() string {
	if p, err := exec.LookPath("kit"); err == nil {
		return p
	}
	return ""
}

// semanticVerify runs the graph-grounded verifier (`kit verify`) for one
// contract, streams its investigation to the user, parses the verdict, combines
// it with the deterministic state, and persists it to the store. This is the
// single semantic path — used by both `kai verify` (manual) and the `kai watch`
// heuristic. It is the only thing that can set `verified`.
func semanticVerify(ctx context.Context, kit string, store *contract.Store, c *contract.Contract, structural contract.CheckResult) error {
	cmd := exec.CommandContext(ctx, kit, "verify", c.Statement)
	cmd.Stderr = os.Stderr // stream kit's graph queries live to the user
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kit verify: %w", err)
	}

	sem, residueQs := verify.ParseSemantic(out.String())
	sem.RanAt = time.Now().UnixMilli()
	c.Semantic = sem
	c.Residue = nil
	for _, q := range residueQs {
		c.Residue = append(c.Residue, contract.ResidueItem{
			Contract: c.ID, Prompt: q, Origin: contract.ResidueSemanticUnconfirmed,
		})
	}
	c.Status = verify.SemanticVerdict(structural, sem)
	if err := store.Save(c); err != nil {
		return err
	}

	note := c.Semantic.Note
	if note != "" {
		note = " — " + note
	}
	fmt.Printf("  %s %s%s\n", c.Status.Glyph(), c.Status.Display(), note)
	for _, r := range c.Residue {
		fmt.Printf("    ? %s\n", r.Prompt)
	}
	return nil
}

// workingTreeDiff returns the uncommitted diff vs HEAD (tracked changes),
// capped. Empty if not a git repo or nothing changed.
func workingTreeDiff(maxBytes int) string {
	out, err := exec.Command("git", "--no-pager", "diff", "HEAD").Output()
	if err != nil || len(out) == 0 {
		out, _ = exec.Command("git", "--no-pager", "diff").Output()
	}
	if len(out) > maxBytes {
		return string(out[:maxBytes]) + "\n... (diff truncated)\n"
	}
	return string(out)
}

// changedLines returns the added/removed content lines of a unified diff
// (excluding the +++/--- file headers). Used to measure material change
// deterministically — no LLM.
func changedLines(diff string) []string {
	var out []string
	for _, l := range strings.Split(diff, "\n") {
		if strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "---") {
			continue
		}
		if strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") {
			out = append(out, l)
		}
	}
	return out
}
