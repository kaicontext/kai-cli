//go:build reviewer

package main

import (
	"github.com/kaicontext/kai-engine/provider"
	"github.com/kaicontext/kai-engine/reviewcmd"

	"kai/internal/config"
)

// The reviewer is Kai-team-only: this stub compiles only with `-tags
// reviewer` (internal Kai-team builds — see kai-engine's
// scripts/local-review.sh), never into user releases — the command, its
// prompts, and its help text must not exist in published binaries (ci.yml
// guards this). The reviewer itself lives in kai-engine/reviewcmd
// (private); the only thing wired here is the CLI's provider plumbing.
func init() {
	rootCmd.AddCommand(reviewcmd.New(func() (provider.Provider, string, error) {
		cfg, err := config.Load(kaiDir)
		if err != nil {
			return nil, "", err
		}
		prov, reviewModel, _, err := buildGateProvider(cfg)
		return prov, reviewModel, err
	}))
}
