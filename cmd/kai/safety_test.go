package main

import (
	"os"
	"regexp"
	"testing"
)

// TestDetectStructuralRisks verifies that structural risk detection works correctly
func TestDetectStructuralRisks(t *testing.T) {
	tests := []struct {
		name          string
		changedFiles  []string
		affectedTests map[string]bool
		allTestFiles  []string
		modules       []string
		wantRisks     int
		wantTypes     []string
	}{
		{
			name:          "no changes, no risks",
			changedFiles:  []string{},
			affectedTests: map[string]bool{},
			allTestFiles:  []string{},
			modules:       []string{},
			wantRisks:     0,
			wantTypes:     nil,
		},
		{
			name:          "config file change triggers risk",
			changedFiles:  []string{"package.json"},
			affectedTests: map[string]bool{"tests/app.test.js": true}, // has test mapping to avoid no-test-mapping risk
			allTestFiles:  []string{"tests/app.test.js"},
			modules:       []string{},
			wantRisks:     1,
			wantTypes:     []string{RiskConfigChange},
		},
		{
			name:          "tsconfig change triggers risk",
			changedFiles:  []string{"tsconfig.json"},
			affectedTests: map[string]bool{"tests/app.test.ts": true}, // has test mapping
			allTestFiles:  []string{"tests/app.test.ts"},
			modules:       []string{},
			wantRisks:     1,
			wantTypes:     []string{RiskConfigChange},
		},
		{
			name:          "jest config change triggers risk",
			changedFiles:  []string{"jest.config.js"},
			affectedTests: map[string]bool{"tests/app.test.js": true}, // has test mapping
			allTestFiles:  []string{"tests/app.test.js"},
			modules:       []string{},
			wantRisks:     1,
			wantTypes:     []string{RiskConfigChange},
		},
		{
			name:          "test infrastructure change triggers risk",
			changedFiles:  []string{"tests/fixtures/sample.json"},
			affectedTests: map[string]bool{"tests/app.test.js": true}, // has test mapping
			allTestFiles:  []string{"tests/app.test.js"},
			modules:       []string{},
			wantRisks:     1,
			wantTypes:     []string{RiskTestInfra},
		},
		{
			name:          "mock file change triggers risk",
			changedFiles:  []string{"src/__mocks__/api.js"},
			affectedTests: map[string]bool{"tests/app.test.js": true}, // has test mapping
			allTestFiles:  []string{"tests/app.test.js"},
			modules:       []string{},
			wantRisks:     1,
			wantTypes:     []string{RiskTestInfra},
		},
		{
			name: "many files changed triggers risk",
			changedFiles: []string{
				"src/a.js", "src/b.js", "src/c.js", "src/d.js", "src/e.js",
				"src/f.js", "src/g.js", "src/h.js", "src/i.js", "src/j.js",
				"src/k.js", "src/l.js", "src/m.js", "src/n.js", "src/o.js",
				"src/p.js", "src/q.js", "src/r.js", "src/s.js", "src/t.js",
				"src/u.js",
			},
			affectedTests: map[string]bool{"tests/app.test.js": true},
			allTestFiles:  []string{"tests/app.test.js"},
			modules:       []string{},
			wantRisks:     1,
			wantTypes:     []string{RiskManyFilesChanged},
		},
		{
			name:          "cross-module change triggers risk",
			changedFiles:  []string{"src/app.js", "lib/utils.js", "pkg/helper.js"},
			affectedTests: map[string]bool{"tests/app.test.js": true},
			allTestFiles:  []string{"tests/app.test.js"},
			modules:       []string{"App", "Utils", "Helpers"},
			wantRisks:     1,
			wantTypes:     []string{RiskCrossModuleChange},
		},
		{
			name:          "normal code change no risk",
			changedFiles:  []string{"src/app.js"},
			affectedTests: map[string]bool{"tests/app.test.js": true},
			allTestFiles:  []string{"tests/app.test.js"},
			modules:       []string{"App"},
			wantRisks:     0,
			wantTypes:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			risks := detectStructuralRisks(tc.changedFiles, tc.affectedTests, tc.allTestFiles, tc.modules)

			if len(risks) != tc.wantRisks {
				t.Errorf("detectStructuralRisks() got %d risks, want %d", len(risks), tc.wantRisks)
				for _, r := range risks {
					t.Logf("  risk: %s - %s", r.Type, r.Description)
				}
			}

			// Check that expected risk types are present
			for _, wantType := range tc.wantTypes {
				found := false
				for _, r := range risks {
					if r.Type == wantType {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("detectStructuralRisks() missing expected risk type %q", wantType)
				}
			}
		})
	}
}

// TestCalculateConfidence verifies confidence scoring
func TestCalculateConfidence(t *testing.T) {
	tests := []struct {
		name         string
		risks        []StructuralRisk
		testsFound   int
		changedFiles int
		minConf      float64
		maxConf      float64
	}{
		{
			name:         "no changes = max confidence",
			risks:        nil,
			testsFound:   0,
			changedFiles: 0,
			minConf:      1.0,
			maxConf:      1.0,
		},
		{
			name:         "no risks = high confidence",
			risks:        nil,
			testsFound:   5,
			changedFiles: 3,
			minConf:      0.7,
			maxConf:      0.9,
		},
		{
			name: "high severity risk = lower confidence",
			risks: []StructuralRisk{
				{Type: RiskConfigChange, Severity: "high"},
			},
			testsFound:   5,
			changedFiles: 3,
			minConf:      0.5,
			maxConf:      0.7,
		},
		{
			name: "critical severity = much lower confidence",
			risks: []StructuralRisk{
				{Type: RiskConfigChange, Severity: "critical"},
			},
			testsFound:   5,
			changedFiles: 3,
			minConf:      0.4,
			maxConf:      0.6,
		},
		{
			name: "multiple high risks = low confidence",
			risks: []StructuralRisk{
				{Type: RiskConfigChange, Severity: "high"},
				{Type: RiskTestInfra, Severity: "high"},
			},
			testsFound:   5,
			changedFiles: 3,
			minConf:      0.3,
			maxConf:      0.5,
		},
		{
			name:         "no tests found = lower confidence",
			risks:        nil,
			testsFound:   0,
			changedFiles: 3,
			minConf:      0.4,
			maxConf:      0.6,
		},
		{
			name:         "many changes = lower confidence",
			risks:        nil,
			testsFound:   5,
			changedFiles: 25,
			minConf:      0.5,
			maxConf:      0.7,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conf := calculateConfidence(tc.risks, tc.testsFound, tc.changedFiles)

			if conf < tc.minConf || conf > tc.maxConf {
				t.Errorf("calculateConfidence() = %.2f, want between %.2f and %.2f",
					conf, tc.minConf, tc.maxConf)
			}
		})
	}
}

