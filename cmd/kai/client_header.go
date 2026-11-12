package main

import "github.com/kaicontext/kai-engine/agent"

// setClient names this app to the engine at startup, for every command:
// every call the engine makes to the Kai server then carries
// X-Kai-Client: cli/<version>, and run-cost reports carry the version.
// kai's own calls to the server (ship, review, findings, live sync) send the
// same header through clientid.Client, which keeps it off other hosts.
func setClient() {
	agent.SetClient("cli", Version)
}
