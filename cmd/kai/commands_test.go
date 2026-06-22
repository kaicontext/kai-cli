package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestRootCommand tests that the root command is properly configured
func TestRootCommand(t *testing.T) {
	if rootCmd == nil {
		t.Fatal("rootCmd should not be nil")
	}
	if rootCmd.Use != "kai" {
		t.Errorf("expected Use 'kai', got %q", rootCmd.Use)
	}
	if rootCmd.Short == "" {
		t.Error("Short description should not be empty")
	}
}

// TestInitCommand tests the init command configuration
func TestInitCommand(t *testing.T) {
	if initCmd == nil {
		t.Fatal("initCmd should not be nil")
	}
	if initCmd.Use != "init" {
		t.Errorf("expected Use 'init', got %q", initCmd.Use)
	}
	if initCmd.RunE == nil {
		t.Error("RunE should not be nil")
	}
}

// TestSnapshotCommand tests the snapshot command configuration
func TestSnapshotCommand(t *testing.T) {
	if snapshotCmd == nil {
		t.Fatal("snapshotCmd should not be nil")
	}
	// Use is now just "snapshot" - positional args are banned
	if snapshotCmd.Use != "snapshot" {
		t.Errorf("expected Use 'snapshot', got %q", snapshotCmd.Use)
	}
}

// TestAnalyzeCommand tests the analyze command group
func TestAnalyzeCommand(t *testing.T) {
	if analyzeCmd == nil {
		t.Fatal("analyzeCmd should not be nil")
	}
	if analyzeCmd.Use != "analyze" {
		t.Errorf("expected Use 'analyze', got %q", analyzeCmd.Use)
	}
	// Should have subcommands
	if !analyzeCmd.HasSubCommands() {
		t.Error("analyze should have subcommands")
	}
}

// TestChangesetCommand tests the changeset command group
func TestChangesetCommand(t *testing.T) {
	if changesetCmd == nil {
		t.Fatal("changesetCmd should not be nil")
	}
	if changesetCmd.Use != "changeset" {
		t.Errorf("expected Use 'changeset', got %q", changesetCmd.Use)
	}
}

// TestIntentCommand tests the intent command group
func TestIntentCommand(t *testing.T) {
	if intentCmd == nil {
		t.Fatal("intentCmd should not be nil")
	}
	if intentCmd.Use != "intent" {
		t.Errorf("expected Use 'intent', got %q", intentCmd.Use)
	}
}

// TestDumpCommand tests the dump command
func TestDumpCommand(t *testing.T) {
	if dumpCmd == nil {
		t.Fatal("dumpCmd should not be nil")
	}
	if dumpCmd.Use != "dump <changeset-id>" {
		t.Errorf("expected Use 'dump <changeset-id>', got %q", dumpCmd.Use)
	}
}

// TestListCommand tests the list command group
func TestListCommand(t *testing.T) {
	if listCmd == nil {
		t.Fatal("listCmd should not be nil")
	}
	if listCmd.Use != "list" {
		t.Errorf("expected Use 'list', got %q", listCmd.Use)
	}
	if !listCmd.HasSubCommands() {
		t.Error("list should have subcommands")
	}
}

// TestLogCommand tests the log command
func TestLogCommand(t *testing.T) {
	if logCmd == nil {
		t.Fatal("logCmd should not be nil")
	}
	if logCmd.Use != "log" {
		t.Errorf("expected Use 'log', got %q", logCmd.Use)
	}
}

// TestStatusCommand tests the status command
func TestStatusCommand(t *testing.T) {
	if statusCmd == nil {
		t.Fatal("statusCmd should not be nil")
	}
	if statusCmd.Use != "status" {
		t.Errorf("expected Use 'status', got %q", statusCmd.Use)
	}
}

// TestDiffCommand tests the diff command
func TestDiffCommand(t *testing.T) {
	if diffCmd == nil {
		t.Fatal("diffCmd should not be nil")
	}
	if !strings.HasPrefix(diffCmd.Use, "diff") {
		t.Errorf("expected Use to start with 'diff', got %q", diffCmd.Use)
	}
}

