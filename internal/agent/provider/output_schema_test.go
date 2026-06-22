package provider

import (
	"encoding/json"
	"testing"
)

// TestBuildAnthropicRequest_OutputJSONSchemaAttached pins that an
// OutputJSONSchema on the Request flows through to the Anthropic
// wire shape as `output_config.format.{type, schema}`. The 2026-05-12
// planner work switched to structured outputs to guarantee parseable
// JSON instead of fishing it out of a markdown fence; this test
// keeps the wire format honest so a future refactor can't silently
// drop the field.
func TestBuildAnthropicRequest_OutputJSONSchemaAttached(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"summary": map[string]interface{}{"type": "string"},
		},
		"required":             []string{"summary"},
		"additionalProperties": false,
	}
	req := buildAnthropicRequest(Request{
		Model:            "claude-sonnet-4-6",
		Messages:         twoTurnHistory(),
		MaxTokens:        4096,
		OutputJSONSchema: schema,
	})
	oc, ok := req["output_config"].(map[string]interface{})
	if !ok {
		t.Fatalf("output_config missing or wrong type: %T %v", req["output_config"], req["output_config"])
	}
	format, ok := oc["format"].(map[string]interface{})
	if !ok {
		t.Fatalf("output_config.format missing: %v", oc)
	}
	if format["type"] != "json_schema" {
		t.Errorf("expected format.type=json_schema, got %v", format["type"])
	}
	if _, ok := format["schema"].(map[string]interface{}); !ok {
		t.Errorf("expected format.schema to be the passed schema map, got %T", format["schema"])
	}
	// Round-trip through JSON to confirm the wire bytes are what the
	// proxy will forward to Anthropic.
	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundtrip map[string]interface{}
	if err := json.Unmarshal(out, &roundtrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if roundtrip["output_config"] == nil {
		t.Error("output_config dropped during JSON round-trip")
	}
}

// TestBuildAnthropicRequest_NoSchemaNoConfig pins the negative: when
// the planner is NOT using structured outputs (e.g. an older model
// or a non-planner agent), the request must not include an empty
// output_config field — Anthropic would reject that.
func TestBuildAnthropicRequest_NoSchemaNoConfig(t *testing.T) {
	req := buildAnthropicRequest(Request{
		Model:     "claude-sonnet-4-6",
		Messages:  twoTurnHistory(),
		MaxTokens: 4096,
	})
	if _, ok := req["output_config"]; ok {
		t.Errorf("output_config should be absent when no schema set, got %v", req["output_config"])
	}
}

// TestBuildOpenAIRequest_OutputJSONSchemaAttached pins that
// OutputJSONSchema flows through to the OpenAI/Together wire shape
// as `response_format: {type: "json_schema", json_schema: {...}}`.
// Together AI has normalized this across their catalog, so the
// planner can rely on structured output for Qwen/Kimi/DeepSeek the same
// way it does for Anthropic — no more fishing JSON out of fenced
// markdown for non-Anthropic routes.
func TestBuildOpenAIRequest_OutputJSONSchemaAttached(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"summary": map[string]interface{}{"type": "string"},
		},
		"required":             []string{"summary"},
		"additionalProperties": false,
	}
	req := buildOpenAIRequest(Request{
		Model:            "Qwen/Qwen3.5-397B-A17B",
		Messages:         twoTurnHistory(),
		MaxTokens:        4096,
		OutputJSONSchema: schema,
	}, false)
	rf, ok := req["response_format"].(map[string]interface{})
	if !ok {
		t.Fatalf("response_format missing or wrong type: %T %v", req["response_format"], req["response_format"])
	}
	if rf["type"] != "json_schema" {
		t.Errorf("expected response_format.type=json_schema, got %v", rf["type"])
	}
	js, ok := rf["json_schema"].(map[string]interface{})
	if !ok {
		t.Fatalf("response_format.json_schema missing: %v", rf)
	}
	if js["name"] == "" || js["name"] == nil {
		t.Errorf("response_format.json_schema.name must be non-empty (OpenAI requires it)")
	}
	if _, ok := js["schema"].(map[string]interface{}); !ok {
		t.Errorf("response_format.json_schema.schema should be the passed schema map, got %T", js["schema"])
	}
	if js["strict"] != true {
		t.Errorf("strict should be true for structured outputs, got %v", js["strict"])
	}
}

// TestBuildOpenAIRequest_NoSchemaNoResponseFormat pins the negative:
// callers that don't set OutputJSONSchema must not see response_format
// added — Together (and some other providers) 400 on unknown values.
func TestBuildOpenAIRequest_NoSchemaNoResponseFormat(t *testing.T) {
	req := buildOpenAIRequest(Request{
		Model:     "Qwen/Qwen3.5-397B-A17B",
		Messages:  twoTurnHistory(),
		MaxTokens: 4096,
	}, false)
	if _, ok := req["response_format"]; ok {
		t.Errorf("response_format should be absent when no schema set, got %v", req["response_format"])
	}
}
