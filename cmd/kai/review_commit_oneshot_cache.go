package main

import (
	"os"
	"strings"
)

// One-shot prompts are never re-sent, so a cache breakpoint is an unread 1.25x write; KAI_REVIEW_ONESHOT_CACHE=on restores it.
var rcOneShotNoCache = rcOneShotNoCacheFrom(os.Getenv("KAI_REVIEW_ONESHOT_CACHE"))

func rcOneShotNoCacheFrom(v string) bool {
	return !strings.EqualFold(strings.TrimSpace(v), "on")
}