// TestShouldExpandForSafety verifies mode-specific expansion logic
func TestShouldExpandForSafety(t *testing.T) {
	highRisk := []StructuralRisk{
		{Type: RiskConfigChange, Severity: "high", Triggered: true, Description: "test risk"},
	}
	mediumRisk := []StructuralRisk{
		{Type: RiskManyFilesChanged, Severity: "medium", Triggered: false},
	}

	tests := []struct {
		name       string
		safetyMode string
		risks      []StructuralRisk
		confidence float64
		wantExpand bool
	}{
		// Shadow mode never expands
		{
			name:       "shadow mode never expands even with high risk",
			safetyMode: "shadow",
			risks:      highRisk,
			confidence: 0.5,
			wantExpand: false,
		},
		// Strict mode never expands
		{
			name:       "strict mode never expands even with high risk",
			safetyMode: "strict",
			risks:      highRisk,
			confidence: 0.5,
			wantExpand: false,
		},
		// Guarded mode expands on triggered high risk
		{
			name:       "guarded mode expands on triggered high risk",
			safetyMode: "guarded",
			risks:      highRisk,
			confidence: 0.5,
			wantExpand: true,
		},
		// Guarded mode doesn't expand on non-triggered risk
		{
			name:       "guarded mode doesn't expand on non-triggered medium risk",
			safetyMode: "guarded",
			risks:      mediumRisk,
			confidence: 0.6,
			wantExpand: false,
		},
		// Guarded mode expands on very low confidence
		{
			name:       "guarded mode expands on very low confidence",
			safetyMode: "guarded",
			risks:      nil,
			confidence: 0.2,
			wantExpand: true,
		},
		// Guarded mode doesn't expand on medium confidence
		{
			name:       "guarded mode doesn't expand on medium confidence",
			safetyMode: "guarded",
			risks:      nil,
			confidence: 0.5,
			wantExpand: false,
		},
	}

	// Use default policy for tests
	defaultPolicy := DefaultCIPolicy()

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			expand, _ := shouldExpandForSafety(tc.safetyMode, tc.risks, tc.confidence, defaultPolicy)

			if expand != tc.wantExpand {
				t.Errorf("shouldExpandForSafety(%q) = %v, want %v",
					tc.safetyMode, expand, tc.wantExpand)
			}
		})
	}
}

// TestCheckPanicSwitch verifies panic switch detection
func TestCheckPanicSwitch(t *testing.T) {
	// Save and restore env vars
	origForce := os.Getenv("KAI_FORCE_FULL")
	origPanic := os.Getenv("KAI_PANIC")
	defer func() {
		os.Setenv("KAI_FORCE_FULL", origForce)
		os.Setenv("KAI_PANIC", origPanic)
	}()

	tests := []struct {
		name       string
		forceFull  string
		panic      string
		wantSwitch bool
	}{
		{
			name:       "no env vars",
			forceFull:  "",
			panic:      "",
			wantSwitch: false,
		},
		{
			name:       "KAI_FORCE_FULL=1",
			forceFull:  "1",
			panic:      "",
			wantSwitch: true,
		},
		{
			name:       "KAI_FORCE_FULL=true",
			forceFull:  "true",
			panic:      "",
			wantSwitch: true,
		},
		{
			name:       "KAI_PANIC=1",
			forceFull:  "",
			panic:      "1",
			wantSwitch: true,
		},
		{
			name:       "KAI_PANIC=true",
			forceFull:  "",
			panic:      "true",
			wantSwitch: true,
		},
		{
			name:       "KAI_FORCE_FULL=0 doesn't trigger",
			forceFull:  "0",
			panic:      "",
			wantSwitch: false,
		},
		{
			name:       "KAI_FORCE_FULL=false doesn't trigger",
			forceFull:  "false",
			panic:      "",
			wantSwitch: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			os.Setenv("KAI_FORCE_FULL", tc.forceFull)
			os.Setenv("KAI_PANIC", tc.panic)

			got := checkPanicSwitch()
			if got != tc.wantSwitch {
				t.Errorf("checkPanicSwitch() = %v, want %v", got, tc.wantSwitch)
			}
		})
	}
}

// TestConfigFilePatterns verifies that config file patterns are comprehensive
func TestConfigFilePatterns(t *testing.T) {
	// These files should all be considered config files
	configFiles := []string{
		"package.json",
		"package-lock.json",
		"yarn.lock",
		"tsconfig.json",
		"jest.config.js",
		"webpack.config.js",
		"go.mod",
		"Cargo.toml",
		"Makefile",
		"Dockerfile",
	}

	// These files should NOT be considered config files
	codeFiles := []string{
		"src/app.js",
		"lib/utils.ts",
		"main.go",
		"server.py",
	}

	for _, file := range configFiles {
		found := false
		for _, pattern := range configFilePatterns {
			if file == pattern {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Config file %q not in configFilePatterns", file)
		}
	}

	for _, file := range codeFiles {
		for _, pattern := range configFilePatterns {
			if file == pattern {
				t.Errorf("Code file %q should not match configFilePatterns", file)
			}
		}
	}
}

// TestDefaultCIPolicy verifies the default policy has sensible values
func TestDefaultCIPolicy(t *testing.T) {
	policy := DefaultCIPolicy()

	// Check version
	if policy.Version != 1 {
		t.Errorf("DefaultCIPolicy().Version = %d, want 1", policy.Version)
	}

	// Check thresholds are set
	if policy.Thresholds.MinConfidence == 0 {
		t.Error("DefaultCIPolicy().Thresholds.MinConfidence should not be 0")
	}
	if policy.Thresholds.MaxUncertainty == 0 {
		t.Error("DefaultCIPolicy().Thresholds.MaxUncertainty should not be 0")
	}
	if policy.Thresholds.MaxFilesChanged == 0 {
		t.Error("DefaultCIPolicy().Thresholds.MaxFilesChanged should not be 0")
	}

	// Check paranoia patterns exist
	if len(policy.Paranoia.AlwaysFullPatterns) == 0 {
		t.Error("DefaultCIPolicy().Paranoia.AlwaysFullPatterns should not be empty")
	}

	// Check behavior defaults
	if policy.Behavior.OnHighRisk == "" {
		t.Error("DefaultCIPolicy().Behavior.OnHighRisk should not be empty")
	}
	if policy.Behavior.OnLowConfidence == "" {
		t.Error("DefaultCIPolicy().Behavior.OnLowConfidence should not be empty")
	}
}

// TestLoadCIPolicyDefault verifies loading without a file returns defaults
func TestLoadCIPolicyDefault(t *testing.T) {
	// Ensure we're in a directory without kai.ci-policy.yaml
	tmpDir := t.TempDir()
	oldDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldDir)

	policy, hash, err := loadCIPolicy()
	if err != nil {
		t.Fatalf("loadCIPolicy() error = %v", err)
	}

	// Hash should be empty when using defaults
	if hash != "" {
		t.Errorf("loadCIPolicy() hash = %q, want empty for defaults", hash)
	}

	// Should get default values
	if policy.Version != 1 {
		t.Errorf("loadCIPolicy().Version = %d, want 1", policy.Version)
	}
}

// TestLoadCIPolicyFromFile verifies loading from an actual file
func TestLoadCIPolicyFromFile(t *testing.T) {
	tmpDir := t.TempDir()
	oldDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldDir)

	// Write a test policy file
	policyYAML := `version: 2
thresholds:
  minConfidence: 0.5
  maxUncertainty: 80
  maxFilesChanged: 100
  maxTestsSkipped: 0.95
behavior:
  onHighRisk: fail
  onLowConfidence: warn
  onNoTests: pass
  failOnExpansion: true
`
	if err := os.WriteFile("kai.ci-policy.yaml", []byte(policyYAML), 0644); err != nil {
		t.Fatalf("WriteFile error = %v", err)
	}

	policy, hash, err := loadCIPolicy()
	if err != nil {
		t.Fatalf("loadCIPolicy() error = %v", err)
	}

	// Hash should not be empty
	if hash == "" {
		t.Error("loadCIPolicy() hash should not be empty when file exists")
	}

	// Check loaded values
	if policy.Version != 2 {
		t.Errorf("loadCIPolicy().Version = %d, want 2", policy.Version)
	}
	if policy.Thresholds.MinConfidence != 0.5 {
		t.Errorf("loadCIPolicy().Thresholds.MinConfidence = %f, want 0.5", policy.Thresholds.MinConfidence)
	}
	if policy.Behavior.OnHighRisk != "fail" {
		t.Errorf("loadCIPolicy().Behavior.OnHighRisk = %q, want 'fail'", policy.Behavior.OnHighRisk)
	}
	if !policy.Behavior.FailOnExpansion {
		t.Error("loadCIPolicy().Behavior.FailOnExpansion should be true")
	}
}

