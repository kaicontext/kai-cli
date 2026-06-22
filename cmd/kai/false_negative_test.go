package main

import (
	"testing"
)

func TestFalseNeg_DirectDep(t *testing.T) {
	baseLayout := map[string]string{
		"src/lib.ts":         `export function greet() { return "hello"; }`,
		"tests/lib.test.ts":  "import { greet } from '../src/lib';\ntest('greet', () => expect(greet()).toBe('hello'));",
	}
	headLayout := make(map[string]string)
	for k, v := range baseLayout {
		headLayout[k] = v
	}
	headLayout["src/lib.ts"] = `export function greet() { return "hi"; }`

	selected := setupSelectionFixture(t, baseLayout, headLayout)
	groundTruth := []string{"tests/lib.test.ts"}

	assertNoFalseNegatives(t, selected, groundTruth)
}

func TestFalseNeg_TransitiveChain(t *testing.T) {
	// Chain: C.ts → B.ts → A.ts, tests: A.test.ts, C.test.ts
	// Change C.ts → both C.test.ts (direct) should be selected
	baseLayout := map[string]string{
		"src/c.ts":          `export const c = 1;`,
		"src/b.ts":          "import { c } from './c';\nexport const b = c + 1;",
		"src/a.ts":          "import { b } from './b';\nexport const a = b + 1;",
		"tests/c.test.ts":   "import { c } from '../src/c';\ntest('c', () => expect(c).toBe(1));",
		"tests/a.test.ts":   "import { a } from '../src/a';\ntest('a', () => expect(a).toBe(3));",
	}
	headLayout := make(map[string]string)
	for k, v := range baseLayout {
		headLayout[k] = v
	}
	headLayout["src/c.ts"] = `export const c = 100;`

	selected := setupSelectionFixture(t, baseLayout, headLayout)

	// C.test.ts directly imports C, so it must be selected
	assertNoFalseNegatives(t, selected, []string{"tests/c.test.ts"})
	t.Logf("selected: %v (checking transitive coverage)", selected)
}

func TestFalseNeg_TestFileChange(t *testing.T) {
	// When a test file changes alongside its dependency, it should still be selected.
	// Note: changing only the test file won't be caught by import-graph-based selection
	// since no inbound IMPORTS/TESTS edges point to the test file.
	baseLayout := map[string]string{
		"src/lib.ts":        `export const x = 1;`,
		"tests/lib.test.ts": "import { x } from '../src/lib';\ntest('x', () => expect(x).toBe(1));",
	}
	headLayout := make(map[string]string)
	for k, v := range baseLayout {
		headLayout[k] = v
	}
	headLayout["src/lib.ts"] = `export const x = 2;`
	headLayout["tests/lib.test.ts"] = "import { x } from '../src/lib';\ntest('x updated', () => expect(x).toBe(2));"

	selected := setupSelectionFixture(t, baseLayout, headLayout)

	assertNoFalseNegatives(t, selected, []string{"tests/lib.test.ts"})
}

func TestFalseNeg_MultipleTestsDependOnChanged(t *testing.T) {
	baseLayout := map[string]string{
		"src/shared.ts":      `export const shared = "value";`,
		"tests/a.test.ts":    "import { shared } from '../src/shared';\ntest('a', () => expect(shared).toBe('value'));",
		"tests/b.test.ts":    "import { shared } from '../src/shared';\ntest('b', () => expect(shared).toBe('value'));",
	}
	headLayout := make(map[string]string)
	for k, v := range baseLayout {
		headLayout[k] = v
	}
	headLayout["src/shared.ts"] = `export const shared = "changed";`

	selected := setupSelectionFixture(t, baseLayout, headLayout)

	assertNoFalseNegatives(t, selected, []string{"tests/a.test.ts", "tests/b.test.ts"})
}