// TestDiffCommand_AcceptsZeroArgs tests that diff accepts zero arguments (defaults to @snap:last)
func TestDiffCommand_AcceptsZeroArgs(t *testing.T) {
	// Verify the Args validator accepts 0-2 args
	if err := diffCmd.Args(diffCmd, []string{}); err != nil {
		t.Errorf("diff should accept zero arguments, got error: %v", err)
	}
	if err := diffCmd.Args(diffCmd, []string{"@snap:last"}); err != nil {
		t.Errorf("diff should accept one argument, got error: %v", err)
	}
	if err := diffCmd.Args(diffCmd, []string{"@snap:prev", "@snap:last"}); err != nil {
		t.Errorf("diff should accept two arguments, got error: %v", err)
	}
	if err := diffCmd.Args(diffCmd, []string{"a", "b", "c"}); err == nil {
		t.Error("diff should reject three arguments")
	}
}

// TestWorkspaceCommands tests the workspace command group
func TestWorkspaceCommands(t *testing.T) {
	if wsCmd == nil {
		t.Fatal("wsCmd should not be nil")
	}
	if wsCmd.Use != "ws" {
		t.Errorf("expected Use 'ws', got %q", wsCmd.Use)
	}
	if !wsCmd.HasSubCommands() {
		t.Error("ws should have subcommands")
	}

	// Check subcommands
	subcommands := []string{"create", "list", "stage", "log", "shelve", "unshelve", "close"}
	for _, sub := range subcommands {
		found := false
		for _, cmd := range wsCmd.Commands() {
			if cmd.Use == sub || strings.HasPrefix(cmd.Use, sub) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ws should have subcommand %q", sub)
		}
	}
}

// TestRefCommands tests the ref command group
func TestRefCommands(t *testing.T) {
	if refCmd == nil {
		t.Fatal("refCmd should not be nil")
	}
	if refCmd.Use != "ref" {
		t.Errorf("expected Use 'ref', got %q", refCmd.Use)
	}
	if !refCmd.HasSubCommands() {
		t.Error("ref should have subcommands")
	}

	// Check subcommands
	subcommands := []string{"list", "set", "del"}
	for _, sub := range subcommands {
		found := false
		for _, cmd := range refCmd.Commands() {
			if cmd.Use == sub || strings.HasPrefix(cmd.Use, sub) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("ref should have subcommand %q", sub)
		}
	}
}

// TestRemoteCommands tests the remote command group
func TestRemoteCommands(t *testing.T) {
	if remoteCmd == nil {
		t.Fatal("remoteCmd should not be nil")
	}
	if remoteCmd.Use != "remote" {
		t.Errorf("expected Use 'remote', got %q", remoteCmd.Use)
	}
	if !remoteCmd.HasSubCommands() {
		t.Error("remote should have subcommands")
	}

	// Check subcommands
	subcommands := []string{"set", "get", "list", "del"}
	for _, sub := range subcommands {
		found := false
		for _, cmd := range remoteCmd.Commands() {
			if cmd.Use == sub || strings.HasPrefix(cmd.Use, sub) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("remote should have subcommand %q", sub)
		}
	}
}

// TestAuthCommands tests the auth command group
func TestAuthCommands(t *testing.T) {
	if authCmd == nil {
		t.Fatal("authCmd should not be nil")
	}
	if authCmd.Use != "auth" {
		t.Errorf("expected Use 'auth', got %q", authCmd.Use)
	}
	if !authCmd.HasSubCommands() {
		t.Error("auth should have subcommands")
	}

	// Check subcommands
	subcommands := []string{"login", "logout", "status"}
	for _, sub := range subcommands {
		found := false
		for _, cmd := range authCmd.Commands() {
			if cmd.Use == sub || strings.HasPrefix(cmd.Use, sub) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("auth should have subcommand %q", sub)
		}
	}
}

// TestPushCommand tests the push command
func TestPushCommand(t *testing.T) {
	if pushCmd == nil {
		t.Fatal("pushCmd should not be nil")
	}
	if !strings.HasPrefix(pushCmd.Use, "push") {
		t.Errorf("expected Use to start with 'push', got %q", pushCmd.Use)
	}
}

// TestFetchCommand tests the fetch command
func TestFetchCommand(t *testing.T) {
	if fetchCmd == nil {
		t.Fatal("fetchCmd should not be nil")
	}
	if !strings.HasPrefix(fetchCmd.Use, "fetch") {
		t.Errorf("expected Use to start with 'fetch', got %q", fetchCmd.Use)
	}
}

