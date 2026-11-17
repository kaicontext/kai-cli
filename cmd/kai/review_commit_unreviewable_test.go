package main

import (
	"reflect"
	"strings"
	"testing"
)

const rcTestLockDiff = `diff --git a/package-lock.json b/package-lock.json
index 1..2 100644
--- a/package-lock.json
+++ b/package-lock.json
@@ -10,9 +10,12 @@
     "node_modules/electron": {
-      "version": "41.0.0",
-      "resolved": "https://registry.npmjs.org/electron/-/electron-41.0.0.tgz",
-      "integrity": "sha512-old",
+      "version": "42.11.17",
+      "resolved": "https://registry.npmjs.org/electron/-/electron-42.11.17.tgz",
+      "integrity": "sha512-new",
+      "engines": {
+        "node": ">=22.12.0"
+      },
     },
     "node_modules/left-pad": {
-      "resolved": "https://registry.npmjs.org/left-pad/-/left-pad-1.3.0.tgz",
+      "resolved": "https://evil.example.com/left-pad-1.3.0.tgz",
     },
diff --git a/src/app.ts b/src/app.ts
index 3..4 100644
--- a/src/app.ts
+++ b/src/app.ts
@@ -1 +1 @@
-const a = 1
+const a = 2
diff --git a/api/v1/svc.pb.go b/api/v1/svc.pb.go
--- a/api/v1/svc.pb.go
+++ b/api/v1/svc.pb.go
@@ -1 +1,2 @@
+// generated
+package v1
`

func TestDropUnreviewableSummarisesLockfileKeepsCodeOmitsGenerated(t *testing.T) {
	got := rcDropUnreviewable(rcTestLockDiff)
	for _, want := range []string{
		"(lockfile: 11 changed line(s); only package versions, engine requirements and non-registry sources are shown)\n",
		`     "node_modules/electron": {` + "\n" + `-      "version": "41.0.0",` + "\n" + `+      "version": "42.11.17",` + "\n",
		`+      "engines": {` + "\n" + `+        "node": ">=22.12.0"` + "\n",
		`     "node_modules/left-pad": {` + "\n" + `+      "resolved": "https://evil.example.com/left-pad-1.3.0.tgz",` + "\n",
		"diff --git a/src/app.ts b/src/app.ts\nindex 3..4 100644\n--- a/src/app.ts\n+++ b/src/app.ts\n@@ -1 +1 @@\n-const a = 1\n+const a = 2\n",
		"diff --git a/api/v1/svc.pb.go b/api/v1/svc.pb.go\n(vendored, generated, minified or binary file: 2 changed line(s), not shown)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, gone := range []string{"integrity", "registry.npmjs.org", "+package v1"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q should not be shown:\n%s", gone, got)
		}
	}
}

func TestLockSummaryIsCapped(t *testing.T) {
	var b strings.Builder
	b.WriteString("diff --git a/Cargo.lock b/Cargo.lock\n--- a/Cargo.lock\n+++ b/Cargo.lock\n@@ -1,400 +1,400 @@\n")
	for i := 0; i < 200; i++ {
		b.WriteString(" name = \"crate\"\n-version = \"1.0.0\"\n+version = \"1.0.1\"\n")
	}
	got := rcDropUnreviewable(b.String())
	if !strings.Contains(got, "... (450 more lockfile line(s) not shown)\n") {
		t.Errorf("summary not capped:\n%s", got[len(got)-300:])
	}
}

func TestDropUnreviewableVendorOnlyOnABump(t *testing.T) {
	vendorEdit := "diff --git a/vendor/lib/x.go b/vendor/lib/x.go\n--- a/vendor/lib/x.go\n+++ b/vendor/lib/x.go\n@@ -1 +1 @@\n-return nil\n+return err\n"
	if got := rcDropUnreviewable(vendorEdit); got != vendorEdit {
		t.Errorf("a hand edit to vendored code must be reviewed in full:\n%s", got)
	}
	bump := "diff --git a/go.mod b/go.mod\n--- a/go.mod\n+++ b/go.mod\n@@ -1 +1 @@\n-require lib v1.0.0\n+require lib v1.1.0\n" + vendorEdit
	got := rcDropUnreviewable(bump)
	if strings.Contains(got, "+return err") || !strings.Contains(got, "+require lib v1.1.0") {
		t.Errorf("a re-vendor must keep go.mod and drop vendor/:\n%s", got)
	}
}

func TestDropUnreviewableLeavesCodeOnlyDiffUnchanged(t *testing.T) {
	d := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n"
	if got := rcDropUnreviewable(d); got != d {
		t.Errorf("code-only diff changed:\n%s", got)
	}
	if got := rcDropUnreviewable(""); got != "" {
		t.Errorf("empty diff: got %q", got)
	}
}

func TestFileKindOf(t *testing.T) {
	for _, c := range []struct {
		path string
		bump bool
		want rcFileKind
	}{
		{"package-lock.json", false, rcFileLockSummary},
		{"web/yarn.lock", false, rcFileLockSummary},
		{"go.sum", false, rcFileLockSummary},
		{"Cargo.lock", false, rcFileLockSummary},
		{"bun.lockb", false, rcFileOmit},
		{"app/node_modules/a/index.js", false, rcFileOmit},
		{"static/app.min.js", false, rcFileOmit},
		{"api/v1/service.pb.go", false, rcFileOmit},
		{"proto/svc_pb2_grpc.py", false, rcFileOmit},
		{"assets/logo.png", false, rcFileOmit},
		{"vendor/github.com/x/y.go", false, rcFileReview},
		{"vendor/github.com/x/y.go", true, rcFileOmit},
		{"src/__snapshots__/a.test.ts.snap", false, rcFileReview},
		{"go.mod", false, rcFileReview},
		{"package.json", false, rcFileReview},
		{"Dockerfile", false, rcFileReview},
		{"deploy/k8s/app.yaml", false, rcFileReview},
		{"build/deploy.sh", false, rcFileReview},
		{"src/vendors.ts", false, rcFileReview},
		{"internal/lockfile.go", false, rcFileReview},
	} {
		if got := rcFileKindOf(c.path, c.bump); got != c.want {
			t.Errorf("rcFileKindOf(%q, bump=%v) = %v, want %v", c.path, c.bump, got, c.want)
		}
	}
}

func TestReviewablePaths(t *testing.T) {
	if rcReviewablePaths(nil, []string{"a.go"}) != nil {
		t.Error("nil scope must stay nil")
	}
	changed := []string{"go.sum", "main.go", "vendor/a/b.go", "go.mod", "x.pb.go"}
	got := rcReviewablePaths(changed, changed)
	if want := []string{"main.go", "go.mod"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := rcReviewablePaths([]string{"vendor/a/b.go"}, []string{"vendor/a/b.go"}); len(got) != 1 {
		t.Errorf("hand-edited vendor file must stay in scope, got %v", got)
	}
}