// TestCIPolicyThresholdsInExpansion verifies policy thresholds affect expansion decisions
func TestCIPolicyThresholdsInExpansion(t *testing.T) {
	// Create a policy with a high min confidence threshold
	highThresholdPolicy := CIPolicyConfig{
		Version: 1,
		Thresholds: CIPolicyThresholds{
			MinConfidence:   0.8, // High threshold
			MaxUncertainty:  50,
			MaxFilesChanged: 10,
			MaxTestsSkipped: 0.5,
		},
	}

	// Create a policy with a low min confidence threshold
	lowThresholdPolicy := CIPolicyConfig{
		Version: 1,
		Thresholds: CIPolicyThresholds{
			MinConfidence:   0.2, // Low threshold
			MaxUncertainty:  90,
			MaxFilesChanged: 100,
			MaxTestsSkipped: 0.99,
		},
	}

	// With 0.5 confidence and high threshold policy, should expand
	expand, reasons := shouldExpandForSafety("guarded", nil, 0.5, highThresholdPolicy)
	if !expand {
		t.Error("shouldExpandForSafety with 0.5 confidence and 0.8 threshold should expand")
	}
	if len(reasons) == 0 {
		t.Error("shouldExpandForSafety should provide reasons for expansion")
	}

	// With 0.5 confidence and low threshold policy, should NOT expand
	expand, _ = shouldExpandForSafety("guarded", nil, 0.5, lowThresholdPolicy)
	if expand {
		t.Error("shouldExpandForSafety with 0.5 confidence and 0.2 threshold should not expand")
	}
}

// TestDetectDynamicImports verifies dynamic import pattern detection
func TestDetectDynamicImports(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		filePath    string // File path affects language detection
		wantDynamic bool
		wantDesc    string // Now uses "kind" values from patterns
	}{
		{
			name:        "static require is safe",
			content:     `const foo = require("foo");`,
			filePath:    "test.js",
			wantDynamic: false,
		},
		{
			name:        "static import is safe",
			content:     `import foo from "foo";`,
			filePath:    "test.js",
			wantDynamic: false,
		},
		{
			name:        "dynamic require with variable",
			content:     `const mod = require(moduleName);`,
			filePath:    "test.js",
			wantDynamic: true,
			wantDesc:    "require(variable)",
		},
		{
			name:        "dynamic import with variable",
			content:     `const mod = await import(moduleName);`,
			filePath:    "test.js",
			wantDynamic: true,
			wantDesc:    "import(variable)",
		},
		{
			name:        "require.resolve with variable",
			content:     `const path = require.resolve(moduleName);`,
			filePath:    "test.js",
			wantDynamic: true,
			wantDesc:    "require.resolve(variable)",
		},
		{
			name:        "require.resolve with literal is false positive",
			content:     `const path = require.resolve("foo");`,
			filePath:    "test.js",
			wantDynamic: false, // Literal is filtered out as false positive
		},
		{
			name:        "python __import__",
			content:     `mod = __import__(module_name)`,
			filePath:    "test.py",
			wantDynamic: true,
			wantDesc:    "__import__(variable)",
		},
		{
			name:        "python importlib",
			content:     `mod = importlib.import_module(name)`,
			filePath:    "test.py",
			wantDynamic: true,
			wantDesc:    "importlib.import_module(variable)",
		},
		{
			name:        "go plugin.Open",
			content:     `p, err := plugin.Open(path)`,
			filePath:    "test.go",
			wantDynamic: true,
			wantDesc:    "plugin.Open(variable)",
		},
		{
			name:        "eval with require",
			content:     `eval("require('foo')")`,
			filePath:    "test.js",
			wantDynamic: true,
			wantDesc:    "eval(require)",
		},
		{
			name:        "webpack dynamic require bypass",
			content:     `const mod = __non_webpack_require__(path);`,
			filePath:    "test.js",
			wantDynamic: true,
			wantDesc:    "webpack bypass",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			filePath := tc.filePath
			if filePath == "" {
				filePath = "test.js"
			}
			hasDynamic, desc := detectDynamicImports([]byte(tc.content), filePath)

			if hasDynamic != tc.wantDynamic {
				t.Errorf("detectDynamicImports() = %v, want %v", hasDynamic, tc.wantDynamic)
			}

			if tc.wantDynamic && desc != tc.wantDesc {
				t.Errorf("detectDynamicImports() desc = %q, want %q", desc, tc.wantDesc)
			}
		})
	}
}

// TestDynamicImportRiskDetection verifies dynamic imports trigger structural risks
func TestDynamicImportRiskDetection(t *testing.T) {
	// Create a content reader that returns dynamic import content
	contentMap := map[string][]byte{
		"src/loader.js": []byte(`const mod = require(moduleName);`),
		"src/utils.ts":  []byte(`export function foo() {}`),
	}
	contentReader := func(path string) ([]byte, error) {
		if content, ok := contentMap[path]; ok {
			return content, nil
		}
		return nil, os.ErrNotExist
	}

	risks := detectStructuralRisksWithContent(
		[]string{"src/loader.js", "src/utils.ts"},
		map[string]bool{},
		[]string{},
		[]string{},
		contentReader,
	)

	// Should detect dynamic import in loader.js
	foundDynamicImport := false
	for _, r := range risks {
		if r.Type == RiskDynamicImport && r.FilePath == "src/loader.js" {
			foundDynamicImport = true
			if !r.Triggered {
				t.Error("Dynamic import risk should be triggered")
			}
			if r.Severity != "high" {
				t.Errorf("Dynamic import severity = %q, want 'high'", r.Severity)
			}
		}
	}

	if !foundDynamicImport {
		t.Error("Expected to find dynamic_import risk for src/loader.js")
	}
}

