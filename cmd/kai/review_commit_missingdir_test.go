package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The missing-directory case, preserved from the deep-path capture (attempt 3
// at revision 9a4a1ff, GLM-5.2 drafting and challenging). The change's
// requirement is "cd into the workspace before running the command … so the
// command always runs in the correct directory". The draft alleged that
// `cd <path> && <command>` "silently aborts the command when the workspace
// directory is missing"; the challenger ran the exact construction on
// /bad/missing/path and observed exit 2, empty stdout and
// `cd: can't cd to /bad/missing/path: No such file or directory` on stderr —
// the requirement's own behavior for that case, and not silent — then declared
// "the command runs anyway" (exit 0, COMMAND_RAN) the INTENDED behavior, and
// the validator published the allegation as a confirmed defect with a remedy
// (`cd … ; <command>`) that would run the command in whatever directory the
// persistent shell was in.
//
// Acceptance: stopping after a failed cd is not published as a confirmed
// defect, and a remedy that runs the command in the wrong directory is not
// published as a correction. The fixture is the challenger's exact inputs
// (issues, all 18 sources, the four experiment records) and its exact
// submission; the test adds nothing.
type rcDeepCase struct {
	Requirement struct {
		Stated        string   `json:"stated"`
		AuthorComment []string `json:"authorComment"`
	} `json:"requirement"`
	Issues          []string                       `json:"issues"`
	Decisions       []string                       `json:"decisions"`
	Sources         []string                       `json:"sources"`
	ExperimentImage string                         `json:"experimentImage"`
	Experiments     map[string]*rcExperimentRecord `json:"experiments"`
	Submitted       string                         `json:"submitted"`
}

func rcLoadMissingDirCase(t *testing.T) (rcDeepCase, map[int]*rcExperimentRecord) {
	t.Helper()
	raw, err := os.ReadFile("testdata/pr429/deep-attempt3-missing-dir.json")
	if err != nil {
		t.Fatal(err)
	}
	var c rcDeepCase
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	exps := map[int]*rcExperimentRecord{}
	for k, rec := range c.Experiments {
		n, err := strconv.Atoi(k)
		if err != nil {
			t.Fatalf("experiment key %q: %v", k, err)
		}
		exps[n] = rec
	}
	return c, exps
}

// The fixture is the run we think it is, and the experiment sources the
// challenger read are byte-for-byte what render() produces from the recorded
// records — so a citation into them points at the same observed output.
func TestMissingDirCaseFixtureIsTheOneWeThink(t *testing.T) {
	c, exps := rcLoadMissingDirCase(t)
	if c.Requirement.Stated != "play button: cd into the workspace before running the command" {
		t.Fatalf("requirement: %q", c.Requirement.Stated)
	}
	if len(c.Issues) != 3 || !strings.Contains(c.Issues[0], "silently aborts the command when the workspace directory is missing") {
		t.Fatalf("issues: %q", c.Issues)
	}
	if len(c.Sources) != 18 {
		t.Fatalf("sources: %d", len(c.Sources))
	}
	rec := exps[18]
	if rec == nil || rec.GeneratedCommand != `cd "/bad/missing/path" && echo COMMAND_RAN` {
		t.Fatalf("experiment 18: %+v", rec)
	}
	if rec.ExitCode != 2 || rec.ObservedPWD != "/tmp" || rec.Stdout != "" {
		t.Fatalf("experiment 18 observed exit=%d pwd=%q stdout=%q", rec.ExitCode, rec.ObservedPWD, rec.Stdout)
	}
	if !strings.Contains(rec.Stderr, "can't cd to /bad/missing/path") {
		t.Fatalf("experiment 18 stderr does not carry the shell's cd error: %q", rec.Stderr)
	}
	for n := 15; n <= 18; n++ {
		// The prompt numbers a source's lines after trimming its trailing
		// newline; compare what the challenger saw against render() the same way.
		if got, want := strings.TrimRight(exps[n].render(c.ExperimentImage), "\n"), c.Sources[n-1]; got != want {
			gl, wl := strings.Split(got, "\n"), strings.Split(want, "\n")
			for i := 0; i < len(gl) || i < len(wl); i++ {
				var g, w string
				if i < len(gl) {
					g = gl[i]
				}
				if i < len(wl) {
					w = wl[i]
				}
				if g != w {
					t.Fatalf("source %d as the challenger saw it differs from render() of the recorded experiment at line %d (%d vs %d lines):\n saw:    %q\n render: %q", n, i+1, len(wl), len(gl), w, g)
				}
			}
			t.Fatalf("source %d differs from render() only in length: %d vs %d bytes", n, len(want), len(got))
		}
	}
	// The submission we replay is the one that supported the allegation and
	// offered the wrong-directory remedy.
	var ans rcChallengeAnswer
	if err := json.Unmarshal([]byte(c.Submitted), &ans); err != nil {
		t.Fatal(err)
	}
	var check *rcIssueCheck
	for i := range ans.Checks {
		if ans.Checks[i].Issue == c.Issues[0] {
			check = &ans.Checks[i]
		}
	}
	if check == nil || check.Verdict != "supported" || !strings.Contains(check.Remedy, "`cd ... ; <command>`") {
		t.Fatalf("submitted check for the allegation: %+v", check)
	}
	var ev *rcCheckEvidence
	for i := range check.Evidence {
		if check.Evidence[i].Source == 18 {
			ev = &check.Evidence[i]
		}
	}
	if ev == nil || ev.Expectation != "intended" {
		t.Fatalf("the citation of experiment 18: %+v", ev)
	}
	// "Intended" as the challenger declared it: exit 0 and COMMAND_RAN on a
	// missing directory — the command running anyway.
	for _, idx := range ev.Assertions {
		a := rec.Assertions[idx-1]
		if !(a.Kind == "exit" && a.Value == "0") && !(a.Kind == "stdout_contains" && a.Value == "COMMAND_RAN") {
			t.Fatalf("offered assertion %d is %s %q", idx, a.Kind, a.Value)
		}
	}
}

// Acceptance: replaying the challenger's exact submission over its exact
// inputs must not publish "stopping after a failed cd" as a confirmed defect,
// and must not publish the `cd … ; <command>` remedy as a correction.
func TestStoppingAfterFailedCdIsNotPublishedAsADefect(t *testing.T) {
	c, exps := rcLoadMissingDirCase(t)
	res, err := rcValidateChallenge(c.Submitted, c.Issues, c.Decisions, c.Sources, exps)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	var a *rcAllegationResult
	for i := range res.Allegations {
		if res.Allegations[i].Issue == c.Issues[0] {
			a = &res.Allegations[i]
		}
	}
	if a == nil {
		t.Fatal("allegation missing from the result")
	}
	if a.Status == "supported" {
		t.Errorf("stopping after a failed cd (exit 2, `cd: can't cd to /bad/missing/path` on stderr) was published as a confirmed defect; the requirement is %q", c.Requirement.Stated)
	}
	if a.Remedy != "" {
		t.Errorf("a remedy that runs the command in the wrong directory was published as a correction: %q", a.Remedy)
	}
	if strings.Contains(res.Review, "`cd ... ; <command>`") {
		t.Errorf("the assembled review carries the wrong-directory remedy")
	}
}

// The same replay must keep the correct verdict: the quoting defect
// (allegation 2) was observed on the alleged inputs and stays supported.
func TestMissingDirCaseKeepsTheQuotingVerdict(t *testing.T) {
	c, exps := rcLoadMissingDirCase(t)
	res, err := rcValidateChallenge(c.Submitted, c.Issues, c.Decisions, c.Sources, exps)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, a := range res.Allegations {
		if a.Issue == c.Issues[1] {
			if a.Status != "supported" || a.Remedy == "" {
				t.Fatalf("quoting allegation: status=%s remedy=%q", a.Status, a.Remedy)
			}
			return
		}
	}
	t.Fatal("quoting allegation missing")
}
