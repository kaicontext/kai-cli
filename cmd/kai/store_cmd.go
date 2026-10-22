package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kaicontext/kai-engine/kaipath"
	"github.com/kaicontext/kai-engine/storemerge"
	"github.com/spf13/cobra"
)

var storeCmd = &cobra.Command{
	Use:   "store",
	Short: "Show where this project's kai store lives",
	Long: `Every project keeps its kai store in <project>/.kai. Projects initialized
while .git/kai was the default for git repos move there automatically the
first time kai runs in them, leaving .git/kai as a symlink.

A project whose history was split across both locations needs
'kai store merge'.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		stores := kaipath.Stores(".")
		switch len(stores) {
		case 0:
			fmt.Println("no kai store here — run 'kai init'")
		case 1:
			fmt.Printf("%s  (%s)\n", filepath.Join(stores[0], kaipath.StoreFile), describeStore(stores[0]))
		default:
			fmt.Print(splitStoreMessage(stores))
		}
		return nil
	},
}

var storeMergeCmd = &cobra.Command{
	Use:   "merge",
	Short: "Merge a split project's .git/kai store into .kai",
	Long: `Copies everything .git/kai/db.sqlite has that .kai/db.sqlite lacks — snapshots,
workspaces, edges, refs, logs and objects — into .kai. Nothing in .kai is
overwritten: a ref the two stores point at different snapshots keeps .kai's
target and the other is kept as split/<name>. .git/kai is then kept as
.git/kai.merged-<timestamp> and replaced by a symlink to .kai.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		stores := kaipath.Stores(".")
		if len(stores) < 2 {
			return fmt.Errorf("this project has one kai store; nothing to merge")
		}
		primary, other := stores[0], stores[1]
		fmt.Printf("merging %s (%s)\n   into %s (%s)\n", other, describeStore(other), primary, describeStore(primary))
		rep, err := storemerge.Merge(primary, other)
		if err != nil {
			return fmt.Errorf("merge failed, nothing retired: %w", err)
		}
		backup, err := kaipath.RetireLegacy(".")
		if err != nil {
			return fmt.Errorf("merged, but could not retire %s: %w", other, err)
		}
		fmt.Printf("copied %d nodes, %d edges, %d refs, %d log entries, %d objects, %d files\n",
			rep.Rows["nodes"], rep.Rows["edges"], rep.Refs, rep.Rows["logs"], rep.Objects, rep.Files)
		if len(rep.SplitRefs) > 0 {
			fmt.Printf("kept the other store's target as %s<name> for: %s\n", storemerge.SplitRefPrefix, strings.Join(rep.SplitRefs, ", "))
		}
		if len(rep.Pointers) > 0 {
			fmt.Printf("kept .kai's position for: %s\n", strings.Join(rep.Pointers, ", "))
		}
		fmt.Printf("store: %s  (%s)\nold store kept at %s\n", filepath.Join(primary, kaipath.StoreFile), describeStore(primary), backup)
		return nil
	},
}

func init() {
	storeCmd.AddCommand(storeMergeCmd)
	rootCmd.AddCommand(storeCmd)
}
