package parse

import (
	"testing"
)

func TestParser_ParseFunction(t *testing.T) {
	parser := NewParser()

	code := []byte(`
function hello(name) {
  return "Hello, " + name;
}
`)

	parsed, err := parser.Parse(code, "js")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	if len(parsed.Symbols) == 0 {
		t.Fatal("Expected at least one symbol")
	}

	found := false
	for _, sym := range parsed.Symbols {
		if sym.Name == "hello" && sym.Kind == "function" {
			found = true
			break
		}
	}

	if !found {
		t.Error("Expected to find function 'hello'")
	}
}

func TestParser_ParseClass(t *testing.T) {
	parser := NewParser()

	code := []byte(`
class User {
  constructor(name) {
    this.name = name;
  }

  greet() {
    return "Hello, " + this.name;
  }
}
`)

	parsed, err := parser.Parse(code, "js")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	foundClass := false
	foundMethod := false
	for _, sym := range parsed.Symbols {
		if sym.Name == "User" && sym.Kind == "class" {
			foundClass = true
		}
		if sym.Name == "User.greet" && sym.Kind == "function" {
			foundMethod = true
		}
	}

	if !foundClass {
		t.Error("Expected to find class 'User'")
	}

	if !foundMethod {
		t.Error("Expected to find method 'User.greet'")
	}
}

func TestParser_ParseVariables(t *testing.T) {
	parser := NewParser()

	code := []byte(`
const MAX_SIZE = 100;
let count = 0;
var name = "test";
`)

	parsed, err := parser.Parse(code, "js")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	expected := map[string]bool{
		"MAX_SIZE": false,
		"count":    false,
		"name":     false,
	}

	for _, sym := range parsed.Symbols {
		if _, ok := expected[sym.Name]; ok {
			expected[sym.Name] = true
		}
	}

	for name, found := range expected {
		if !found {
			t.Errorf("Expected to find variable '%s'", name)
		}
	}
}

func TestParser_ParseArrowFunction(t *testing.T) {
	parser := NewParser()

	code := []byte(`
const add = (a, b) => a + b;
const multiply = (a, b) => {
  return a * b;
};
`)

	parsed, err := parser.Parse(code, "js")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	foundAdd := false
	foundMultiply := false
	for _, sym := range parsed.Symbols {
		if sym.Name == "add" {
			foundAdd = true
		}
		if sym.Name == "multiply" {
			foundMultiply = true
		}
	}

	if !foundAdd {
		t.Error("Expected to find arrow function 'add'")
	}

	if !foundMultiply {
		t.Error("Expected to find arrow function 'multiply'")
	}
}

func TestRangesOverlap(t *testing.T) {
	tests := []struct {
		name     string
		r1       Range
		r2       Range
		expected bool
	}{
		{
			name:     "Same range",
			r1:       Range{Start: [2]int{1, 0}, End: [2]int{5, 10}},
			r2:       Range{Start: [2]int{1, 0}, End: [2]int{5, 10}},
			expected: true,
		},
		{
			name:     "r1 contains r2",
			r1:       Range{Start: [2]int{0, 0}, End: [2]int{10, 0}},
			r2:       Range{Start: [2]int{2, 0}, End: [2]int{5, 0}},
			expected: true,
		},
		{
			name:     "No overlap - r1 before r2",
			r1:       Range{Start: [2]int{0, 0}, End: [2]int{5, 0}},
			r2:       Range{Start: [2]int{6, 0}, End: [2]int{10, 0}},
			expected: false,
		},
		{
			name:     "No overlap - r2 before r1",
			r1:       Range{Start: [2]int{6, 0}, End: [2]int{10, 0}},
			r2:       Range{Start: [2]int{0, 0}, End: [2]int{5, 0}},
			expected: false,
		},
		{
			name:     "Partial overlap",
			r1:       Range{Start: [2]int{0, 0}, End: [2]int{5, 0}},
			r2:       Range{Start: [2]int{3, 0}, End: [2]int{8, 0}},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := RangesOverlap(tt.r1, tt.r2)
			if result != tt.expected {
				t.Errorf("RangesOverlap(%v, %v) = %v, expected %v", tt.r1, tt.r2, result, tt.expected)
			}
		})
	}
}
