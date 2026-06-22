package main

import (
	"encoding/json"
	"os"

	"github.com/spf13/cobra"
)

var graphCmd = &cobra.Command{
	Use:   "graph",
	Short: "Graph export commands",
}

var (
	graphNodeCursor  int64
	graphEdgeCursor  int64
	graphExportLimit int
)

var graphExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export graph data as JSON",
	RunE:  runGraphExport,
}

func init() {
	graphExportCmd.PersistentFlags().Int64Var(&graphNodeCursor, "node-cursor", 0, "Rowid cursor for node pagination (0 starts from beginning)")
	graphExportCmd.PersistentFlags().Int64Var(&graphEdgeCursor, "edge-cursor", 0, "Rowid cursor for edge pagination (0 starts from beginning)")
	graphExportCmd.PersistentFlags().IntVar(&graphExportLimit, "limit", 10000, "Maximum number of items to return (split evenly between nodes and edges)")

	graphCmd.AddCommand(graphExportCmd)
}

func runGraphExport(cmd *cobra.Command, args []string) error {
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	result, err := db.ExportPage(graphNodeCursor, graphEdgeCursor, graphExportLimit)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(result); err != nil {
		return err
	}
	return nil
}