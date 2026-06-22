package review

import (
	"strings"
	"testing"

	"kai/internal/diff"
)

func TestBuildReviewSummary_Basic(t *testing.T) {
	sd := &diff.SemanticDiff{
		Base: "abc123",
		Head: "def456",
		Files: []diff.FileDiff{
			{
				Path:   "api/handler.go",
				Action: diff.ActionModified,
				Units: []diff.UnitDiff{
					{
						Kind:      diff.UnitFunction,
						Name:      "HandleLogin",
						FQName:    "api.HandleLogin",
						Action:    diff.ActionModified,
						BeforeSig: "func(w, r)",
						AfterSig:  "func(ctx, w, r)",
					},
				},
			},
			{
				Path:   "internal/auth/auth.go",
				Action: diff.ActionAdded,
				Units: []diff.UnitDiff{
					{
						Kind:     diff.UnitFunction,
						Name:     "ValidateToken",
						FQName:   "auth.ValidateToken",
						Action:   diff.ActionAdded,
						AfterSig: "func(token string) bool",
					},
				},
			},
			{
				Path:   "internal/auth/auth_test.go",
				Action: diff.ActionAdded,
				Units: []diff.UnitDiff{
					{
						Kind:     diff.UnitFunction,
						Name:     "TestValidateToken",
						FQName:   "auth.TestValidateToken",
						Action:   diff.ActionAdded,
						AfterSig: "func(t *testing.T)",
					},
				},
			},
		},
	}

	summary := BuildReviewSummary(sd)

	if summary.TotalFiles != 3 {
		t.Errorf("expected 3 files, got %d", summary.TotalFiles)
	}

	if len(summary.Changes) == 0 {
		t.Fatal("expected at least one change group")
	}

	// Check that changes are grouped
	foundAPI := false
	foundInternal := false
	foundTest := false
	for _, c := range summary.Changes {
		for _, f := range c.Files {
			if strings.Contains(f, "api/") {
				foundAPI = true
			}
			if strings.Contains(f, "internal/") && !strings.Contains(f, "_test.go") {
				foundInternal = true
			}
			if strings.Contains(f, "_test.go") {
				foundTest = true
			}
		}
	}

	if !foundAPI {
		t.Error("expected to find API changes")
	}
	if !foundInternal {
		t.Error("expected to find internal changes")
	}
	if !foundTest {
		t.Error("expected to find test changes")
	}
}

func TestCategorizeFile(t *testing.T) {
	tests := []struct {
		path     string
		expected string
	}{
		{"api/handler.go", "api"},
		{"internal/service.go", "internal"},
		{"handler/users.go", "internal"}, // "handler/" without "/" prefix is not matched
		{"pkg/util_test.go", "test"},
		{"tests/integration.go", "internal"},        // tests/ without prefix slash is not matched
		{"src/tests/integration.go", "test"},        // /tests/ is matched
		{"tests/integration_test.go", "test"},      // _test.go is matched
		{"service_test.go", "test"},                // _test.go suffix
		{"component.spec.ts", "test"},              // .spec.ts suffix
		{"README.md", "docs"},
		{"docs/guide.txt", "docs"},
		{"config.yaml", "config"},
		{"go.mod", "config"},
		{"main.go", "internal"},
		{"src/controller/user.go", "api"},          // controller is matched
		{"/api/v1/endpoint.go", "api"},             // /api/ is matched
	}

	for _, tc := range tests {
		got := categorizeFile(tc.path)
		if got != tc.expected {
			t.Errorf("categorizeFile(%q) = %q, want %q", tc.path, got, tc.expected)
		}
	}
}

func TestFormatSummary(t *testing.T) {
	sd := &diff.SemanticDiff{
		Files: []diff.FileDiff{
			{
				Path:   "api/handler.go",
				Action: diff.ActionModified,
				Units: []diff.UnitDiff{
					{
						Kind:     diff.UnitFunction,
						Name:     "HandleLogin",
						Action:   diff.ActionModified,
						AfterSig: "func(ctx, w, r)",
					},
				},
			},
		},
	}

	summary := BuildReviewSummary(sd)
	output := summary.FormatSummary()

	if !strings.Contains(output, "CHANGES") {
		t.Error("expected output to contain CHANGES section")
	}

	// Verify it's valid output (no panics, etc.)
	if len(output) == 0 {
		t.Error("expected non-empty output")
	}
}

func TestFormatChange(t *testing.T) {
	sd := &diff.SemanticDiff{
		Files: []diff.FileDiff{
			{
				Path:   "api/handler.go",
				Action: diff.ActionModified,
				Units: []diff.UnitDiff{
					{
						Kind:     diff.UnitFunction,
						Name:     "HandleLogin",
						Action:   diff.ActionModified,
						AfterSig: "func(ctx, w, r)",
					},
				},
			},
		},
	}

	summary := BuildReviewSummary(sd)

	// Test valid index
	output := summary.FormatChange(0)
	if strings.Contains(output, "Invalid") {
		t.Error("expected valid output for index 0")
	}

	// Test invalid index
	output = summary.FormatChange(-1)
	if !strings.Contains(output, "Invalid") {
		t.Error("expected error for negative index")
	}

	output = summary.FormatChange(100)
	if !strings.Contains(output, "Invalid") {
		t.Error("expected error for out-of-bounds index")
	}
}

func TestIsAPISymbol(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"HandleLogin", true},    // Exported (uppercase)
		{"handleLogin", false},   // Unexported (lowercase)
		{"ValidateToken", true},  // Exported
		{"validateToken", false}, // Unexported
		{"", false},              // Empty
	}

	for _, tc := range tests {
		sym := SymbolChange{Name: tc.name}
		got := isAPISymbol(sym)
		if got != tc.expected {
			t.Errorf("isAPISymbol(%q) = %v, want %v", tc.name, got, tc.expected)
		}
	}
}
