package main

// kai live join — enter a shared session room. The CRDT room key IS
// the workspace name (Kai Ship D1), so joining a room is nothing more
// than checking out the same workspace with live sync on: the RGA
// merges everyone's concurrent edits, and because the ship branch is
// derived from the same identity, every member's `kai ship` lands on
// the same PR (the server's ship lease collapses simultaneous
// publishes into one).

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kaicontext/kai-engine/kaipath"
	spawnpkg "github.com/kaicontext/kai-engine/spawn"
)

var liveJoinCmd = &cobra.Command{
	Use:   "join <workspace>",
	Short: "Join a shared session room: check out its workspace with live sync on",
	Long: `Check out the named workspace and connect to its live-sync room. Everyone
in the room edits the same workspace — the CRDT merges concurrent edits
line-by-line — and a ` + "`kai ship`" + ` from any member lands on the room's one
PR branch (kai/<workspace>).

If the workspace doesn't exist locally yet it is fetched from the
remote, and created fresh when the room is new.`,
	Args: cobra.ExactArgs(1),
	RunE: runLiveJoin,
}

func init() {
	liveCmd.AddCommand(liveJoinCmd)
}

func runLiveJoin(cmd *cobra.Command, args []string) error {
	name, err := spawnpkg.SanitizeName(args[0])
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	// Joining means syncing: clear any standing live-sync opt-out so the
	// checkout's autosync daemon actually connects to the room.
	if err := os.Remove(autoSyncOffPath(kaipath.Resolve(cwd))); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clearing the live-sync opt-out: %w", err)
	}

	// Checkout order: existing local workspace → fetch it from the
	// remote → create it fresh (the first member founds the room). Each
	// step is a subcommand so join composes the same paths the user
	// could run by hand, and its failure messages stay theirs.
	if err := runIn(cwd, "ws", "checkout", name); err != nil {
		if ferr := runIn(cwd, "fetch", "--ws", name); ferr == nil {
			err = runIn(cwd, "ws", "checkout", name)
		} else if cerr := runIn(cwd, "ws", "create", name); cerr == nil {
			fmt.Printf("workspace %s did not exist anywhere — created it; you are the room's first member\n", name)
			err = runIn(cwd, "ws", "checkout", name)
		}
		if err != nil {
			return fmt.Errorf("joining %s: %w", name, err)
		}
	}

	fmt.Printf("joined %s — edits sync live with everyone in the room; `kai ship` from any member lands on kai/%s\n", name, name)
	return nil
}
