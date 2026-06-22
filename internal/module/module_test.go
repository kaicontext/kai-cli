package module

import (
	"testing"
)

func TestMatcher_MatchPath(t *testing.T) {
	modules := []ModuleRule{
		{Name: "Auth", Paths: []string{"auth/**"}},
		{Name: "Billing", Paths: []string{"billing/**"}},
		{Name: "Core", Paths: []string{"src/core/**", "lib/**"}},
	}

	matcher := NewMatcher(modules)

	tests := []struct {
		path     string
		expected []string
	}{
		{"auth/login.ts", []string{"Auth"}},
		{"auth/session/manager.ts", []string{"Auth"}},
		{"billing/invoice.ts", []string{"Billing"}},
		{"src/core/utils.ts", []string{"Core"}},
		{"lib/helpers.ts", []string{"Core"}},
		{"other/file.ts", nil},
		{"auth.ts", nil}, // Not inside auth/ directory
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := matcher.MatchPath(tt.path)

			if len(result) != len(tt.expected) {
				t.Errorf("MatchPath(%s) returned %v, expected %v", tt.path, result, tt.expected)
				return
			}

			for i, mod := range result {
				if mod != tt.expected[i] {
					t.Errorf("MatchPath(%s) returned %v, expected %v", tt.path, result, tt.expected)
					break
				}
			}
		})
	}
}

func TestMatcher_MatchPaths(t *testing.T) {
	modules := []ModuleRule{
		{Name: "Auth", Paths: []string{"auth/**"}},
		{Name: "Billing", Paths: []string{"billing/**"}},
	}

	matcher := NewMatcher(modules)

	paths := []string{
		"auth/login.ts",
		"auth/session.ts",
		"billing/invoice.ts",
		"other/file.ts",
	}

	result := matcher.MatchPaths(paths)

	if len(result["Auth"]) != 2 {
		t.Errorf("Expected 2 Auth files, got %d", len(result["Auth"]))
	}

	if len(result["Billing"]) != 1 {
		t.Errorf("Expected 1 Billing file, got %d", len(result["Billing"]))
	}

	if len(result["Other"]) != 0 {
		t.Errorf("Expected 0 Other files, got %d", len(result["Other"]))
	}
}

func TestMatcher_GetModulePayload(t *testing.T) {
	modules := []ModuleRule{
		{Name: "Auth", Paths: []string{"auth/**", "authentication/**"}},
	}

	matcher := NewMatcher(modules)

	payload := matcher.GetModulePayload("Auth")
	if payload == nil {
		t.Fatal("Expected payload, got nil")
	}

	if payload["name"] != "Auth" {
		t.Errorf("Expected name 'Auth', got %v", payload["name"])
	}

	patterns, ok := payload["patterns"].([]interface{})
	if !ok {
		t.Fatal("Expected patterns to be []interface{}")
	}

	if len(patterns) != 2 {
		t.Errorf("Expected 2 patterns, got %d", len(patterns))
	}

	// Non-existent module
	payload = matcher.GetModulePayload("NonExistent")
	if payload != nil {
		t.Errorf("Expected nil for non-existent module, got %v", payload)
	}
}

func TestMatcher_GetAllModules(t *testing.T) {
	modules := []ModuleRule{
		{Name: "Auth", Paths: []string{"auth/**"}},
		{Name: "Billing", Paths: []string{"billing/**"}},
	}

	matcher := NewMatcher(modules)

	all := matcher.GetAllModules()
	if len(all) != 2 {
		t.Errorf("Expected 2 modules, got %d", len(all))
	}
}
