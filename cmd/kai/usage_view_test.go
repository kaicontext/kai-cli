package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/kaicontext/kai-engine/remote"
)

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

func usageServer(t *testing.T, status int, body string) *remote.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/billing/usage" || r.URL.Query().Get("org") != "acme co" {
			t.Errorf("request %s", r.URL)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &remote.Client{BaseURL: srv.URL, AuthToken: "tok", HTTPClient: srv.Client()}
}

func renderUsage(t *testing.T, c *remote.Client) string {
	t.Helper()
	u, err := fetchUsage(c, "acme co")
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	printUsage(&b, "acme", u)
	return ansi.ReplaceAllString(b.String(), "")
}

// An older server sends none of the new fields: the output is what it was,
// except that a paid plan other than Pro now shows its seats, named by plan.
func TestUsageAgainstAnOlderServer(t *testing.T) {
	c := usageServer(t, 200, `{"tier":"max","seats_purchased":3,"seats_assigned":2,"has_seat":true,
		"daily_cap_cents":6666,"daily_cost_cents":100,"monthly_cap_cents":30000,"monthly_cost_cents":3000,"upgrade_url":null}`)
	out := renderUsage(t, c)
	for _, want := range []string{
		"  Org:     acme\n",
		"  Plan:    max\n",
		"  Seats:   2 / 3 assigned\n",
		"(Max seat, resets midnight UTC)",
		"  Month:   $30.00 / $300.00",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, not := range []string{"Reviews this month", "Free:", "Split:", "Pro seat"} {
		if strings.Contains(out, not) {
			t.Errorf("unexpected %q in:\n%s", not, out)
		}
	}
}

func TestUsageFreePlanHasNoSeatsLine(t *testing.T) {
	c := usageServer(t, 200, `{"tier":"free","has_seat":false,"daily_cap_cents":300,"monthly_cap_cents":500}`)
	out := renderUsage(t, c)
	if strings.Contains(out, "Seats:") {
		t.Errorf("seats line on Free:\n%s", out)
	}
	if !strings.Contains(out, "(free limit, resets midnight UTC)") {
		t.Errorf("no free limit note:\n%s", out)
	}
}

func TestUsageShowsReviewBreakdown(t *testing.T) {
	c := usageServer(t, 200, `{"tier":"pro","seats_purchased":3,"seats_assigned":3,"has_seat":true,
		"daily_cap_cents":3333,"monthly_cap_cents":15000,"monthly_cost_cents":5000,
		"review_usage":{"allowance":"org",
			"by_tier":[{"tier":"standard","name":"Standard","share_pct":30,"reviews":12},{"tier":"light","name":"Light","share_pct":2.5,"reviews":1}],
			"by_repo":[{"repo":"docs","share_pct":30,"reviews":12},{"repo":"app","share_pct":0.04,"reviews":1}],
			"paid_from":[{"source":"org_plan","reviews":13},{"source":"org_credit","reviews":2}]}}`)
	out := renderUsage(t, c)
	for _, want := range []string{
		"  Reviews this month (share of the org's shared budget):\n",
		"    Standard        30%  12 reviews\n",
		"    Light          2.5%  1 review\n",
		"  Top repos:\n",
		"    docs                        30%  12 reviews\n",
		"    app                       <0.1%  1 review\n",
		"  2 reviews were paid from credit and not counted here\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "$") != 4 {
		t.Errorf("the breakdown shows money:\n%s", out)
	}
}

func TestUsageNoReviewsYet(t *testing.T) {
	c := usageServer(t, 200, `{"tier":"free","review_usage":{"allowance":"billing_person","by_tier":[],"by_repo":[]}}`)
	if out := renderUsage(t, c); !strings.Contains(out, "  Reviews this month: none on the allowance yet\n") {
		t.Errorf("no empty line:\n%s", out)
	}
}

func TestUsageFreeOnGoogle(t *testing.T) {
	c := usageServer(t, 200, `{"tier":"free","monthly_cap_cents":500,"monthly_cost_cents":200,
		"free_reviews":{"label":"Free (Google models)","reviews_share_pct":12,"own_use_share_pct":28,"prs_left":15,"billed_to_you":true}}`)
	out := renderUsage(t, c)
	for _, want := range []string{
		"  Plan:    Free (Google models)\n",
		"  Free:    about 15 pull requests left this month\n",
		"  Split:   reviews of acme 12% · your own use 28%\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	// No count when the server can't yet say; someone else's allowance.
	c = usageServer(t, 200, `{"tier":"free","free_reviews":{"label":"Free (Google models)","reviews_share_pct":0,"own_use_share_pct":0,"billed_to_you":false}}`)
	out = renderUsage(t, c)
	if strings.Contains(out, "pull requests left") || !strings.Contains(out, "the billing person's own use 0%") {
		t.Errorf("free lines:\n%s", out)
	}
}

func TestFetchUsageErrors(t *testing.T) {
	c := usageServer(t, 404, `{"error":"org not found"}`)
	if _, err := fetchUsage(c, "acme co"); err == nil || err.Error() != "org not found" {
		t.Errorf("err = %v", err)
	}
	c = usageServer(t, 502, `bad gateway`)
	if _, err := fetchUsage(c, "acme co"); err == nil || err.Error() != "server error: 502 bad gateway" {
		t.Errorf("err = %v", err)
	}
}

func TestFmtShare(t *testing.T) {
	for in, want := range map[float64]string{0: "0%", 0.04: "<0.1%", 0.1: "0.1%", 2.5: "2.5%", 9.94: "9.9%", 10: "10%", 30: "30%", 100: "100%"} {
		if got := fmtShare(in); got != want {
			t.Errorf("fmtShare(%v) = %q, want %q", in, got, want)
		}
	}
}
