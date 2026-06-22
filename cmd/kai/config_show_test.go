package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupConfigShowDir creates a temp kaiDir with a minimal config.yaml and
// returns the directory path along with a cleanup function.
func setupConfigShowDir(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	cfgContent := "model: test-model\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfgContent), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	oldKaiDir := kaiDir
	kaiDir = dir
	return dir, func() { kaiDir = oldKaiDir }
}

// clearProviderEnv unsets all env vars that runConfigShow reads so each test
// starts from a known state.
func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"KAI_PROVIDER",
		"KAI_ANTHROPIC_MODEL",
		"KAI_OPENAI_MODEL",
		"ANTHROPIC_API_KEY",
		"OPENAI_API_KEY",
	} {
		t.Setenv(k, "")
	}
}

// TestRunConfigShow_YAML verifies that the default (YAML) output contains
// expected keys when no provider env var is set.
func TestRunConfigShow_YAML(t *testing.T) {
	_, cleanup := setupConfigShowDir(t)
	defer cleanup()
	clearProviderEnv(t)

	oldJSON := configShowJSON
	configShowJSON = false
	defer func() { configShowJSON = oldJSON }()

	out := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})

	if !strings.Contains(out, "provider:") {
		t.Errorf("expected 'provider:' in YAML output; got:\n%s", out)
	}
	if !strings.Contains(out, "kailab") {
		t.Errorf("expected default provider 'kailab' in output; got:\n%s", out)
	}
	if !strings.Contains(out, "kai_dir:") {
		t.Errorf("expected 'kai_dir:' in YAML output; got:\n%s", out)
	}
}

// TestRunConfigShow_DefaultProvider verifies that an unset KAI_PROVIDER
// resolves to "kailab" and uses the kailab token source.
func TestRunConfigShow_DefaultProvider(t *testing.T) {
	_, cleanup := setupConfigShowDir(t)
	defer cleanup()
	clearProviderEnv(t)

	oldJSON := configShowJSON
	configShowJSON = false
	defer func() { configShowJSON = oldJSON }()

	out := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})

	if !strings.Contains(out, "kailab") {
		t.Errorf("expected provider 'kailab'; got:\n%s", out)
	}
	if !strings.Contains(out, "kailab token") {
		t.Errorf("expected api_key_source to mention 'kailab token'; got:\n%s", out)
	}
}

// TestRunConfigShow_AnthropicProviderNoKey verifies anthropic provider without
// the ANTHROPIC_API_KEY set reports "(not set)".
func TestRunConfigShow_AnthropicProviderNoKey(t *testing.T) {
	_, cleanup := setupConfigShowDir(t)
	defer cleanup()
	clearProviderEnv(t)
	t.Setenv("KAI_PROVIDER", "anthropic")

	oldJSON := configShowJSON
	configShowJSON = false
	defer func() { configShowJSON = oldJSON }()

	out := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})

	if !strings.Contains(out, "anthropic") {
		t.Errorf("expected provider 'anthropic'; got:\n%s", out)
	}
	if !strings.Contains(out, "(not set)") {
		t.Errorf("expected api_key_source '(not set)'; got:\n%s", out)
	}
}

// TestRunConfigShow_AnthropicProviderWithKey verifies anthropic provider with
// ANTHROPIC_API_KEY set reports "ANTHROPIC_API_KEY (env)".
func TestRunConfigShow_AnthropicProviderWithKey(t *testing.T) {
	_, cleanup := setupConfigShowDir(t)
	defer cleanup()
	clearProviderEnv(t)
	t.Setenv("KAI_PROVIDER", "anthropic")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("KAI_ANTHROPIC_MODEL", "claude-3-opus")

	oldJSON := configShowJSON
	configShowJSON = false
	defer func() { configShowJSON = oldJSON }()

	out := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})

	if !strings.Contains(out, "ANTHROPIC_API_KEY (env)") {
		t.Errorf("expected 'ANTHROPIC_API_KEY (env)'; got:\n%s", out)
	}
	if !strings.Contains(out, "claude-3-opus") {
		t.Errorf("expected model override 'claude-3-opus'; got:\n%s", out)
	}
}

// TestRunConfigShow_OpenAIProviderNoKey verifies openai provider without
// OPENAI_API_KEY set reports the local-endpoint message.
func TestRunConfigShow_OpenAIProviderNoKey(t *testing.T) {
	_, cleanup := setupConfigShowDir(t)
	defer cleanup()
	clearProviderEnv(t)
	t.Setenv("KAI_PROVIDER", "openai")

	oldJSON := configShowJSON
	configShowJSON = false
	defer func() { configShowJSON = oldJSON }()

	out := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})

	if !strings.Contains(out, "local endpoint assumed") {
		t.Errorf("expected 'local endpoint assumed'; got:\n%s", out)
	}
}

// TestRunConfigShow_OpenAIProviderWithKey verifies openai provider with
// OPENAI_API_KEY set reports "OPENAI_API_KEY (env)".
func TestRunConfigShow_OpenAIProviderWithKey(t *testing.T) {
	_, cleanup := setupConfigShowDir(t)
	defer cleanup()
	clearProviderEnv(t)
	t.Setenv("KAI_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "sk-openai-test")
	t.Setenv("KAI_OPENAI_MODEL", "gpt-4o")

	oldJSON := configShowJSON
	configShowJSON = false
	defer func() { configShowJSON = oldJSON }()

	out := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})

	if !strings.Contains(out, "OPENAI_API_KEY (env)") {
		t.Errorf("expected 'OPENAI_API_KEY (env)'; got:\n%s", out)
	}
	if !strings.Contains(out, "gpt-4o") {
		t.Errorf("expected model override 'gpt-4o'; got:\n%s", out)
	}
}