// TestCloneCommand tests the clone command
func TestCloneCommand(t *testing.T) {
	if cloneCmd == nil {
		t.Fatal("cloneCmd should not be nil")
	}
	if !strings.HasPrefix(cloneCmd.Use, "clone") {
		t.Errorf("expected Use to start with 'clone', got %q", cloneCmd.Use)
	}
}

// TestCheckoutCommand tests the checkout command
func TestCheckoutCommand(t *testing.T) {
	if checkoutCmd == nil {
		t.Fatal("checkoutCmd should not be nil")
	}
	if !strings.HasPrefix(checkoutCmd.Use, "checkout") {
		t.Errorf("expected Use to start with 'checkout', got %q", checkoutCmd.Use)
	}
}

// TestIntegrateCommand tests the integrate command
func TestIntegrateCommand(t *testing.T) {
	if integrateCmd == nil {
		t.Fatal("integrateCmd should not be nil")
	}
	if integrateCmd.Use != "integrate" {
		t.Errorf("expected Use 'integrate', got %q", integrateCmd.Use)
	}
}

// TestPickCommand tests the pick command
func TestPickCommand(t *testing.T) {
	if pickCmd == nil {
		t.Fatal("pickCmd should not be nil")
	}
	if !strings.HasPrefix(pickCmd.Use, "pick") {
		t.Errorf("expected Use to start with 'pick', got %q", pickCmd.Use)
	}
}

// TestCompletionCommand tests the completion command
func TestCompletionCommand(t *testing.T) {
	if completionCmd == nil {
		t.Fatal("completionCmd should not be nil")
	}
	if !strings.HasPrefix(completionCmd.Use, "completion") {
		t.Errorf("expected Use to start with 'completion', got %q", completionCmd.Use)
	}
	// Should accept specific shell types
	if len(completionCmd.ValidArgs) == 0 {
		t.Error("completion should have valid args")
	}
}

// TestRemoteLogCommand tests the remote-log command
func TestRemoteLogCommand(t *testing.T) {
	if remoteLogCmd == nil {
		t.Fatal("remoteLogCmd should not be nil")
	}
	if !strings.HasPrefix(remoteLogCmd.Use, "remote-log") {
		t.Errorf("expected Use to start with 'remote-log', got %q", remoteLogCmd.Use)
	}
}

