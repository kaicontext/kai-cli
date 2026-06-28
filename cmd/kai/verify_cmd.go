package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kaicontext/kai-engine/contract"
)

var verifyCmd = &cobra.Command{
	Use:   "verify [contract]",
	Short: "Force a graph-grounded semantic intent-match check now (expensive)",
	Long: "Runs the graph-grounded verifier (kit verify) to judge whether the " +
		"working-tree changes actually implement the declared intent — using the " +
		"semantic-graph tools to catch incomplete changes a diff review misses. " +
		"This is the only check that can reach 'verified'. Writes the verdict + " +
		"residue into the contract and shows in 'kai status'. With no argument it " +
		"verifies every in-flight contract.",
	Args: cobra.MaximumNArgs(1),
	RunE: runVerify,
}

func init() {
	rootCmd.AddCommand(verifyCmd)
}

func runVerify(cmd *cobra.Command, args []string) error {
	store, err := contract.Open(kaiDir)
	if err != nil {
		return err
	}
	defer store.Close()

	var targets []*contract.Contract
	if len(args) == 1 {
		c, err := store.Get(args[0])
		if err != nil {
			return err
		}
		if c == nil {
			return fmt.Errorf("contract %q not found", args[0])
		}
		targets = []*contract.Contract{c}
	} else {
		all, err := store.List()
		if err != nil {
			return err
		}
		for _, c := range all {
			if !c.Closed {
				targets = append(targets, c)
			}
		}
	}
	if len(targets) == 0 {
		fmt.Println("no contracts to verify (open one with 'kai intent')")
		return nil
	}

	kit := kitBinary()
	if kit == "" {
		return fmt.Errorf("kai verify needs the 'kit' binary on PATH (the graph-grounded verifier)")
	}

	structural, _, _ := store.GetStructural()
	ctx := context.Background()
	for _, c := range targets {
		fmt.Printf("verifying %s …\n", c.ID)
		if err := semanticVerify(ctx, kit, store, c, structural); err != nil {
			return err
		}
	}
	return nil
}
