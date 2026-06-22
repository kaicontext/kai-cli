package vision

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDescribe_SendsExpectedShape pins the request shape sent to
// kailab so a server-side change can be caught here rather than as
// a mysterious "vision: upstream 400" at runtime. Verifies endpoint
// path, auth header, JSON body shape (model + multipart content
// with text and image_url data: URL).
func TestDescribe_SendsExpectedShape(t *testing.T) {
	var captured struct {
		Path   string
		Auth   string
		Body   map[string]interface{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Path = r.URL.Path
		captured.Auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured.Body)
		// Return a minimal OpenAI-shaped reply.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"a screenshot of an error dialog"}}]}`))
	}))
	defer srv.Close()

	got, err := Describe(context.Background(), srv.URL, "tok_test", "Qwen/Qwen3.5-397B-A17B",
		[]byte{0x89, 0x50, 0x4E, 0x47}, "image/png")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got != "a screenshot of an error dialog" {
		t.Errorf("description mismatch: %q", got)
	}
	if captured.Path != "/api/v1/llm/completions" {
		t.Errorf("endpoint path mismatch: %q", captured.Path)
	}
	if captured.Auth != "Bearer tok_test" {
		t.Errorf("auth header mismatch: %q", captured.Auth)
	}
	if captured.Body["model"] != "Qwen/Qwen3.5-397B-A17B" {
		t.Errorf("model mismatch: %v", captured.Body["model"])
	}
	msgs, ok := captured.Body["messages"].([]interface{})
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages malformed: %v", captured.Body["messages"])
	}
	parts, ok := msgs[0].(map[string]interface{})["content"].([]interface{})
	if !ok || len(parts) != 2 {
		t.Fatalf("content not multipart array: %v", msgs[0])
	}
	if parts[0].(map[string]interface{})["type"] != "text" {
		t.Errorf("first part should be text, got %v", parts[0])
	}
	imagePart := parts[1].(map[string]interface{})
	if imagePart["type"] != "image_url" {
		t.Errorf("second part should be image_url, got %v", imagePart)
	}
	urlMap := imagePart["image_url"].(map[string]interface{})
	url := urlMap["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Errorf("image url not a data: URL: %q", url)
	}
}

// TestDescribe_PropagatesUpstreamError surfaces the upstream
// response body in the error so the user can see "model not found"
// rather than a generic "vision failed."
func TestDescribe_PropagatesUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":{"message":"model not found: foo/bar"}}`))
	}))
	defer srv.Close()
	_, err := Describe(context.Background(), srv.URL, "tok", "", []byte{0x00}, "image/png")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Errorf("expected upstream message in error, got: %v", err)
	}
}

// TestDescribe_RequiresConfig — caller invariants.
func TestDescribe_RequiresConfig(t *testing.T) {
	ctx := context.Background()
	if _, err := Describe(ctx, "", "tok", "", []byte{0x00}, ""); err == nil {
		t.Error("empty baseURL should fail")
	}
	if _, err := Describe(ctx, "http://x", "", "", []byte{0x00}, ""); err == nil {
		t.Error("empty token should fail")
	}
	if _, err := Describe(ctx, "http://x", "tok", "", nil, ""); err == nil {
		t.Error("empty image should fail")
	}
}

// TestDetectMIME covers the supported image extensions + an
// unsupported case. The slash handler in repl.go uses this as the
// pre-attach validator; unsupported types must return "".
func TestDetectMIME(t *testing.T) {
	cases := map[string]string{
		"foo.png":         "image/png",
		"FOO.PNG":         "image/png",
		"shot.jpg":        "image/jpeg",
		"shot.jpeg":       "image/jpeg",
		"frame.webp":      "image/webp",
		"anim.gif":        "image/gif",
		"doc.pdf":         "",
		"notes.txt":       "",
		"no-extension":    "",
		"weird.png.bak":   "",
	}
	for in, want := range cases {
		if got := DetectMIME(in); got != want {
			t.Errorf("DetectMIME(%q) = %q, want %q", in, got, want)
		}
	}
}