// TestRuntimeRiskPatterns verifies runtime risk pattern detection
func TestRuntimeRiskPatterns(t *testing.T) {
	tests := []struct {
		name         string
		content      string
		wantRisks    bool
		wantType     string
		wantSeverity string
	}{
		// Node.js / JavaScript
		{
			name:         "module not found JS",
			content:      `Error: Cannot find module 'missing-package'`,
			wantRisks:    true,
			wantType:     RuntimeRiskModuleNotFound,
			wantSeverity: "critical",
		},
		{
			name:         "Cannot find module with path",
			content:      `Cannot find module './utils/helper'`,
			wantRisks:    true,
			wantType:     RuntimeRiskModuleNotFound,
			wantSeverity: "critical",
		},
		{
			name:         "webpack module resolution",
			content:      `Module not found: Error: Can't resolve 'axios' in '/app/src'`,
			wantRisks:    true,
			wantType:     RuntimeRiskModuleNotFound,
			wantSeverity: "critical",
		},
		{
			name:         "reference error",
			content:      `ReferenceError: myFunction is not defined`,
			wantRisks:    true,
			wantType:     RuntimeRiskImportError,
			wantSeverity: "high",
		},
		{
			name:         "type error not a function",
			content:      `TypeError: doSomething is not a function`,
			wantRisks:    true,
			wantType:     RuntimeRiskImportError,
			wantSeverity: "high",
		},

		// TypeScript
		{
			name:         "typescript module not found",
			content:      `error TS2307: Cannot find module './missing' or its corresponding type declarations.`,
			wantRisks:    true,
			wantType:     RuntimeRiskModuleNotFound,
			wantSeverity: "critical",
		},
		{
			name:         "typescript export missing",
			content:      `error TS2305: Module '"./utils"' has no exported member 'helper'.`,
			wantRisks:    true,
			wantType:     RuntimeRiskImportError,
			wantSeverity: "critical",
		},
		{
			name:         "typescript error generic",
			content:      `error TS2304: Cannot find name 'MyType'.`,
			wantRisks:    true,
			wantType:     RuntimeRiskTypeError,
			wantSeverity: "high",
		},

		// Python
		{
			name:         "module not found Python",
			content:      `ModuleNotFoundError: No module named 'flask'`,
			wantRisks:    true,
			wantType:     RuntimeRiskModuleNotFound,
			wantSeverity: "critical",
		},
		{
			name:         "import error Python",
			content:      `ImportError: cannot import name 'foo' from 'bar'`,
			wantRisks:    true,
			wantType:     RuntimeRiskImportError,
			wantSeverity: "critical",
		},
		{
			name:         "Python importlib error",
			content:      `importlib.metadata.PackageNotFoundError: No package metadata was found`,
			wantRisks:    true,
			wantType:     RuntimeRiskImportError,
			wantSeverity: "critical",
		},
		{
			name:         "pytest fixture not found",
			content:      `fixture 'db_session' not found`,
			wantRisks:    true,
			wantType:     RuntimeRiskSetupCrash,
			wantSeverity: "critical",
		},
		{
			name:         "pytest collection errors",
			content:      `ERRORS during collection`,
			wantRisks:    true,
			wantType:     RuntimeRiskSetupCrash,
			wantSeverity: "critical",
		},

		// Go
		{
			name:         "go package not found",
			content:      `cannot find package "github.com/missing/pkg" in any of:`,
			wantRisks:    true,
			wantType:     RuntimeRiskModuleNotFound,
			wantSeverity: "critical",
		},
		{
			name:         "go plugin load failed",
			content:      `plugin.Open("./plugin.so") failed: plugin was built with a different version`,
			wantRisks:    true,
			wantType:     RuntimeRiskImportError,
			wantSeverity: "critical",
		},
		{
			name:         "go build failed",
			content:      `FAIL	github.com/example/pkg [build failed]`,
			wantRisks:    true,
			wantType:     RuntimeRiskSetupCrash,
			wantSeverity: "critical",
		},

		// Jest / Test setup
		{
			name:         "test setup crash",
			content:      `Test suite failed to run`,
			wantRisks:    true,
			wantType:     RuntimeRiskSetupCrash,
			wantSeverity: "critical",
		},
		{
			name:         "beforeAll failed",
			content:      `beforeAll hook failed: Error initializing database`,
			wantRisks:    true,
			wantType:     RuntimeRiskSetupCrash,
			wantSeverity: "critical",
		},
		{
			name:         "jest environment not found",
			content:      `Test environment jest-environment-jsdom not found`,
			wantRisks:    true,
			wantType:     RuntimeRiskSetupCrash,
			wantSeverity: "critical",
		},
		{
			name:         "jest unexpected token",
			content:      `Jest encountered an unexpected token`,
			wantRisks:    true,
			wantType:     RuntimeRiskSetupCrash,
			wantSeverity: "critical",
		},

		// Generic
		{
			name:         "out of memory",
			content:      `FATAL ERROR: heap out of memory`,
			wantRisks:    true,
			wantType:     RuntimeRiskSetupCrash,
			wantSeverity: "critical",
		},
		{
			name:         "clean output no risks",
			content:      `PASS src/app.test.js\nAll tests passed!`,
			wantRisks:    false,
			wantType:     "",
			wantSeverity: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Check each pattern against the content
			foundRisk := false
			var foundType, foundSeverity string

			for _, p := range runtimeRiskPatterns {
				re, err := regexp.Compile(p.pattern)
				if err != nil {
					continue
				}
				if re.MatchString(tc.content) {
					foundRisk = true
					foundType = p.riskType
					foundSeverity = p.severity
					break
				}
			}

			if foundRisk != tc.wantRisks {
				t.Errorf("runtimeRiskPatterns match = %v, want %v", foundRisk, tc.wantRisks)
			}

			if tc.wantRisks {
				if foundType != tc.wantType {
					t.Errorf("runtimeRiskPatterns type = %q, want %q", foundType, tc.wantType)
				}
				if foundSeverity != tc.wantSeverity {
					t.Errorf("runtimeRiskPatterns severity = %q, want %q", foundSeverity, tc.wantSeverity)
				}
			}
		})
	}
}

// TestExtractFailedTestsFromEvidence verifies parsing of test result formats
func TestExtractFailedTestsFromEvidence(t *testing.T) {
	tests := []struct {
		name      string
		evidence  string
		wantTests []string
	}{
		{
			name: "jest format",
			evidence: `{
				"testResults": [
					{"name": "tests/auth.test.js", "status": "passed"},
					{"name": "tests/api.test.js", "status": "failed"},
					{"name": "tests/db.test.js", "status": "failed"}
				]
			}`,
			wantTests: []string{"tests/api.test.js", "tests/db.test.js"},
		},
		{
			name: "pytest format",
			evidence: `{
				"tests": [
					{"nodeid": "tests/test_auth.py::test_login", "outcome": "passed"},
					{"nodeid": "tests/test_api.py::test_get", "outcome": "failed"}
				]
			}`,
			wantTests: []string{"tests/test_api.py::test_get"},
		},
		{
			name: "go test json format",
			evidence: `{"Action":"run","Package":"myapp/auth","Test":"TestLogin"}
{"Action":"pass","Package":"myapp/auth","Test":"TestLogin"}
{"Action":"run","Package":"myapp/api","Test":"TestGet"}
{"Action":"fail","Package":"myapp/api","Test":"TestGet"}`,
			wantTests: []string{"myapp/api/TestGet"},
		},
		{
			name:      "empty results",
			evidence:  `{}`,
			wantTests: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			failed := extractFailedTestsFromEvidence([]byte(tc.evidence))

			if len(failed) != len(tc.wantTests) {
				t.Errorf("extractFailedTestsFromEvidence() got %d tests, want %d", len(failed), len(tc.wantTests))
				t.Logf("got: %v", failed)
				t.Logf("want: %v", tc.wantTests)
				return
			}

			for i, want := range tc.wantTests {
				if i < len(failed) && failed[i] != want {
					t.Errorf("extractFailedTestsFromEvidence()[%d] = %q, want %q", i, failed[i], want)
				}
			}
		})
	}
}

// TestMissRecordDetection verifies miss detection logic
func TestMissRecordDetection(t *testing.T) {
	selected := []string{"tests/auth.test.js", "tests/api.test.js"}
	failed := []string{"tests/api.test.js", "tests/db.test.js", "tests/utils.test.js"}

	// Build selected set
	selectedSet := make(map[string]bool)
	for _, t := range selected {
		selectedSet[t] = true
	}

	// Find misses
	var missed []string
	for _, t := range failed {
		if !selectedSet[t] {
			missed = append(missed, t)
		}
	}

	// Expect db.test.js and utils.test.js to be missed
	if len(missed) != 2 {
		t.Errorf("Expected 2 missed tests, got %d", len(missed))
	}

	expectedMisses := map[string]bool{
		"tests/db.test.js":    true,
		"tests/utils.test.js": true,
	}

	for _, m := range missed {
		if !expectedMisses[m] {
			t.Errorf("Unexpected miss: %s", m)
		}
	}
}

