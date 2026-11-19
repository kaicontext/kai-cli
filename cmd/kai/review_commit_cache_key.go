package main

import (
	"crypto/rand"
	"encoding/hex"
)

// rcNewCacheKey names one review conversation for OpenAI's prompt cache, so
// its turns share a cache rather than only the prefix every review shares.
// A resumed session (the coverage gate, a retry) keeps its conversation's key.
func rcNewCacheKey(stage string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return "kai-review-" + stage + "-" + hex.EncodeToString(b)
}
