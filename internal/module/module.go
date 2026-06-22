// Package module re-exports module matching from kai-core.
package module

import (
	"github.com/kaicontext/kai-core/modulematch"
)

// Re-export types from kai-core/modulematch
type ModuleRule = modulematch.ModuleRule
type ModulesConfig = modulematch.ModulesConfig
type Matcher = modulematch.Matcher

// Re-export functions from kai-core/modulematch
var (
	LoadRules        = modulematch.LoadRules
	LoadRulesOrEmpty = modulematch.LoadRulesOrEmpty
	NewMatcher       = modulematch.NewMatcher
)