// TestDetectDynamicImportsDetailed verifies detailed dynamic import detection
func TestDetectDynamicImportsDetailed(t *testing.T) {
	policy := &CIPolicyDynamicImports{
		Expansion:      "nearest_module",
		OwnersFallback: true,
		Allowlist:      []string{"src/vendor/**"},
	}

	tests := []struct {
		name            string
		content         string
		filePath        string
		wantCount       int
		wantBounded     bool
		wantAllowlisted bool
	}{
		{
			name:      "no dynamic imports",
			content:   `const foo = require("foo");`,
			filePath:  "src/app.js",
			wantCount: 0,
		},
		{
			name:      "dynamic require detected",
			content:   `const mod = require(moduleName);`,
			filePath:  "src/loader.js",
			wantCount: 1,
		},
		{
			name:            "allowlisted file",
			content:         `const mod = require(moduleName);`,
			filePath:        "src/vendor/legacy.js",
			wantCount:       1,
			wantAllowlisted: true,
		},
		{
			name: "bounded by webpackInclude",
			content: `
				/* webpackInclude: /\.widget\.js$/ */
				const mod = await import(moduleName);
			`,
			filePath:    "src/widgets.js",
			wantCount:   1,
			wantBounded: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			results := detectDynamicImportsDetailed([]byte(tc.content), tc.filePath, policy)

			if len(results) != tc.wantCount {
				t.Errorf("detectDynamicImportsDetailed() count = %d, want %d", len(results), tc.wantCount)
			}

			if tc.wantCount > 0 {
				if results[0].Bounded != tc.wantBounded {
					t.Errorf("detectDynamicImportsDetailed() bounded = %v, want %v", results[0].Bounded, tc.wantBounded)
				}
				if results[0].Allowlisted != tc.wantAllowlisted {
					t.Errorf("detectDynamicImportsDetailed() allowlisted = %v, want %v", results[0].Allowlisted, tc.wantAllowlisted)
				}
			}
		})
	}
}

// TestExpandForDynamicImports verifies scoped expansion strategies
func TestExpandForDynamicImports(t *testing.T) {
	allTestFiles := []string{
		"src/a/a.test.js",
		"src/a/a2.test.js",
		"src/b/b.test.js",
		"src/c/c.test.js",
	}
	// Use proper ModulePathMapping with path prefixes (not just names)
	moduleMappings := []ModulePathMapping{
		{Name: "A", PathPrefixes: []string{"src/a"}},
		{Name: "B", PathPrefixes: []string{"src/b"}},
		{Name: "C", PathPrefixes: []string{"src/c"}},
	}
	moduleTestMap := buildModuleTestMap(allTestFiles, moduleMappings)

	tests := []struct {
		name              string
		imports           []DynamicImportFile
		expansion         string
		ownersFallback    bool
		wantExpandedCount int
	}{
		{
			name:              "no imports - no expansion",
			imports:           []DynamicImportFile{},
			expansion:         "nearest_module",
			ownersFallback:    true,
			wantExpandedCount: 0,
		},
		{
			name: "bounded import - no expansion",
			imports: []DynamicImportFile{
				{Path: "src/a/loader.js", Kind: "import(variable)", Bounded: true},
			},
			expansion:         "nearest_module",
			ownersFallback:    true,
			wantExpandedCount: 0,
		},
		{
			name: "allowlisted import - no expansion",
			imports: []DynamicImportFile{
				{Path: "src/a/loader.js", Kind: "import(variable)", Allowlisted: true},
			},
			expansion:         "nearest_module",
			ownersFallback:    true,
			wantExpandedCount: 0,
		},
		{
			name: "unbounded import - nearest_module expansion",
			imports: []DynamicImportFile{
				{Path: "src/a/loader.js", Kind: "import(variable)", Bounded: false, Allowlisted: false},
			},
			expansion:         "nearest_module",
			ownersFallback:    true,
			wantExpandedCount: 2, // src/a/*.test.js
		},
		{
			name: "unbounded import - package expansion",
			imports: []DynamicImportFile{
				{Path: "src/a/loader.js", Kind: "import(variable)", Bounded: false, Allowlisted: false},
			},
			expansion:         "package",
			ownersFallback:    true,
			wantExpandedCount: 2, // src/a/*.test.js
		},
		{
			name: "unbounded import - full_suite expansion (no fallback)",
			imports: []DynamicImportFile{
				{Path: "src/a/loader.js", Kind: "import(variable)", Bounded: false, Allowlisted: false},
			},
			expansion:         "full_suite",
			ownersFallback:    false, // Disable fallback to go straight to full_suite
			wantExpandedCount: 4,     // All tests
		},
		{
			name: "unbounded import - unknown path falls back to full_suite",
			imports: []DynamicImportFile{
				{Path: "unknown/loader.js", Kind: "import(variable)", Bounded: false, Allowlisted: false},
			},
			expansion:         "nearest_module",
			ownersFallback:    true,
			wantExpandedCount: 4, // Falls back to full_suite since no module matches
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			policy := &CIPolicyDynamicImports{
				Expansion:      tc.expansion,
				OwnersFallback: tc.ownersFallback,
			}

			expandedTests, info := expandForDynamicImports(
				tc.imports,
				policy,
				allTestFiles,
				[]string{},
				moduleMappings,
				moduleTestMap,
			)

			if len(expandedTests) != tc.wantExpandedCount {
				t.Errorf("expandForDynamicImports() expanded %d tests, want %d", len(expandedTests), tc.wantExpandedCount)
			}

			// Verify telemetry
			if info == nil {
				t.Error("expandForDynamicImports() info should not be nil")
			} else if info.Telemetry.TotalDetected != len(tc.imports) {
				t.Errorf("telemetry.TotalDetected = %d, want %d", info.Telemetry.TotalDetected, len(tc.imports))
			}
		})
	}
}

// TestDynamicImportTelemetry verifies telemetry counters
func TestDynamicImportTelemetry(t *testing.T) {
	imports := []DynamicImportFile{
		{Path: "a.js", Kind: "import()", Bounded: false, Allowlisted: false},
		{Path: "b.js", Kind: "import()", Bounded: true, Allowlisted: false},
		{Path: "c.js", Kind: "import()", Bounded: false, Allowlisted: true},
		{Path: "d.js", Kind: "import()", Bounded: false, Allowlisted: false},
	}

	policy := &CIPolicyDynamicImports{
		Expansion: "full_suite",
	}

	_, info := expandForDynamicImports(
		imports,
		policy,
		[]string{"test.js"},
		[]string{},
		[]ModulePathMapping{}, // Empty module mappings
		map[string][]string{},
	)

	if info.Telemetry.TotalDetected != 4 {
		t.Errorf("TotalDetected = %d, want 4", info.Telemetry.TotalDetected)
	}
	if info.Telemetry.Bounded != 1 {
		t.Errorf("Bounded = %d, want 1", info.Telemetry.Bounded)
	}
	if info.Telemetry.Allowlisted != 1 {
		t.Errorf("Allowlisted = %d, want 1", info.Telemetry.Allowlisted)
	}
	if info.Telemetry.Unbounded != 2 {
		t.Errorf("Unbounded = %d, want 2", info.Telemetry.Unbounded)
	}
}

// TestDeterminism verifies that the same input always produces the same output
// This is critical for CI reproducibility
func TestDeterminism(t *testing.T) {
	changedFiles := []string{"src/app.js", "src/utils.js"}
	affectedTests := map[string]bool{
		"tests/app.test.js":   true,
		"tests/utils.test.js": true,
	}
	allTestFiles := []string{
		"tests/app.test.js",
		"tests/utils.test.js",
		"tests/other.test.js",
	}
	modules := []string{"App", "Utils"}

	// Run detection multiple times
	results := make([][]StructuralRisk, 10)
	for i := 0; i < 10; i++ {
		results[i] = detectStructuralRisks(changedFiles, affectedTests, allTestFiles, modules)
	}

	// All results should be identical
	first := results[0]
	for i := 1; i < 10; i++ {
		if len(results[i]) != len(first) {
			t.Fatalf("Run %d produced %d risks, run 0 produced %d", i, len(results[i]), len(first))
		}
		for j := range first {
			if results[i][j].Type != first[j].Type {
				t.Errorf("Run %d risk %d type = %s, want %s", i, j, results[i][j].Type, first[j].Type)
			}
			if results[i][j].Severity != first[j].Severity {
				t.Errorf("Run %d risk %d severity = %s, want %s", i, j, results[i][j].Severity, first[j].Severity)
			}
		}
	}
}

