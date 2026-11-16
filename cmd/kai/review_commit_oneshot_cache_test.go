package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kaicontext/kai-engine/provider"
)

func bodyRecorder(t *testing.T) (*provider.Kailab, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	return provider.NewKailab(srv.URL, "tok"), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func TestOneShotCallsSendNoCacheBreakpoint(t *testing.T) {
	const model = "claude-sonnet-5-5"
	ctx := context.Background()
	patches := map[string]string{}
	var order []string
	for i := 0; i < 3; i++ {
		f := fmt.Sprintf("f%d.go", i)
		order = append(order, f)
		// Big enough to force more than one sweep chunk.
		patches[f] = "@@ -1 +1 @@\n-a\n+" + strings.Repeat("b", rcSweepMaxFileBytes-20) + "\n"
	}
	calls := []struct {
		name string
		call func(p provider.Provider)
	}{
		{"intent", func(p provider.Provider) { _, _ = rcInferIntent(ctx, p, model, "subject", "body", "diff") }},
		{"description", func(p provider.Provider) { _, _ = rcWriteDescription(ctx, p, model, "author", "diff") }},
		{"sweep", func(p provider.Provider) { rcRunSweep(ctx, p, model, "intent", order, patches) }},
		{"sweep2", func(p provider.Provider) { rcRunSweepWith(ctx, p, model, rcStageSweep2, "", order, patches) }},
	}
	for _, on := range []bool{true, false} {
		saved := rcOneShotNoCache
		rcOneShotNoCache = on
		for _, tc := range calls {
			t.Run(fmt.Sprintf("%s/nocache=%v", tc.name, on), func(t *testing.T) {
				p, got := bodyRecorder(t)
				tc.call(p)
				bodies := got()
				if len(bodies) == 0 {
					t.Fatal("no request was made")
				}
				if tc.name == "sweep" && len(bodies) < 2 {
					t.Fatalf("sweep sent %d request(s); the test needs more than one chunk", len(bodies))
				}
				for i, b := range bodies {
					if has := strings.Contains(b, "cache_control"); has == on {
						t.Errorf("request %d: cache_control present = %v, want %v", i, has, !on)
					}
				}
			})
		}
		rcOneShotNoCache = saved
	}
}

func TestOneShotCacheSwitch(t *testing.T) {
	for v, want := range map[string]bool{"": true, "on": false, " ON ": false, "off": true, "1": true} {
		if got := rcOneShotNoCacheFrom(v); got != want {
			t.Errorf("KAI_REVIEW_ONESHOT_CACHE=%q: NoCache = %v, want %v", v, got, want)
		}
	}
}
