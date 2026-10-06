package main

import (
	"reflect"
	"strings"
	"testing"
)

// Every stage prompt composes from the skill files, in the order the model
// should read them, and the shared definition reaches every stage that judges
// a defect.
func TestReviewSkillFilesCompose(t *testing.T) {
	defect := rcSkillFile("defect.md")
	for name, prompt := range map[string]string{"review": rcReviewSystem, "fast": rcFastReviewSystem, "challenge": rcChallengeSystemHead} {
		if !strings.Contains(prompt, defect) {
			t.Errorf("the %s prompt does not carry the shared definition of a defect", name)
		}
		if strings.Contains(prompt, rcSharedMarker) {
			t.Errorf("the %s prompt still carries the composition marker", name)
		}
	}
	order := func(prompt string, parts ...string) {
		t.Helper()
		last := -1
		for _, p := range parts {
			i := strings.Index(prompt, p)
			if i <= last {
				t.Errorf("%q is out of order (at %d, after %d)", p, i, last)
			}
			last = i
		}
	}
	order(rcReviewSystem, "# How to review this change", "# What counts as a defect", "# Defect catalog", "# Report format", rcReviewDataMarker)
	order(rcFastReviewSystem, "# Fast first pass", "# What counts as a defect", "# Defect catalog", "## Report format", rcReviewDataMarker)
	order(rcSweepSystem, "# Line-by-line sweep", "## Output", "- (none)")
	if strings.Contains(rcSweepSystem, "# Defect catalog") {
		t.Error("the sweep carries the catalog; it proposes from its own checklist")
	}
	order(rcChallengeSystemHead, "# Check a draft review before it is published", "# What counts as a defect", "## How to check")
	// The coda the parsers read stays where it was: last, and complete.
	for name, prompt := range map[string]string{"review": rcReviewSystem, "fast": rcFastReviewSystem} {
		coda := prompt[strings.Index(prompt, rcReviewDataMarker):]
		for _, field := range []string{"INTENT_MATCH:", "MERGE_READY:", "SUMMARY:", "ISSUES:", "DECISIONS:"} {
			if !strings.Contains(coda, field) {
				t.Errorf("the %s coda lost %s", name, field)
			}
		}
	}
}

// Shouting reads as a rule the model must overweight. The skill states rules
// in plain sentences; the only capitals are the coda's fields and the name of
// a block the pipeline adds to the prompt.
func TestReviewSkillDoesNotShout(t *testing.T) {
	allowed := map[string]bool{"INTENT_MATCH": true, "MERGE_READY": true, "SUMMARY": true, "ISSUES": true, "DECISIONS": true,
		"REVIEW-DATA": true, "AUTHOR": true, "CONTEXT": true, "INTENT": true, "DIFF": true, "HOSTS": true, "THIS": true, "CHANGE": true, "INTRODUCES": true,
		"URL": true, "SQL": true, "HTML": true, "API": true, "JSON": true, "XSS": true, "N+1": true, "ORM": true, "ACL": true, "JWT": true, "CSRF": true, "UPDATE": true, "WHERE": true, "ID": true}
	for _, f := range []string{"procedure.md", "defect.md", "catalog.md", "report.md", "fast.md", "sweep.md", "challenge.md"} {
		for _, w := range strings.FieldsFunc(rcSkillFile(f), func(r rune) bool {
			return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r == '_' || r == '-' || r == '+' || r >= '0' && r <= '9')
		}) {
			if len(w) >= 4 && strings.ToUpper(w) == w && strings.ToLower(w) != w && !allowed[strings.Trim(w, "-")] {
				t.Errorf("%s shouts %q", f, w)
			}
		}
	}
}

func TestPacksFollowWhatTheChangeTouches(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/api/views.py b/api/views.py",
		"+++ b/api/views.py",
		"+def refund(request, order_id):",
		"+    order = Order.objects.get(id=order_id)",
		"+    stripe.Refund.create(amount=order.amount)",
		"+    if request.user.is_authenticated and has_permission(request.user):",
		"+@login_required",
		"diff --git a/web/Button.tsx b/web/Button.tsx",
		"+++ b/web/Button.tsx",
		"+export const Button = () => <button />",
		"diff --git a/db/migrations/0042_add.sql b/db/migrations/0042_add.sql",
		"+++ b/db/migrations/0042_add.sql",
		"+ALTER TABLE orders ADD COLUMN refunded boolean NOT NULL;",
	}, "\n")
	got := rcPacksFor(diff)
	want := []string{"python", "typescript", "auth", "data", "money"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("packs = %v, want %v", got, want)
	}
	if got := rcPacksFor("diff --git a/README.md b/README.md\n+++ b/README.md\n+Hello\n"); len(got) != 0 {
		t.Errorf("a docs-only change got packs %v", got)
	}
	// One passing mention is not a change to that area.
	if got := rcPacksFor("diff --git a/x.go b/x.go\n+++ b/x.go\n+\tsession := store.Get(r)\n"); !reflect.DeepEqual(got, []string{"go"}) {
		t.Errorf("one mention of a session picked packs %v, want [go]", got)
	}
	// Workflows are found by path, shell scripts by extension.
	for _, p := range []string{".github/workflows/build.yml", "ci/.gitlab-ci.yml", "actions/setup/action.yaml", "scripts/release.sh"} {
		if got := rcPacksFor("diff --git a/" + p + " b/" + p + "\n+++ b/" + p + "\n+  run: echo hi\n"); !reflect.DeepEqual(got, []string{"ci"}) {
			t.Errorf("%s picked packs %v, want [ci]", p, got)
		}
	}
	if got := rcPacksFor("diff --git a/k8s/deploy.yml b/k8s/deploy.yml\n+++ b/k8s/deploy.yml\n+replicas: 2\n"); len(got) != 0 {
		t.Errorf("a plain YAML file picked packs %v", got)
	}
	// Every pack a change can pick exists, and the prompt carries its text.
	for lang := range rcLangPackExts {
		if !strings.Contains(rcReviewSystemFor([]string{lang}), rcSkillFile("packs/"+lang+".md")) {
			t.Errorf("pack %s does not reach the review prompt", lang)
		}
	}
	for _, r := range rcRiskPacks {
		if !strings.Contains(rcSweepSystemFor([]string{r.name}), rcSkillFile("packs/"+r.name+".md")) {
			t.Errorf("pack %s does not reach the sweep prompt", r.name)
		}
	}
}