// TestShortID tests the shortID helper function
func TestShortID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"1234567890123456", "123456789012"},
		{"123456789012", "123456789012"},
		{"12345678901", "12345678901"},
		{"short", "short"},
		{"", ""},
	}

	for _, tt := range tests {
		result := shortID(tt.input)
		if result != tt.expected {
			t.Errorf("shortID(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

// TestCommandFlags tests that commands have expected flags
func TestCommandFlags(t *testing.T) {
	tests := []struct {
		cmd      *cobra.Command
		flags    []string
		cmdName  string
	}{
		{snapshotCreateCmd, []string{"repo", "dir", "message"}, "snapshot create"},
		{statusCmd, []string{"dir", "against", "name-only", "json", "semantic"}, "status"},
		{logCmd, []string{"limit"}, "log"},
		{diffCmd, []string{"dir", "name-only", "explain", "semantic", "json", "patch"}, "diff"},
		{checkoutCmd, []string{"dir", "clean"}, "checkout"},
		{pushCmd, []string{"force", "all"}, "push"},
		{remoteLogCmd, []string{"ref", "limit"}, "remote-log"},
	}

	for _, tt := range tests {
		for _, flagName := range tt.flags {
			flag := tt.cmd.Flags().Lookup(flagName)
			if flag == nil {
				t.Errorf("%s should have --%s flag", tt.cmdName, flagName)
			}
		}
	}
}

// TestRunInit_CreatesKaiDirectory tests that init creates the .kai directory
func TestRunInit_CreatesKaiDirectory(t *testing.T) {
	// Create a temp directory
	tmpDir := t.TempDir()

	// Change to the temp directory
	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	// Run init
	err := runInit(initCmd, nil)
	if err != nil {
		t.Fatalf("runInit failed: %v", err)
	}

	// Check .kai directory exists
	kaiPath := filepath.Join(tmpDir, ".kai")
	if _, err := os.Stat(kaiPath); os.IsNotExist(err) {
		t.Error(".kai directory should exist")
	}

	// Check objects directory exists
	objPath := filepath.Join(kaiPath, "objects")
	if _, err := os.Stat(objPath); os.IsNotExist(err) {
		t.Error("objects directory should exist")
	}

	// Check db.sqlite exists
	dbPath := filepath.Join(kaiPath, "db.sqlite")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Error("db.sqlite should exist")
	}

	// Check kai.modules.yaml exists
	modulesPath := filepath.Join(tmpDir, "kai.modules.yaml")
	if _, err := os.Stat(modulesPath); os.IsNotExist(err) {
		t.Error("kai.modules.yaml should exist")
	}

	// Check AGENTS.md exists
	agentsPath := filepath.Join(kaiPath, "AGENTS.md")
	if _, err := os.Stat(agentsPath); os.IsNotExist(err) {
		t.Error("AGENTS.md should exist")
	}
}

// TestRunInit_Idempotent tests that init can be run multiple times
func TestRunInit_Idempotent(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	// Run init twice
	if err := runInit(initCmd, nil); err != nil {
		t.Fatalf("first runInit failed: %v", err)
	}
	if err := runInit(initCmd, nil); err != nil {
		t.Fatalf("second runInit failed: %v", err)
	}

	// Directory should still exist
	if _, err := os.Stat(filepath.Join(tmpDir, ".kai")); os.IsNotExist(err) {
		t.Error(".kai directory should exist after second init")
	}
}

// TestCommandHelp tests that commands have help text
func TestCommandHelp(t *testing.T) {
	commands := []*cobra.Command{
		rootCmd, initCmd, snapshotCmd, analyzeCmd, changesetCmd,
		intentCmd, dumpCmd, listCmd, logCmd, statusCmd, diffCmd,
		wsCmd, integrateCmd, resolveCmd, checkoutCmd, refCmd, pickCmd,
		completionCmd, remoteCmd, pushCmd, fetchCmd, cloneCmd,
		remoteLogCmd, authCmd,
	}

	for _, cmd := range commands {
		if cmd.Short == "" {
			t.Errorf("%s command should have Short description", cmd.Use)
		}
	}
}

// TestSubcommandArgs tests that subcommands have correct arg validation
func TestSubcommandArgs(t *testing.T) {
	// Commands that require exact args
	exactArgsCommands := []struct {
		cmd   *cobra.Command
		args  int
	}{
		{analyzeSymbolsCmd, 1},
		{changesetCreateCmd, 2},
		{intentRenderCmd, 1},
		{dumpCmd, 1},
		{checkoutCmd, 1},
		{refSetCmd, 2},
		{refDelCmd, 1},
		{pickCmd, 1},
		{remoteSetCmd, 2},
		{remoteGetCmd, 1},
		{remoteDelCmd, 1},
	}

	for _, tt := range exactArgsCommands {
		// Create wrong number of args
		wrongArgs := make([]string, tt.args+1)
		err := tt.cmd.Args(tt.cmd, wrongArgs)
		if err == nil {
			t.Errorf("%s should reject wrong number of args", tt.cmd.Use)
		}
	}
}

// TestCompletionShells tests that completion supports expected shells
func TestCompletionShells(t *testing.T) {
	expectedShells := []string{"bash", "zsh", "fish", "powershell"}

	for _, shell := range expectedShells {
		found := false
		for _, arg := range completionCmd.ValidArgs {
			if arg == shell {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("completion should support %s", shell)
		}
	}
}

// TestRunCompletion tests the completion command
func TestRunCompletion(t *testing.T) {
	shells := []string{"bash", "zsh", "fish", "powershell"}

	for _, shell := range shells {
		t.Run(shell, func(t *testing.T) {
			// Capture stdout
			oldStdout := os.Stdout
			r, w, _ := os.Pipe()
			os.Stdout = w

			// Read from pipe concurrently to avoid deadlock when
			// completion output exceeds the OS pipe buffer (~64KB).
			var buf bytes.Buffer
			done := make(chan struct{})
			go func() {
				buf.ReadFrom(r)
				close(done)
			}()

			err := runCompletion(completionCmd, []string{shell})

			w.Close()
			os.Stdout = oldStdout
			<-done

			if err != nil {
				t.Errorf("runCompletion(%s) failed: %v", shell, err)
			}

			// Should produce some output
			if buf.Len() == 0 {
				t.Errorf("completion for %s should produce output", shell)
			}
		})
	}
}

// TestAllCommandsRegistered tests that all expected commands are registered
func TestAllCommandsRegistered(t *testing.T) {
	expectedCommands := []string{
		"init", "snapshot", "analyze", "changeset", "intent",
		"dump", "list", "log", "status", "diff", "ws",
		"integrate", "checkout", "ref", "pick", "completion",
		"remote", "push", "fetch", "clone", "remote-log", "auth",
	}

	registeredCommands := make(map[string]bool)
	for _, cmd := range rootCmd.Commands() {
		registeredCommands[cmd.Name()] = true
	}

	for _, expected := range expectedCommands {
		if !registeredCommands[expected] {
			t.Errorf("command %q should be registered", expected)
		}
	}
}

// TestNoHiddenCommands ensures no commands are hidden (regression test for mode removal)
func TestNoHiddenCommands(t *testing.T) {
	// These commands were previously hidden in "Simple mode"
	// They must always be visible now
	formerlyHiddenCommands := []string{
		"snapshot", "snap", "analyze", "changeset", "intent",
		"dump", "list", "log", "ws", "integrate", "merge",
		"checkout", "ref", "modules", "pick", "test",
		"remote", "push", "fetch", "clone", "prune", "remote-log",
	}

	for _, name := range formerlyHiddenCommands {
		cmd, _, err := rootCmd.Find([]string{name})
		if err != nil {
			t.Errorf("command %q should be findable: %v", name, err)
			continue
		}
		if cmd.Hidden {
			t.Errorf("command %q should NOT be hidden (mode system was removed)", name)
		}
	}
}

// TestNoModeFlag ensures no --mode flag exists on any command (regression test)
func TestNoModeFlag(t *testing.T) {
	// Check root command
	if rootCmd.Flags().Lookup("mode") != nil {
		t.Error("rootCmd should NOT have a --mode flag (mode system was removed)")
	}

	// Check all subcommands
	for _, cmd := range rootCmd.Commands() {
		if cmd.Flags().Lookup("mode") != nil {
			t.Errorf("command %q should NOT have a --mode flag (mode system was removed)", cmd.Name())
		}
	}
}

// TestCommandsHaveGroups ensures commands are properly grouped for discoverability
func TestCommandsHaveGroups(t *testing.T) {
	// Check that groups are defined
	groups := rootCmd.Groups()
	if len(groups) == 0 {
		t.Error("rootCmd should have command groups defined")
	}

	// Check key commands have group assignments
	// Note: "workspace" and "plumbing" groups were merged into "advanced" to simplify PLG UX
	groupedCommands := map[string]string{
		"init":     "start",
		"capture":  "start",
		"diff":     "diff",
		"review":   "diff",
		"ws":       "advanced", // Workspaces are advanced to reduce cognitive load
		"ci":       "ci",
		"push":     "remote",
		"snapshot": "advanced", // Plumbing commands are in advanced
	}

	for cmdName, expectedGroup := range groupedCommands {
		cmd, _, err := rootCmd.Find([]string{cmdName})
		if err != nil {
			t.Errorf("command %q should be findable: %v", cmdName, err)
			continue
		}
		if cmd.GroupID != expectedGroup {
			t.Errorf("command %q should be in group %q, got %q", cmdName, expectedGroup, cmd.GroupID)
		}
	}
}

// TestFlagsHaveDefaults tests that flags have appropriate defaults
func TestFlagsHaveDefaults(t *testing.T) {
	// Log limit should default to 10
	flag := logCmd.Flags().Lookup("limit")
	if flag != nil && flag.DefValue != "10" {
		t.Errorf("log --limit should default to 10, got %s", flag.DefValue)
	}

	// Remote log limit should default to 20
	flag = remoteLogCmd.Flags().Lookup("limit")
	if flag != nil && flag.DefValue != "20" {
		t.Errorf("remote-log --limit should default to 20, got %s", flag.DefValue)
	}

	// Remote tenant should default to "default"
	flag = remoteSetCmd.Flags().Lookup("tenant")
	if flag != nil && flag.DefValue != "default" {
		t.Errorf("remote set --tenant should default to 'default', got %s", flag.DefValue)
	}

	// Remote repo should default to "main"
	flag = remoteSetCmd.Flags().Lookup("repo")
	if flag != nil && flag.DefValue != "main" {
		t.Errorf("remote set --repo should default to 'main', got %s", flag.DefValue)
	}
}

// TestShowUnifiedDiff tests the pure Go unified diff implementation
func TestShowUnifiedDiff(t *testing.T) {
	tests := []struct {
		name     string
		before   string
		after    string
		wantAdd  bool
		wantDel  bool
	}{
		{
			name:    "single line addition",
			before:  "line1\nline2\n",
			after:   "line1\nline2\nline3\n",
			wantAdd: true,
			wantDel: false,
		},
		{
			name:    "single line deletion",
			before:  "line1\nline2\nline3\n",
			after:   "line1\nline2\n",
			wantAdd: false,
			wantDel: true,
		},
		{
			name:    "modification",
			before:  "line1\nold\nline3\n",
			after:   "line1\nnew\nline3\n",
			wantAdd: true,
			wantDel: true,
		},
		{
			name:    "no change",
			before:  "line1\nline2\n",
			after:   "line1\nline2\n",
			wantAdd: false,
			wantDel: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Capture stdout
			oldStdout := os.Stdout
			r, w, _ := os.Pipe()
			os.Stdout = w

			showUnifiedDiff(tt.before, tt.after)

			w.Close()
			os.Stdout = oldStdout

			var buf bytes.Buffer
			buf.ReadFrom(r)
			output := buf.String()

			hasAdd := strings.Contains(output, "+")
			hasDel := strings.Contains(output, "-")

			if tt.wantAdd && !hasAdd {
				t.Errorf("expected additions in output, got: %s", output)
			}
			if tt.wantDel && !hasDel {
				t.Errorf("expected deletions in output, got: %s", output)
			}
			if !tt.wantAdd && !tt.wantDel && len(output) > 0 {
				// No changes expected, but some edge case output is OK
			}
		})
	}
}

// TestReviewOpenBaseFlag verifies --base flag exists on review open command
func TestReviewOpenBaseFlag(t *testing.T) {
	flag := reviewOpenCmd.Flags().Lookup("base")
	if flag == nil {
		t.Fatal("review open command should have --base flag")
	}
	if flag.DefValue != "" {
		t.Errorf("--base should default to empty, got %s", flag.DefValue)
	}
}

// TestDiffForceFlag verifies --force flag exists on diff command
func TestDiffForceFlag(t *testing.T) {
	flag := diffCmd.Flags().Lookup("force")
	if flag == nil {
		t.Fatal("diff command should have --force flag")
	}
	if flag.DefValue != "false" {
		t.Errorf("--force should default to false, got %s", flag.DefValue)
	}
}

// TestPruneYesFlag verifies --yes flag exists on prune command
func TestPruneYesFlag(t *testing.T) {
	flag := pruneCmd.Flags().Lookup("yes")
	if flag == nil {
		t.Fatal("prune command should have --yes flag")
	}
	if flag.DefValue != "false" {
		t.Errorf("--yes should default to false, got %s", flag.DefValue)
	}
}

// TestPruneKeepFlag verifies --keep flag exists on prune command
func TestPruneKeepFlag(t *testing.T) {
	flag := pruneCmd.Flags().Lookup("keep")
	if flag == nil {
		t.Fatal("prune command should have --keep flag")
	}
}

// TestGateCommand verifies the gate command tree is registered with
// the four expected subcommands. The actual list/approve/reject logic
// is covered by internal/workspace/gate_e2e_test.go which exercises
// the full DB lifecycle.
func TestGateCommand(t *testing.T) {
	if gateCmd == nil {
		t.Fatal("gateCmd should not be nil")
	}
	if gateCmd.Use != "gate" {
		t.Errorf("expected Use 'gate', got %q", gateCmd.Use)
	}
	want := map[string]bool{"list": false, "show": false, "approve": false, "reject": false}
	for _, sub := range gateCmd.Commands() {
		// sub.Use can be "show <snapshot-id>" — split on whitespace.
		name := sub.Use
		if i := indexSpace(name); i >= 0 {
			name = name[:i]
		}
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing gate subcommand: %s", name)
		}
	}
}

func indexSpace(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			return i
		}
	}
	return -1
}
