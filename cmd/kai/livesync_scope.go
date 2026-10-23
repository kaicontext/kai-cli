package main

import (
	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/util"
	"github.com/kaicontext/kai-engine/workspace"
)

// liveSyncScope is the server-side scope for a workspace's live sync:
// "<name>#<workspace-id>". The server scopes channels, the sync_events log
// and replay by this opaque string. Scoping by name alone made a NEW
// workspace that reused a name inherit every live edit ever pushed under it
// — a workspace deleted, re-created or created independently elsewhere — and
// replay them onto its working tree. The ID is preserved by `kai fetch --ws`
// (the transport carries the original UUID), so everyone who joins a
// workspace that way shares one scope. "" stays "" (repo-wide); a name with
// no local workspace falls back to the bare name.
func liveSyncScope(db *graph.DB, name string) string {
	if name == "" || db == nil {
		return name
	}
	ws, err := workspace.NewManager(db).Get(name)
	if err != nil || ws == nil || len(ws.ID) == 0 {
		return name
	}
	return name + "#" + util.BytesToHex(ws.ID)
}

// currentLiveSyncScope is liveSyncScope for the checked-out workspace, for
// callers without an open DB.
func currentLiveSyncScope() string {
	name, _ := getCurrentWorkspace()
	if name == "" {
		return ""
	}
	db, err := openDB()
	if err != nil {
		return name
	}
	defer db.Close()
	return liveSyncScope(db, name)
}
