package main

import (
	"strings"
	"testing"
)

func TestNewCacheKeyIsPerConversation(t *testing.T) {
	a, b := rcNewCacheKey(rcStageFinder2), rcNewCacheKey(rcStageFinder2)
	if a == "" || a == b {
		t.Fatalf("keys must be non-empty and differ per conversation: %q %q", a, b)
	}
	if !strings.HasPrefix(a, "kai-review-"+rcStageFinder2+"-") {
		t.Errorf("key %q does not name its stage", a)
	}
}