// TestConfidenceDeterminism verifies confidence calculation is deterministic
func TestConfidenceDeterminism(t *testing.T) {
	risks := []StructuralRisk{
		{Type: RiskConfigChange, Severity: "high"},
		{Type: RiskDynamicImport, Severity: "critical"},
	}

	results := make([]float64, 10)
	for i := 0; i < 10; i++ {
		results[i] = calculateConfidence(risks, 5, 3)
	}

	for i := 1; i < 10; i++ {
		if results[i] != results[0] {
			t.Errorf("Confidence calculation not deterministic: run %d = %f, run 0 = %f", i, results[i], results[0])
		}
	}
}

// TestLowConfidenceUnboundedImportNotSkipped verifies that unbounded imports
// with low confidence are NOT skipped (they're still risky)
func TestLowConfidenceUnboundedImportNotSkipped(t *testing.T) {
	// Create content with a dynamic import that would have low confidence
	// but is NOT bounded - this should NOT be skipped
	content := []byte(`
// This is some test code
function loadPlugin(name) {
  // No safe-eval annotation here
  return require(name);  // unbounded dynamic require
}
`)

	policy := &CIPolicyDynamicImports{
		Allowlist: []string{},
	}

	imports := detectDynamicImportsDetailed(content, "test.js", policy)

	// Even with low confidence, unbounded imports should be detected
	foundUnbounded := false
	for _, imp := range imports {
		if !imp.Bounded {
			foundUnbounded = true
			break
		}
	}

	if !foundUnbounded {
		t.Error("Expected unbounded import to be detected even with potential low confidence")
	}
}

// TestBoundedLowConfidenceImportSkipped verifies that bounded imports
// with very low confidence ARE skipped (likely false positives)
func TestBoundedLowConfidenceImportSkipped(t *testing.T) {
	// Content with a bounded import that has very low confidence
	// due to false positive patterns
	content := []byte(`
// @kai:safe-eval bounded to: ['module-a', 'module-b']
const modules = {
  'a': require('./a'),  // This is a static require, not dynamic
  'b': require('./b'),
};
// Just regular object literal, not dynamic import
`)

	policy := &CIPolicyDynamicImports{
		Allowlist: []string{},
	}

	imports := detectDynamicImportsDetailed(content, "config.js", policy)

	// Bounded + low confidence should be skipped
	// The require('./a') should be detected as static, not dynamic
	for _, imp := range imports {
		if imp.Bounded && imp.Confidence <= 0.1 {
			t.Errorf("Low confidence bounded import should have been skipped: %+v", imp)
		}
	}
}

// TestDynamicImportCacheInvalidation verifies cache is invalidated on detector version change
func TestDynamicImportCacheInvalidation(t *testing.T) {
	cache := &DynamicImportCache{
		entries: make(map[string]DynamicImportCacheEntry),
	}

	testDigest := "abc123"
	testImports := []DynamicImportFile{
		{Path: "test.js", Kind: "import()", Line: 1},
	}

	// Store with current version
	cache.Set(testDigest, testImports)

	// Should retrieve with current version
	retrieved, ok := cache.Get(testDigest)
	if !ok {
		t.Fatal("Cache miss for just-stored entry")
	}
	if len(retrieved) != 1 {
		t.Errorf("Retrieved %d imports, want 1", len(retrieved))
	}

	// Manually modify entry to simulate old version
	cache.mu.Lock()
	cache.entries[testDigest] = DynamicImportCacheEntry{
		DetectorVersion: "0.9.0", // Old version
		Imports:         testImports,
	}
	cache.mu.Unlock()

	// Should NOT retrieve with mismatched version
	_, ok = cache.Get(testDigest)
	if ok {
		t.Error("Expected cache miss for mismatched detector version")
	}
}

// TestExpansionStrategyDeterminism verifies expansion strategy produces consistent results
func TestExpansionStrategyDeterminism(t *testing.T) {
	imports := []DynamicImportFile{
		{Path: "src/loader.js", Kind: "import()", Bounded: false, Allowlisted: false},
	}

	policy := &CIPolicyDynamicImports{
		Expansion: "nearest_module",
	}

	allTests := []string{
		"src/loader.test.js",
		"src/utils.test.js",
		"tests/app.test.js",
	}
	changedFiles := []string{"src/loader.js"}
	// Use proper path prefixes for module matching
	moduleMappings := []ModulePathMapping{
		{Name: "Src", PathPrefixes: []string{"src"}},
		{Name: "Tests", PathPrefixes: []string{"tests"}},
	}
	moduleTestMap := map[string][]string{
		"Src":   {"src/loader.test.js", "src/utils.test.js"},
		"Tests": {"tests/app.test.js"},
	}

	// Run expansion multiple times
	results := make([][]string, 10)
	for i := 0; i < 10; i++ {
		expanded, _ := expandForDynamicImports(imports, policy, allTests, changedFiles, moduleMappings, moduleTestMap)
		results[i] = expanded
	}

	// All results should be identical
	for i := 1; i < 10; i++ {
		if len(results[i]) != len(results[0]) {
			t.Fatalf("Run %d produced %d tests, run 0 produced %d", i, len(results[i]), len(results[0]))
		}
		for j := range results[0] {
			if results[i][j] != results[0][j] {
				t.Errorf("Run %d test %d = %s, want %s", i, j, results[i][j], results[0][j])
			}
		}
	}
}

// ========== Coverage Parser Tests ==========

func TestParseNYCCoverage(t *testing.T) {
	nycJSON := `{
		"/app/src/utils.js": {
			"path": "/app/src/utils.js",
			"statementMap": {
				"0": {"start": {"line": 1}, "end": {"line": 1}},
				"1": {"start": {"line": 2}, "end": {"line": 2}},
				"2": {"start": {"line": 5}, "end": {"line": 5}}
			},
			"s": {"0": 10, "1": 5, "2": 0}
		},
		"/app/src/app.js": {
			"path": "/app/src/app.js",
			"statementMap": {
				"0": {"start": {"line": 10}, "end": {"line": 10}}
			},
			"s": {"0": 3}
		}
	}`

	entries, err := parseNYCCoverage([]byte(nycJSON))
	if err != nil {
		t.Fatalf("parseNYCCoverage failed: %v", err)
	}

	if len(entries) != 2 {
		t.Errorf("got %d files, want 2", len(entries))
	}

	utilsEntry, ok := entries["/app/src/utils.js"]
	if !ok {
		t.Error("missing utils.js entry")
	} else if len(utilsEntry[0].LinesCovered) != 2 {
		// Only lines 1 and 2 have hits > 0
		t.Errorf("got %d covered lines, want 2", len(utilsEntry[0].LinesCovered))
	}
}

func TestParseCoveragePyCoverage(t *testing.T) {
	coveragePyJSON := `{
		"files": {
			"src/main.py": {
				"executed_lines": [1, 2, 3, 10, 15]
			},
			"src/util.py": {
				"executed_lines": [5, 6, 7]
			}
		}
	}`

	entries, err := parseCoveragePyCoverage([]byte(coveragePyJSON))
	if err != nil {
		t.Fatalf("parseCoveragePyCoverage failed: %v", err)
	}

	if len(entries) != 2 {
		t.Errorf("got %d files, want 2", len(entries))
	}

	mainEntry, ok := entries["src/main.py"]
	if !ok {
		t.Error("missing main.py entry")
	} else if len(mainEntry[0].LinesCovered) != 5 {
		t.Errorf("got %d covered lines, want 5", len(mainEntry[0].LinesCovered))
	}
}

