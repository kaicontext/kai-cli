package main

import (
	"strings"
	"testing"
)

// The symbol seed is what stops the reviewer from spending its budget
// searching for names its own input contains — verify the extractor reads a
// unified diff the way the dogfood failure needed it to.
func TestRcChangedSymbols(t *testing.T) {
	diff := `diff --git a/internal/api/usage_alerts.go b/internal/api/usage_alerts.go
+++ b/internal/api/usage_alerts.go
@@ -0,0 +1,3 @@
+func highestCrossedThreshold(beforeCents, afterCents, capCents int) int {
+func (h *Handler) maybeSendUsageAlert(userID string, beforeCents int) {
+	notAdecl := 1
diff --git a/frontend/src/App.tsx b/frontend/src/App.tsx
+++ b/frontend/src/App.tsx
+function renderThing(props) {
+class UsagePanel extends Component {
--- a/old.go
-func removedOnly() {}
`
	out := rcChangedSymbols(diff)
	for _, want := range []string{
		"internal/api/usage_alerts.go: highestCrossedThreshold",
		"internal/api/usage_alerts.go: maybeSendUsageAlert", // receiver skipped
		"frontend/src/App.tsx: renderThing",
		"frontend/src/App.tsx: UsagePanel",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "removedOnly") {
		t.Errorf("removed-line symbol leaked in:\n%s", out)
	}
	if strings.Contains(out, "notAdecl") {
		t.Errorf("non-declaration line leaked in:\n%s", out)
	}
	if rcChangedSymbols("no diff markers here") != "" {
		t.Error("garbage input should yield no symbols")
	}
}
