// Vision-to-text via kailab's Together-compatible chat-completions
// endpoint. One-shot helper: takes image bytes, returns a prose
// description suitable for embedding in a downstream text-only
// model's prompt.
//
// Design intent: the kai TUI's main inference path runs on text-only
// models (Deepseek-V4-Pro, GLM-5.1) which can't process images
// directly. When the user attaches an image, kai sends it here, gets
// a description back, and rebuilds the prompt as:
//
//	<original user request>
//
//	Images used in prompt:
//	image 1: <description from vision model>
//	image 2: <description>
//	...
//
// Self-contained HTTP client (no dependency on the agent's provider
// abstraction) — simpler than threading a new content-type through
// the agent loop, and the vision call is structurally a one-shot:
// no streaming, no tool calls, no session continuity. Same pattern
// kai_web_search.go uses for its Brave proxy call.
package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultModel is the vision-capable Together model used when the
// caller doesn't override. Picked from kailab's KailabOpenRouterModels
// allowlist; Qwen3.5 is a native vision-language model (image-in).
// Override per-session via KAI_VISION_MODEL.
const DefaultModel = "qwen/qwen3.5-397b-a17b"

// HTTPTimeout caps the vision round-trip. 45s leaves headroom for
// the larger Qwen / Kimi multimodal models on a cold cache without
// dragging out failed calls when the upstream is wedged.
const HTTPTimeout = 45 * time.Second

// describePrompt is the instruction sent alongside each image. The
// downstream model is text-only, so the description IS the only
// thing it'll see — push the vision model toward concrete content
// extraction (file names, error messages, button labels, code) over
// generic "screenshot of an app" framing.
const describePrompt = `Describe this image in 3-5 sentences. Be specific and concrete. Include any visible text verbatim (file paths, error messages, code snippets, button labels, log lines, CLI output). For UI screenshots, describe the layout and what's interactive. For code or text content, transcribe the relevant parts exactly. The downstream model that reads your description is text-only and cannot see the image — your description is its only source of truth about what's in the image.`

// Describe posts one image to the kailab-proxied Together vision
// model and returns the model's description. Caller supplies kailab
// auth (baseURL+token, same source as the main agent's provider),
// an optional model override (empty → DefaultModel), the image
// bytes, and the MIME type ("image/png", "image/jpeg", etc.).
//
// Errors propagate the upstream message verbatim where possible so
// the user sees "model not found: ..." or "401 unauthorized" rather
// than a generic "vision failed."
func Describe(ctx context.Context, baseURL, token, model string, imageBytes []byte, mimeType string) (string, error) {
	if baseURL == "" {
		return "", fmt.Errorf("vision: kailab baseURL not configured")
	}
	if token == "" {
		return "", fmt.Errorf("vision: not logged in (run `kai auth login`)")
	}
	if len(imageBytes) == 0 {
		return "", fmt.Errorf("vision: empty image")
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	if model == "" {
		model = DefaultModel
	}

	// Build the OpenAI-shaped chat-completions body with
	// multipart user content. Together's vision API accepts this
	// shape directly via kailab's /api/v1/llm/completions proxy.
	dataURL := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(imageBytes)
	body := map[string]interface{}{
		"model": model,
		"messages": []map[string]interface{}{
			{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": describePrompt},
					{
						"type":      "image_url",
						"image_url": map[string]interface{}{"url": dataURL},
					},
				},
			},
		},
		"max_tokens": 600,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("vision: marshal request: %w", err)
	}

	endpoint := strings.TrimRight(baseURL, "/") + "/api/v1/llm/completions"
	reqCtx, cancel := context.WithTimeout(ctx, HTTPTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, "POST", endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("vision: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: HTTPTimeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("vision: http: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Surface the upstream error message — "model not found",
		// "image too large", auth failures, etc.
		return "", fmt.Errorf("vision: upstream %d: %s", resp.StatusCode, truncate(string(respBody), 500))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("vision: parse response: %w (body: %s)", err, truncate(string(respBody), 300))
	}
	if parsed.Error.Message != "" {
		return "", fmt.Errorf("vision: upstream: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("vision: no choices in response (body: %s)", truncate(string(respBody), 300))
	}
	desc := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if desc == "" {
		return "", fmt.Errorf("vision: empty description from model")
	}
	return desc, nil
}

// DetectMIME maps common image extensions to MIME types. Returns
// "" for unsupported extensions so callers can reject the file
// before the upstream call.
func DetectMIME(path string) string {
	p := strings.ToLower(path)
	switch {
	case strings.HasSuffix(p, ".png"):
		return "image/png"
	case strings.HasSuffix(p, ".jpg"), strings.HasSuffix(p, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(p, ".webp"):
		return "image/webp"
	case strings.HasSuffix(p, ".gif"):
		return "image/gif"
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
