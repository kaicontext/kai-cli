// Package parse re-exports tree-sitter parsing from kai-core.
package parse

import (
	coreparse "github.com/kaicontext/kai-core/parse"
)

// Re-export types from kai-core/parse
type Range = coreparse.Range
type Symbol = coreparse.Symbol
type ParsedFile = coreparse.ParsedFile
type Parser = coreparse.Parser

// Call extraction types
type CallSite = coreparse.CallSite
type Import = coreparse.Import
type ParsedCalls = coreparse.ParsedCalls

// Re-export functions from kai-core/parse
var (
	NewParser          = coreparse.NewParser
	GetNodeRange       = coreparse.GetNodeRange
	GetNodeContent     = coreparse.GetNodeContent
	RangesOverlap      = coreparse.RangesOverlap
	IsTestFile         = coreparse.IsTestFile
	FindTestsForFile   = coreparse.FindTestsForFile
	PossibleFilePaths  = coreparse.PossibleFilePaths
	ResolveImportPath  = coreparse.ResolveImportPath
)