func TestDetectCoverageFormat(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		content  string
		want     string
	}{
		{
			name:     "nyc from filename",
			filename: "coverage-final.json",
			content:  "{}",
			want:     "nyc",
		},
		{
			name:     "nyc from content",
			filename: "coverage.json",
			content:  `{"path": "/app/src/a.js", "statementMap": {}}`,
			want:     "nyc",
		},
		{
			name:     "jacoco from filename",
			filename: "jacoco.xml",
			content:  "<?xml",
			want:     "jacoco",
		},
		{
			name:     "jacoco from content",
			filename: "coverage.xml",
			content:  "<jacoco version='1.0'>",
			want:     "jacoco",
		},
		{
			name:     "coveragepy from content",
			filename: "coverage.json",
			content:  `{"files": {"test.py": {"executed_lines": [1, 2]}}}`,
			want:     "coveragepy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectCoverageFormat(tt.filename, []byte(tt.content))
			if got != tt.want {
				t.Errorf("detectCoverageFormat() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ========== Coverage Map Tests ==========

func TestCoverageMapMerge(t *testing.T) {
	existing := []CoverageEntry{
		{TestID: "test1", HitCount: 5, LastSeenAt: "2024-01-01"},
		{TestID: "test2", HitCount: 3, LastSeenAt: "2024-01-01"},
	}

	new := []CoverageEntry{
		{TestID: "test1", HitCount: 2, LastSeenAt: "2024-01-02"},
		{TestID: "test3", HitCount: 1, LastSeenAt: "2024-01-02"},
	}

	merged := mergeTestEntries(existing, new)

	if len(merged) != 3 {
		t.Errorf("got %d entries, want 3", len(merged))
	}

	// Find test1 and verify hit count was accumulated
	for _, e := range merged {
		if e.TestID == "test1" {
			if e.HitCount != 7 {
				t.Errorf("test1 HitCount = %d, want 7", e.HitCount)
			}
			if e.LastSeenAt != "2024-01-02" {
				t.Errorf("test1 LastSeenAt = %q, want 2024-01-02", e.LastSeenAt)
			}
		}
	}
}

// ========== Contract Registry Tests ==========

func TestContractBindingDigest(t *testing.T) {
	// Create a temporary schema file
	tmpDir := t.TempDir()
	schemaPath := tmpDir + "/api.yaml"

	schema1 := "openapi: 3.0.0\npaths: {}"
	if err := os.WriteFile(schemaPath, []byte(schema1), 0644); err != nil {
		t.Fatal(err)
	}

	// Compute digest
	data, _ := os.ReadFile(schemaPath)
	digest1 := computeTestDigest(data)

	// Modify schema
	schema2 := "openapi: 3.0.0\npaths:\n  /users: {}"
	if err := os.WriteFile(schemaPath, []byte(schema2), 0644); err != nil {
		t.Fatal(err)
	}

	data, _ = os.ReadFile(schemaPath)
	digest2 := computeTestDigest(data)

	if digest1 == digest2 {
		t.Error("digests should be different after schema change")
	}
}

// computeTestDigest is a test helper for computing digests
func computeTestDigest(data []byte) string {
	// Simple checksum for testing - in reality uses Blake3
	sum := 0
	for _, b := range data {
		sum += int(b)
	}
	return string(rune(sum % 256))
}

// ========== Fallback Annotation Tests ==========

func TestCIFallbackStruct(t *testing.T) {
	fallback := CIFallback{
		Used:     true,
		Reason:   "runtime_tripwire",
		Trigger:  "ModuleNotFoundError: No module named 'missing'",
		ExitCode: 75,
	}

	if !fallback.Used {
		t.Error("fallback.Used should be true")
	}
	if fallback.Reason != "runtime_tripwire" {
		t.Errorf("fallback.Reason = %q, want runtime_tripwire", fallback.Reason)
	}
	if fallback.ExitCode != 75 {
		t.Errorf("fallback.ExitCode = %d, want 75", fallback.ExitCode)
	}
}

// ========== Policy Config Tests ==========

func TestDefaultCIPolicyCoverage(t *testing.T) {
	policy := DefaultCIPolicy()

	if !policy.Coverage.Enabled {
		t.Error("Coverage should be enabled by default")
	}
	if policy.Coverage.LookbackDays != 30 {
		t.Errorf("Coverage.LookbackDays = %d, want 30", policy.Coverage.LookbackDays)
	}
	if policy.Coverage.MinHits != 1 {
		t.Errorf("Coverage.MinHits = %d, want 1", policy.Coverage.MinHits)
	}
	if policy.Coverage.OnNoCoverage != "warn" {
		t.Errorf("Coverage.OnNoCoverage = %q, want warn", policy.Coverage.OnNoCoverage)
	}
}

func TestDefaultCIPolicyContracts(t *testing.T) {
	policy := DefaultCIPolicy()

	if !policy.Contracts.Enabled {
		t.Error("Contracts should be enabled by default")
	}
	if policy.Contracts.OnChange != "add_tests" {
		t.Errorf("Contracts.OnChange = %q, want add_tests", policy.Contracts.OnChange)
	}
	if len(policy.Contracts.Types) != 3 {
		t.Errorf("Contracts.Types = %v, want 3 types", policy.Contracts.Types)
	}
}

func TestMapKeysToSortedSlice(t *testing.T) {
	m := map[string]bool{
		"zebra":    true,
		"apple":    true,
		"mango":    true,
		"banana":   true,
	}

	result := mapKeysToSortedSlice(m)

	if len(result) != 4 {
		t.Errorf("got %d items, want 4", len(result))
	}

	// Should be alphabetically sorted
	expected := []string{"apple", "banana", "mango", "zebra"}
	for i, v := range expected {
		if result[i] != v {
			t.Errorf("result[%d] = %q, want %q", i, result[i], v)
		}
	}
}

// TestExtractPathPrefix verifies that glob patterns are correctly converted to path prefixes
func TestExtractPathPrefix(t *testing.T) {
	tests := []struct {
		pattern string
		want    string
	}{
		// Standard glob suffixes
		{"src/app/**", "src/app"},
		{"src/app/*", "src/app"},
		{"lib/utils/**/*", "lib/utils"},
		{"lib/utils**", "lib/utils"},

		// Single file patterns
		{"src/app.js", "src/app.js"},
		{"lib/utils.ts", "lib/utils.ts"},

		// Wildcards in middle
		{"src/*/components", "src"},
		{"src/app/*.js", "src/app"},
		{"lib/[a-z]/utils", "lib"},

		// Edge cases
		{"**", ""},
		{"*", ""},
		{"src", "src"},
	}

	for _, tc := range tests {
		t.Run(tc.pattern, func(t *testing.T) {
			got := extractPathPrefix(tc.pattern)
			if got != tc.want {
				t.Errorf("extractPathPrefix(%q) = %q, want %q", tc.pattern, got, tc.want)
			}
		})
	}
}

// TestBuildModuleTestMap verifies that test files are correctly mapped to modules
// using path prefixes (not module names!)
func TestBuildModuleTestMap(t *testing.T) {
	// This test specifically catches the bug where module names were used
	// instead of path prefixes for matching

	testFiles := []string{
		"src/app/app.test.js",
		"src/app/components/button.test.js",
		"src/utils/helper.test.js",
		"lib/core/core.test.js",
		"tests/integration/e2e.test.js",
	}

	// Simulate what buildModulePathMappings would produce:
	// Module "App" has paths ["src/app/**"] -> prefix "src/app"
	// Module "Utils" has paths ["src/utils/**"] -> prefix "src/utils"
	// Module "Core" has paths ["lib/core/**"] -> prefix "lib/core"
	moduleMappings := []ModulePathMapping{
		{Name: "App", PathPrefixes: []string{"src/app"}},
		{Name: "Utils", PathPrefixes: []string{"src/utils"}},
		{Name: "Core", PathPrefixes: []string{"lib/core"}},
	}

	result := buildModuleTestMap(testFiles, moduleMappings)

	// Verify App module gets the right tests
	appTests := result["App"]
	if len(appTests) != 2 {
		t.Errorf("App module got %d tests, want 2: %v", len(appTests), appTests)
	}
	wantAppTests := map[string]bool{
		"src/app/app.test.js":              true,
		"src/app/components/button.test.js": true,
	}
	for _, test := range appTests {
		if !wantAppTests[test] {
			t.Errorf("Unexpected test in App: %s", test)
		}
	}

	// Verify Utils module gets the right tests
	utilsTests := result["Utils"]
	if len(utilsTests) != 1 || utilsTests[0] != "src/utils/helper.test.js" {
		t.Errorf("Utils module got wrong tests: %v", utilsTests)
	}

	// Verify Core module gets the right tests
	coreTests := result["Core"]
	if len(coreTests) != 1 || coreTests[0] != "lib/core/core.test.js" {
		t.Errorf("Core module got wrong tests: %v", coreTests)
	}
}

// TestBuildModuleTestMapWithModuleNamesWouldFail demonstrates the bug that was fixed
// If we passed module names instead of path prefixes, no tests would match
func TestBuildModuleTestMapWithModuleNamesWouldFail(t *testing.T) {
	testFiles := []string{
		"src/app/app.test.js",
		"src/utils/helper.test.js",
	}

	// WRONG: Using module names as if they were paths (the old bug)
	// This simulates what would happen if we passed ["App", "Utils"] directly
	wrongMappings := []ModulePathMapping{
		{Name: "App", PathPrefixes: []string{"App"}},    // Wrong! "App" doesn't match "src/app"
		{Name: "Utils", PathPrefixes: []string{"Utils"}}, // Wrong! "Utils" doesn't match "src/utils"
	}

	result := buildModuleTestMap(testFiles, wrongMappings)

	// With wrong mappings, no tests should match (demonstrating the bug)
	if len(result["App"]) != 0 {
		t.Errorf("With wrong mappings, App should have 0 tests but got: %v", result["App"])
	}
	if len(result["Utils"]) != 0 {
		t.Errorf("With wrong mappings, Utils should have 0 tests but got: %v", result["Utils"])
	}
}

// TestExpandSingleImportUsesPathPrefixes verifies that expandSingleImport
// correctly matches files to modules using path prefixes
func TestExpandSingleImportUsesPathPrefixes(t *testing.T) {
	policy := &CIPolicyDynamicImports{
		Expansion:      "nearest_module",
		OwnersFallback: true,
	}

	allTestFiles := []string{
		"src/app/app.test.js",
		"src/app/loader.test.js",
		"src/utils/utils.test.js",
		"tests/e2e.test.js",
	}

	// Correct mappings with path prefixes
	moduleMappings := []ModulePathMapping{
		{Name: "App", PathPrefixes: []string{"src/app"}},
		{Name: "Utils", PathPrefixes: []string{"src/utils"}},
	}

	filesByModule := map[string][]string{
		"App":   {"src/app/app.test.js", "src/app/loader.test.js"},
		"Utils": {"src/utils/utils.test.js"},
	}

	// Dynamic import in src/app/loader.js should match the App module
	imp := DynamicImportFile{
		Path: "src/app/loader.js",
		Kind: "require(variable)",
		Line: 42,
	}

	tests, strategy := expandSingleImport(imp, policy, allTestFiles, moduleMappings, filesByModule)

	// Should find the App module and return its tests
	if len(tests) != 2 {
		t.Errorf("expandSingleImport got %d tests, want 2: %v", len(tests), tests)
	}

	if strategy != "nearest_module: App" {
		t.Errorf("expandSingleImport strategy = %q, want 'nearest_module: App'", strategy)
	}
}

// TestExpandSingleImportWithModuleNamesWouldFallback demonstrates the bug fix
// With wrong mappings (module names instead of paths), it would fall back to broader strategies
func TestExpandSingleImportWithModuleNamesWouldFallback(t *testing.T) {
	policy := &CIPolicyDynamicImports{
		Expansion:      "nearest_module",
		OwnersFallback: false, // Disable fallback to see the effect
	}

	allTestFiles := []string{
		"src/app/app.test.js",
		"tests/e2e.test.js",
	}

	// WRONG: Module names instead of path prefixes (the old bug)
	wrongMappings := []ModulePathMapping{
		{Name: "App", PathPrefixes: []string{"App"}}, // Won't match src/app/
	}

	filesByModule := map[string][]string{
		"App": {"src/app/app.test.js"},
	}

	imp := DynamicImportFile{
		Path: "src/app/loader.js",
		Kind: "require(variable)",
		Line: 42,
	}

	tests, strategy := expandSingleImport(imp, policy, allTestFiles, wrongMappings, filesByModule)

	// With wrong mappings and no fallback, should go to full_suite
	if strategy != "full_suite" {
		t.Errorf("With wrong mappings and no fallback, expected 'full_suite' but got %q", strategy)
	}
	if len(tests) != len(allTestFiles) {
		t.Errorf("With full_suite, expected all %d tests but got %d", len(allTestFiles), len(tests))
	}
}

// TestModulePathMappingIntegration tests the full flow from module names to correct test matching
func TestModulePathMappingIntegration(t *testing.T) {
	// This is an integration test that simulates the full flow

	testFiles := []string{
		"src/widgets/button.test.js",
		"src/widgets/modal.test.js",
		"src/api/client.test.js",
		"src/api/auth.test.js",
		"lib/helpers/format.test.js",
	}

	// Simulate module configurations like in kai.modules.yaml
	// Module "Widgets" -> paths: ["src/widgets/**"]
	// Module "API" -> paths: ["src/api/**"]
	// Module "Helpers" -> paths: ["lib/helpers/**"]
	moduleMappings := []ModulePathMapping{
		{Name: "Widgets", PathPrefixes: []string{"src/widgets"}},
		{Name: "API", PathPrefixes: []string{"src/api"}},
		{Name: "Helpers", PathPrefixes: []string{"lib/helpers"}},
	}

	testMap := buildModuleTestMap(testFiles, moduleMappings)

	// Verify each module gets correct tests
	if len(testMap["Widgets"]) != 2 {
		t.Errorf("Widgets should have 2 tests, got %d: %v", len(testMap["Widgets"]), testMap["Widgets"])
	}
	if len(testMap["API"]) != 2 {
		t.Errorf("API should have 2 tests, got %d: %v", len(testMap["API"]), testMap["API"])
	}
	if len(testMap["Helpers"]) != 1 {
		t.Errorf("Helpers should have 1 test, got %d: %v", len(testMap["Helpers"]), testMap["Helpers"])
	}

	// Now test expansion for a file in Widgets
	policy := &CIPolicyDynamicImports{
		Expansion:      "nearest_module",
		OwnersFallback: true,
	}

	imp := DynamicImportFile{
		Path: "src/widgets/dynamic-loader.js",
		Kind: "import(variable)",
		Line: 10,
	}

	expandedTests, strategy := expandSingleImport(imp, policy, testFiles, moduleMappings, testMap)

	// Should expand to Widgets tests only (nearest_module strategy)
	if strategy != "nearest_module: Widgets" {
		t.Errorf("Expected strategy 'nearest_module: Widgets', got %q", strategy)
	}
	if len(expandedTests) != 2 {
		t.Errorf("Expected 2 tests for Widgets, got %d: %v", len(expandedTests), expandedTests)
	}

	// Verify the right tests were selected
	expectedTests := map[string]bool{
		"src/widgets/button.test.js": true,
		"src/widgets/modal.test.js":  true,
	}
	for _, test := range expandedTests {
		if !expectedTests[test] {
			t.Errorf("Unexpected test in expansion: %s", test)
		}
	}
}
