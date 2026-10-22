package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kaicontext/kai-engine/kaipath"
	"github.com/spf13/cobra"
)

// splitStoreExempt are commands that run against a split project anyway:
// they diagnose it, print static output, or never open the store
// (`log ingest` runs from agent hooks and writes into the resolved dir).
var splitStoreExempt = map[string]bool{
	"doctor": true, "version": true, "help": true, "completion": true, "ingest": true,
}

// refuseSplitStore stops any command that would read or write a project
// whose history is split across .kai/db.sqlite and .git/kai/db.sqlite.
// Resolve would pick one and the other would keep diverging; that is how
// riddle ended up with Desktop checkpoints in one database and the pushed
// git history in the other.
func refuseSplitStore(cmd *cobra.Command) {
	if os.Getenv("KAI_DIR") != "" || splitStoreExempt[cmd.Name()] {
		return
	}
	stores := kaipath.Stores(".")
	if len(stores) < 2 {
		return
	}
	fmt.Fprint(os.Stderr, splitStoreMessage(stores))
	os.Exit(1)
}

func splitStoreMessage(stores []string) string {
	var b strings.Builder
	b.WriteString("kai: this project has two kai stores, and kai will not pick one silently:\n")
	for _, s := range stores {
		fmt.Fprintf(&b, "  %s  (%s)\n", filepath.Join(s, kaipath.StoreFile), describeStore(s))
	}
	b.WriteString("Keep the one you want and move the other out of the way, e.g.\n")
	fmt.Fprintf(&b, "  mv %s %s.split\n", stores[len(stores)-1], stores[len(stores)-1])
	b.WriteString("then run kai again. 'kai doctor' shows the same summary.\n")
	return b.String()
}

// describeStore summarizes a store well enough to choose between two:
// snapshot and ref counts, whether it maps git commits, and whether it has
// been pushed.
func describeStore(dir string) string {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, kaipath.StoreFile)+"?_pragma=query_only(1)")
	if err != nil {
		return "unreadable: " + err.Error()
	}
	defer db.Close()
	var snaps, refs, gitRefs, remoteRefs int
	if err := db.QueryRow(`SELECT count(*) FROM nodes WHERE kind = 'Snapshot'`).Scan(&snaps); err != nil {
		return "unreadable: " + err.Error()
	}
	_ = db.QueryRow(`SELECT count(*), coalesce(sum(name LIKE 'git.%'), 0), coalesce(sum(name LIKE 'remote/%'), 0) FROM refs`).Scan(&refs, &gitRefs, &remoteRefs)
	parts := []string{fmt.Sprintf("%d snapshots", snaps), fmt.Sprintf("%d refs", refs)}
	if gitRefs > 0 {
		parts = append(parts, fmt.Sprintf("%d git commits mapped", gitRefs))
	}
	if remoteRefs > 0 {
		parts = append(parts, "pushed")
	} else {
		parts = append(parts, "never pushed")
	}
	if info, err := os.Stat(filepath.Join(dir, kaipath.StoreFile)); err == nil {
		parts = append(parts, "modified "+info.ModTime().Format("2006-01-02 15:04"))
	}
	return strings.Join(parts, ", ")
}
