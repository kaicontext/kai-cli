package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kaicontext/kai-engine/message"
	"github.com/kaicontext/kai-engine/provider"
)

func rcTextResponse(s string) provider.Response {
	return provider.Response{Parts: []message.ContentPart{message.TextContent{Text: s}}}
}

func TestDescriptionIsAskedForFromTheChangeAlone(t *testing.T) {
	var got provider.Request
	p := rcChallengeProvider{send: func(_ context.Context, req provider.Request) (provider.Response, error) {
		got = req
		return rcTextResponse("```markdown\nAdds retries to the upload.\n\n#### Changes\n- `upload.go`: retry 3 times\n```"), nil
	}}
	d := rcStartDescription(context.Background(), p, "m", "Retry uploads\n\nThe CDN drops 1% of PUTs.", "diff --git a/upload.go")(time.Second)
	if d != "Adds retries to the upload.\n\n#### Changes\n- `upload.go`: retry 3 times" {
		t.Errorf("description = %q", d)
	}
	user := got.Messages[0].Parts[0].(message.TextContent).Text
	if !strings.Contains(user, "The CDN drops 1% of PUTs.") || !strings.Contains(user, "diff --git a/upload.go") {
		t.Errorf("the call lacks the author's words or the diff:\n%s", user)
	}
	if got.ReasoningEffort != "" || got.MaxTokens < 8000 {
		t.Errorf("description call budget: effort %q, %d tokens — reasoning spends it all", got.ReasoningEffort, got.MaxTokens)
	}
	for _, rule := range []string{"Do not review it", "Never invent", "Never write an @-mention"} {
		if !strings.Contains(got.System, rule) {
			t.Errorf("system prompt lacks %q", rule)
		}
	}
}

func TestDescriptionFailureOrLatenessIsLeftOut(t *testing.T) {
	failing := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		return provider.Response{}, errors.New("upstream 500")
	}}
	if d := rcStartDescription(context.Background(), failing, "m", "x", "y")(time.Second); d != "" {
		t.Errorf("failed call gave %q", d)
	}
	release := make(chan struct{})
	defer close(release)
	slow := rcChallengeProvider{send: func(ctx context.Context, _ provider.Request) (provider.Response, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return rcTextResponse("late"), nil
	}}
	start := time.Now()
	if d := rcStartDescription(context.Background(), slow, "m", "x", "y")(50 * time.Millisecond); d != "" {
		t.Errorf("late description = %q", d)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("collecting a late description waited %s", time.Since(start))
	}
}

func TestCleanDescriptionBounds(t *testing.T) {
	line := "- a line of the change\n"
	d := rcCleanDescription(strings.Repeat(line, 400))
	if len(d) > rcDescriptionMaxBytes {
		t.Fatalf("overlong: %d bytes", len(d))
	}
	if !strings.HasSuffix(d, "- a line of the change") {
		t.Errorf("cut mid-line: ...%q", d[len(d)-30:])
	}
	// No newline to back up to: the cut lands on a character, not inside one.
	one := rcCleanDescription(strings.Repeat("€", rcDescriptionMaxBytes)) // 3 bytes: 4000 is not a boundary
	if !utf8.ValidString(one) || len(one) > rcDescriptionMaxBytes {
		t.Errorf("a one-line description was cut inside a character (%d bytes)", len(one))
	}
}

func TestDescriptionNeverMentions(t *testing.T) {
	d := rcCleanDescription("Pings @alice and @kai; keeps `@Injectable()` and `pkg/@scope/x`.")
	if strings.Contains(d, "@alice") || strings.Contains(d, "@kai") || !strings.Contains(d, "`@Injectable()`") {
		t.Errorf("description = %q", d)
	}
	if d := rcCleanDescription("unpaired ` then @alice"); strings.Contains(d, "@alice") {
		t.Errorf("unpaired backtick left a mention: %q", d)
	}
}

func TestDescriptionCutOffByTheTokenLimitIsLeftOut(t *testing.T) {
	p := rcChallengeProvider{send: func(context.Context, provider.Request) (provider.Response, error) {
		r := rcTextResponse("Adds retries to the upload, and then")
		r.FinishReason = message.FinishReasonMaxTokens
		return r, nil
	}}
	if d := rcStartDescription(context.Background(), p, "m", "x", "y")(time.Second); d != "" {
		t.Errorf("a truncated description was kept: %q", d)
	}
}
