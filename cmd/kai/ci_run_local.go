package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"kai/internal/cirun"
)

var (
	ciRunAll          bool
	ciRunEvent        string
	ciRunTree         string
	ciRunBase         string
	ciRunImage        string
	ciRunArch         string
	ciRunMatrix       []string
	ciRunStrict       bool
	ciRunDockerSocket bool
	ciRunActPath      string
	ciRunJSON         bool
	ciRunVerbose      bool
	ciRunTimeout      time.Duration
)

var ciRunCmd = &cobra.Command{
	Use:   "run [workflow.yml:job | job]...",
	Short: "Run the repo's GitHub Actions jobs on this machine",
	Long: `Runs jobs from .github/workflows locally, in containers, before you push.

The runner is the open-source nektos/act. Kai downloads a pinned, checksummed
copy to ~/.kai/tools/act the first time it's needed (an act on PATH or
--act-path is used instead). A container runtime is required: Docker Desktop,
OrbStack, Colima or Podman.

Before anything runs, each job is checked for what can't be reproduced here.
A job that can't run on this machine (macOS or Windows runners, OIDC tokens,
protected environments) is reported as "not run". It is never shown as passing.

Exit codes:
  0  everything that ran passed
  1  a job failed, or with --strict, a job could not run here

By default the working tree is tested, uncommitted changes included. With
--json, or when KAI_AGENT is set, a clean checkout of HEAD is tested instead so
the result belongs to a commit. --tree overrides either.

No secrets and no GitHub token are passed to jobs, and the host's Docker
socket is not mounted into them unless you pass --docker-socket.

Examples:
  kai ci run ci.yml:test         # one job (and the jobs it needs)
  kai ci run test                # same, when only one workflow has a "test" job
  kai ci run --all               # every job a push of this branch would trigger
  kai ci run --all --event pull_request
  kai ci run build --matrix os=ubuntu-latest,go=1.25

A run number or ID still shows that remote run, as before:
  kai ci run 110`,
	RunE: runCIRunLocal,
}

func init() {
	f := ciRunCmd.Flags()
	f.BoolVar(&ciRunAll, "all", false, "Run every job the event would trigger")
	f.StringVar(&ciRunEvent, "event", "push", "Synthetic event: push or pull_request")
	f.StringVar(&ciRunTree, "tree", "", "Code to test: working (uncommitted changes included) or head (clean checkout of HEAD)")
	f.StringVar(&ciRunBase, "base", "", "Base ref for the event and changed files (default: origin/HEAD, then main, then master)")
	f.StringVar(&ciRunImage, "image", "medium", "Runner image for ubuntu-* jobs: medium, full, or an image reference")
	f.StringVar(&ciRunArch, "arch", "", "Container architecture, e.g. linux/amd64 (default: this machine's)")
	f.StringArrayVar(&ciRunMatrix, "matrix", nil, "Run only matching matrix entries, e.g. os=ubuntu-latest,go=1.25")
	f.BoolVar(&ciRunStrict, "strict", false, "Exit 1 if any picked job could not run here")
	f.BoolVar(&ciRunDockerSocket, "docker-socket", false, "Mount the host's Docker socket into job containers (for jobs that run Docker themselves)")
	f.StringVar(&ciRunActPath, "act-path", "", "Path to an act binary to use instead of the managed one")
	f.BoolVar(&ciRunJSON, "json", false, "Print the report as JSON on stdout")
	f.BoolVarP(&ciRunVerbose, "verbose", "v", false, "Show step output as it runs")
	f.DurationVar(&ciRunTimeout, "timeout", 60*time.Minute, "Give up on a job after this long")
}

// remoteRunRefRE matches what the remote `kai ci run <id>` took: a run number
// or a UUID. Job IDs that look like that need the workflow.yml:job form.
var remoteRunRefRE = regexp.MustCompile(`^(\d+|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})$`)

func runCIRunLocal(cmd *cobra.Command, args []string) error {
	if len(args) == 1 && !ciRunAll && remoteRunRefRE.MatchString(args[0]) {
		return runCIRun(cmd, args)
	}
	if ciRunAll && len(args) > 0 {
		return fmt.Errorf("pass job names or --all, not both")
	}

	tree := ciRunTree
	if tree == "" {
		tree = "working"
		if ciRunJSON || os.Getenv("KAI_AGENT") != "" {
			tree = "head"
		}
	}
	progress := os.Stdout
	if ciRunJSON {
		progress = os.Stderr
	}
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt)
	defer stop()

	rep, err := cirun.Run(ctx, cirun.Options{
		Dir:          ".",
		Targets:      args,
		All:          ciRunAll,
		Event:        ciRunEvent,
		Tree:         tree,
		Base:         ciRunBase,
		Image:        ciRunImage,
		Arch:         ciRunArch,
		Matrix:       ciRunMatrix,
		Strict:       ciRunStrict,
		DockerSocket: ciRunDockerSocket,
		Verbose:      ciRunVerbose,
		ActPath:      ciRunActPath,
		Timeout:      ciRunTimeout,
		Progress:     progress,
	})
	if errors.Is(err, cirun.ErrNoSelection) {
		// Picking jobs from `kai ci plan` comes next; until then, say what
		// there is to pick from.
		if jobs := ciRunJobRefs(); len(jobs) > 0 {
			fmt.Fprintf(os.Stderr, "Jobs here: %s\n", strings.Join(jobs, ", "))
		}
		return fmt.Errorf("name a job or pass --all (picking jobs from the plan isn't built yet)")
	}
	if rep == nil {
		return err
	}
	if ciRunJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(rep); encErr != nil {
			return encErr
		}
	} else {
		fmt.Println()
		rep.WriteSummary(os.Stdout)
	}
	if err != nil {
		return err
	}
	if ciRunAll && ciRunEvent == "push" && !ciRunJSON && ciRunNothingTriggered(rep) {
		fmt.Println("No workflow is triggered by a push of this branch. Try --event pull_request, or name a job.")
	}
	if rep.ExitCode != 0 {
		os.Exit(rep.ExitCode)
	}
	return nil
}

func ciRunNothingTriggered(rep *cirun.Report) bool {
	for _, j := range rep.Jobs {
		if j.Status != cirun.StatusSkipped {
			return false
		}
	}
	return true
}

func ciRunJobRefs() []string {
	wfs, err := cirun.LoadWorkflows(".")
	if err != nil {
		return nil
	}
	var refs []string
	for _, wf := range wfs {
		for _, j := range wf.Jobs {
			refs = append(refs, j.Ref())
		}
	}
	sort.Strings(refs)
	return refs
}

// checkCIRunner is the `kai doctor` check for what `kai ci run` needs.
func checkCIRunner(ok, warn string) {
	if rt, err := cirun.DetectRuntime(); err == nil {
		fmt.Printf("%s container runtime for 'kai ci run': %s\n", ok, rt.Name)
	} else {
		fmt.Printf("%s 'kai ci run': %v\n", warn, err)
	}
}
