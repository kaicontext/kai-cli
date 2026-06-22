package util

import (
	"encoding/json"
	"testing"
)

func TestCanonicalJSON(t *testing.T) {
	// Test that keys are sorted
	input := map[string]interface{}{
		"zebra": 1,
		"apple": 2,
		"mango": 3,
	}

	result, err := CanonicalJSON(input)
	if err != nil {
		t.Fatalf("CanonicalJSON failed: %v", err)
	}

	expected := `{"apple":2,"mango":3,"zebra":1}`
	if string(result) != expected {
		t.Errorf("Expected %s, got %s", expected, string(result))
	}
}

func TestCanonicalJSON_Nested(t *testing.T) {
	input := map[string]interface{}{
		"outer": map[string]interface{}{
			"z": 1,
			"a": 2,
		},
		"array": []interface{}{3, 2, 1},
	}

	result, err := CanonicalJSON(input)
	if err != nil {
		t.Fatalf("CanonicalJSON failed: %v", err)
	}

	// Parse result to verify structure
	var parsed map[string]interface{}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Failed to parse result: %v", err)
	}

	// Verify the result is valid JSON
	if parsed["outer"] == nil || parsed["array"] == nil {
		t.Error("Missing expected keys in result")
	}
}

func TestBlake3Hash(t *testing.T) {
	data := []byte("hello world")
	hash := Blake3Hash(data)

	// Hash should be 32 bytes
	if len(hash) != 32 {
		t.Errorf("Expected 32 byte hash, got %d bytes", len(hash))
	}

	// Same input should produce same hash
	hash2 := Blake3Hash(data)
	if string(hash) != string(hash2) {
		t.Error("Same input produced different hashes")
	}
}

func TestBlake3HashHex(t *testing.T) {
	data := []byte("hello world")
	hex := Blake3HashHex(data)

	// Hex should be 64 characters (32 bytes * 2)
	if len(hex) != 64 {
		t.Errorf("Expected 64 character hex, got %d characters", len(hex))
	}
}

func TestNodeID(t *testing.T) {
	payload := map[string]interface{}{
		"name": "test",
		"kind": "function",
	}

	id1, err := NodeID("Symbol", payload)
	if err != nil {
		t.Fatalf("NodeID failed: %v", err)
	}

	// Same input should produce same ID
	id2, err := NodeID("Symbol", payload)
	if err != nil {
		t.Fatalf("NodeID failed: %v", err)
	}

	if string(id1) != string(id2) {
		t.Error("Same input produced different IDs (not idempotent)")
	}

	// Different kind should produce different ID
	id3, err := NodeID("File", payload)
	if err != nil {
		t.Fatalf("NodeID failed: %v", err)
	}

	if string(id1) == string(id3) {
		t.Error("Different kinds produced same ID")
	}
}

func TestHexConversion(t *testing.T) {
	original := []byte{0x01, 0x02, 0x03, 0xAB, 0xCD, 0xEF}
	hex := BytesToHex(original)

	bytes, err := HexToBytes(hex)
	if err != nil {
		t.Fatalf("HexToBytes failed: %v", err)
	}

	if string(original) != string(bytes) {
		t.Error("Hex round-trip failed")
	}
}