// TestRunConfigShow_LocalProvider verifies that the "local" provider alias
// works the same as "openai".
func TestRunConfigShow_LocalProvider(t *testing.T) {
	_, cleanup := setupConfigShowDir(t)
	defer cleanup()
	clearProviderEnv(t)
	t.Setenv("KAI_PROVIDER", "local")

	oldJSON := configShowJSON
	configShowJSON = false
	defer func() { configShowJSON = oldJSON }()

	out := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})

	if !strings.Contains(out, "local endpoint assumed") {
		t.Errorf("expected 'local endpoint assumed' for local provider; got:\n%s", out)
	}
}

// TestRunConfigShow_JSON verifies that --json output is valid JSON containing
// the expected fields.
func TestRunConfigShow_JSON(t *testing.T) {
	dir, cleanup := setupConfigShowDir(t)
	defer cleanup()
	clearProviderEnv(t)

	oldJSON := configShowJSON
	configShowJSON = true
	defer func() { configShowJSON = oldJSON }()

	out := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput:\n%s", err, out)
	}

	runtime, ok := parsed["runtime"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected 'runtime' object in JSON; got:\n%s", out)
	}
	if runtime["provider"] != "kailab" {
		t.Errorf("expected runtime.provider=kailab; got %v", runtime["provider"])
	}
	if runtime["kai_dir"] != dir {
		t.Errorf("expected runtime.kai_dir=%q; got %v", dir, runtime["kai_dir"])
	}
	if _, hasConfig := parsed["config"]; !hasConfig {
		t.Errorf("expected 'config' key in JSON output; got:\n%s", out)
	}
}

// TestRunConfigShow_JSONAnthropicAlias verifies that the "claude" alias maps to
// the anthropic branch in JSON mode.
func TestRunConfigShow_JSONAnthropicAlias(t *testing.T) {
	_, cleanup := setupConfigShowDir(t)
	defer cleanup()
	clearProviderEnv(t)
	t.Setenv("KAI_PROVIDER", "claude")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-alias")

	oldJSON := configShowJSON
	configShowJSON = true
	defer func() { configShowJSON = oldJSON }()

	out := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput:\n%s", err, out)
	}
	runtime := parsed["runtime"].(map[string]interface{})
	if runtime["api_key_source"] != "ANTHROPIC_API_KEY (env)" {
		t.Errorf("expected ANTHROPIC_API_KEY (env); got %v", runtime["api_key_source"])
	}
}

// TestRunConfigShow_Quiet verifies that --quiet suppresses the runtime block
// and still prints the config section in YAML mode.
func TestRunConfigShow_Quiet(t *testing.T) {
	_, cleanup := setupConfigShowDir(t)
	defer cleanup()
	clearProviderEnv(t)

	oldJSON := configShowJSON
	configShowJSON = false
	defer func() { configShowJSON = oldJSON }()

	oldQuiet := configShowQuiet
	configShowQuiet = true
	defer func() { configShowQuiet = oldQuiet }()

	out := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})

	// Runtime-only fields must NOT appear.
	for _, unwanted := range []string{"runtime:", "kai_dir:", "provider:", "api_key_source:"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("quiet mode: unexpected runtime field %q in output:\n%s", unwanted, out)
		}
	}

	// The config section must appear.
	if !strings.Contains(out, "config:") {
		t.Errorf("quiet mode: expected 'config:' section in output; got:\n%s", out)
	}
}

// TestRunConfigShow_QuietJSON verifies that --quiet --json omits the runtime
// key and returns only the config object.
func TestRunConfigShow_QuietJSON(t *testing.T) {
	_, cleanup := setupConfigShowDir(t)
	defer cleanup()
	clearProviderEnv(t)

	oldJSON := configShowJSON
	configShowJSON = true
	defer func() { configShowJSON = oldJSON }()

	oldQuiet := configShowQuiet
	configShowQuiet = true
	defer func() { configShowQuiet = oldQuiet }()

	out := captureStdout(t, func() {
		if err := runConfigShow(nil, nil); err != nil {
			t.Fatalf("runConfigShow: %v", err)
		}
	})

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("quiet JSON output is not valid JSON: %v\noutput:\n%s", err, out)
	}

	// The runtime key must NOT be present.
	if _, hasRuntime := parsed["runtime"]; hasRuntime {
		t.Errorf("quiet JSON mode: unexpected 'runtime' key in output:\n%s", out)
	}

	// At least one config-level key must be present (the config struct is
	// marshalled directly, so we expect top-level fields from kaiConfig).
	if len(parsed) == 0 {
		t.Errorf("quiet JSON mode: expected config fields in output; got:\n%s", out)
	}
}

// TestRunConfigShow_KaiDirInOutput verifies that the kaiDir path appears in
// both YAML and JSON outputs.
func TestRunConfigShow_KaiDirInOutput(t *testing.T) {
	dir, cleanup := setupConfigShowDir(t)
	defer cleanup()
	clearProviderEnv(t)

	for _, useJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "yaml", true: "json"}[useJSON], func(t *testing.T) {
			oldJSON := configShowJSON
			configShowJSON = useJSON
			defer func() { configShowJSON = oldJSON }()

			out := captureStdout(t, func() {
				if err := runConfigShow(nil, nil); err != nil {
					t.Fatalf("runConfigShow: %v", err)
				}
			})
			if !strings.Contains(out, dir) {
				t.Errorf("expected kai_dir %q in output; got:\n%s", dir, out)
			}
		})
	}
}
