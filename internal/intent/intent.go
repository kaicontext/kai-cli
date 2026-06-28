// Package intent generates intent sentences from change analysis.
package intent

import (
	"path/filepath"
	"strings"

	"github.com/kaicontext/kai-engine/classify"
	"github.com/kaicontext/kai-engine/graph"
	"github.com/kaicontext/kai-engine/util"

	coreintent "github.com/kaicontext/kai-core/intent"
)

// Generator generates intent sentences.
type Generator struct {
	db     *graph.DB
	engine *coreintent.Engine
}

// IntentResult wraps the core intent result for CLI use.
type IntentResult = coreintent.IntentResult

// IntentCandidate wraps the core intent candidate for CLI use.
type IntentCandidate = coreintent.IntentCandidate

// NewGenerator creates a new intent generator.
func NewGenerator(db *graph.DB) *Generator {
	return &Generator{
		db:     db,
		engine: coreintent.NewEngine(),
	}
}

// GenerateIntent generates an intent sentence for a changeset.
func (g *Generator) GenerateIntent(changeSetID []byte, changeTypes []*classify.ChangeType, modules []string, symbols []*graph.Node, changedFiles []string) string {
	// Determine verb from change types (priority order)
	verb := determineVerb(changeTypes)

	// Determine module
	module := "General"
	if len(modules) > 0 {
		module = modules[0]
	}

	// Try to get function names from change types first (most specific)
	funcNames := extractFunctionNames(changeTypes)
	if len(funcNames) > 0 {
		area := formatFunctionNames(funcNames)
		return verb + " " + area + " in " + module
	}

	// Fall back to symbol names or file paths
	area := determineArea(symbols, changedFiles)

	return verb + " " + module + " " + area
}

// extractFunctionNames extracts function names from FUNCTION_ADDED/REMOVED change types.
func extractFunctionNames(changeTypes []*classify.ChangeType) []string {
	var names []string
	seen := make(map[string]bool)

	for _, ct := range changeTypes {
		if ct.Category == classify.FunctionAdded || ct.Category == classify.FunctionRemoved {
			for _, sym := range ct.Evidence.Symbols {
				// Function names are stored as "name:functionName"
				if strings.HasPrefix(sym, "name:") {
					name := strings.TrimPrefix(sym, "name:")
					if !seen[name] {
						seen[name] = true
						names = append(names, name)
					}
				}
			}
		}
	}

	return names
}

// formatFunctionNames formats a list of function names for display.
func formatFunctionNames(names []string) string {
	if len(names) == 0 {
		return ""
	}
	if len(names) == 1 {
		return names[0]
	}
	if len(names) == 2 {
		return names[0] + " and " + names[1]
	}
	// More than 2 functions
	return strings.Join(names[:2], ", ") + " and others"
}

// determineVerb determines the verb based on change types.
func determineVerb(changeTypes []*classify.ChangeType) string {
	// Priority order for semantic changes
	hasFuncAdded := false
	hasFuncRemoved := false
	hasAPI := false
	hasCondition := false
	hasConstant := false
	hasJSONField := false
	hasJSONValue := false
	hasYAMLKey := false
	hasYAMLValue := false
	hasFileContent := false

	for _, ct := range changeTypes {
		switch ct.Category {
		case classify.FunctionAdded:
			hasFuncAdded = true
		case classify.FunctionRemoved:
			hasFuncRemoved = true
		case classify.APISurfaceChanged:
			hasAPI = true
		case classify.ConditionChanged:
			hasCondition = true
		case classify.ConstantUpdated:
			hasConstant = true
		case classify.JSONFieldAdded, classify.JSONFieldRemoved:
			hasJSONField = true
		case classify.JSONValueChanged, classify.JSONArrayChanged:
			hasJSONValue = true
		case classify.YAMLKeyAdded, classify.YAMLKeyRemoved:
			hasYAMLKey = true
		case classify.YAMLValueChanged:
			hasYAMLValue = true
		case classify.FileContentChanged:
			hasFileContent = true
		case classify.FileAdded:
			return "Add"
		case classify.FileDeleted:
			return "Remove"
		}
	}

	// Function changes take highest priority
	if hasFuncAdded && hasFuncRemoved {
		return "Refactor"
	}
	if hasFuncAdded {
		return "Add"
	}
	if hasFuncRemoved {
		return "Remove"
	}
	// Semantic code changes
	if hasAPI {
		return "Update"
	}
	if hasCondition {
		return "Modify"
	}
	if hasConstant {
		return "Update"
	}
	// JSON changes
	if hasJSONField {
		return "Update"
	}
	if hasJSONValue {
		return "Modify"
	}
	// YAML changes
	if hasYAMLKey {
		return "Update"
	}
	if hasYAMLValue {
		return "Modify"
	}
	// File-level fallback
	if hasFileContent {
		return "Update"
	}

	return "Change"
}

// determineArea determines the area from symbols or paths.
func determineArea(symbols []*graph.Node, changedFiles []string) string {
	// Try to get a symbol name first
	if len(symbols) > 0 {
		for _, sym := range symbols {
			if name, ok := sym.Payload["fqName"].(string); ok && name != "" {
				// Return the simple name (last part if dotted)
				parts := strings.Split(name, ".")
				return parts[len(parts)-1]
			}
		}
	}

	// Fallback to common path prefix
	if len(changedFiles) > 0 {
		return getCommonArea(changedFiles)
	}

	return "codebase"
}

// getCommonArea extracts a meaningful area name from file paths.
func getCommonArea(paths []string) string {
	if len(paths) == 0 {
		return "codebase"
	}

	if len(paths) == 1 {
		// Use the file name without extension
		base := filepath.Base(paths[0])
		ext := filepath.Ext(base)
		return strings.TrimSuffix(base, ext)
	}

	// Find common directory prefix
	dirs := make([][]string, len(paths))
	minLen := -1

	for i, p := range paths {
		dirs[i] = strings.Split(filepath.Dir(p), string(filepath.Separator))
		if minLen == -1 || len(dirs[i]) < minLen {
			minLen = len(dirs[i])
		}
	}

	if minLen <= 0 {
		return "codebase"
	}

	// Find the longest common prefix
	var common []string
	for i := 0; i < minLen; i++ {
		val := dirs[0][i]
		allMatch := true
		for j := 1; j < len(dirs); j++ {
			if dirs[j][i] != val {
				allMatch = false
				break
			}
		}
		if allMatch {
			common = append(common, val)
		} else {
			break
		}
	}

	if len(common) > 0 {
		// Return the last meaningful directory name
		for i := len(common) - 1; i >= 0; i-- {
			if common[i] != "" && common[i] != "." {
				return common[i]
			}
		}
	}

	return "codebase"
}

// GetSymbolsForChangeSet retrieves all symbols associated with a changeset.
// Symbols are found by looking up symbols defined in the modified files.
func (g *Generator) GetSymbolsForChangeSet(changeSetID []byte) ([]*graph.Node, error) {
	// Get the changeset node to find the head snapshot
	csNode, err := g.db.GetNode(changeSetID)
	if err != nil {
		return nil, err
	}
	if csNode == nil {
		return nil, nil
	}

	// Get the head snapshot ID from the changeset
	headHex, ok := csNode.Payload["head"].(string)
	if !ok || headHex == "" {
		return nil, nil
	}
	headSnapID, _ := util.HexToBytes(headHex)

	// Get MODIFIES edges from changeset to files
	edges, err := g.db.GetEdges(changeSetID, graph.EdgeModifies)
	if err != nil {
		return nil, err
	}

	var symbols []*graph.Node
	seen := make(map[string]bool)

	for _, edge := range edges {
		fileNode, err := g.db.GetNode(edge.Dst)
		if err != nil {
			return nil, err
		}
		if fileNode == nil || fileNode.Kind != graph.KindFile {
			continue
		}

		// Get symbols defined in this file for this snapshot context
		symEdges, err := g.db.GetEdgesByContextAndDst(headSnapID, graph.EdgeDefinesIn, edge.Dst)
		if err != nil {
			continue
		}

		for _, symEdge := range symEdges {
			symIDHex := util.BytesToHex(symEdge.Src)
			if seen[symIDHex] {
				continue
			}
			seen[symIDHex] = true

			symNode, err := g.db.GetNode(symEdge.Src)
			if err != nil {
				continue
			}
			if symNode != nil && symNode.Kind == graph.KindSymbol {
				symbols = append(symbols, symNode)
			}
		}
	}

	return symbols, nil
}

// GetModulesForChangeSet retrieves all module names associated with a changeset.
func (g *Generator) GetModulesForChangeSet(changeSetID []byte) ([]string, error) {
	// Get AFFECTS edges from changeset to modules
	edges, err := g.db.GetEdges(changeSetID, graph.EdgeAffects)
	if err != nil {
		return nil, err
	}

	var modules []string
	for _, edge := range edges {
		node, err := g.db.GetNode(edge.Dst)
		if err != nil {
			return nil, err
		}
		if node != nil && node.Kind == graph.KindModule {
			if name, ok := node.Payload["name"].(string); ok {
				modules = append(modules, name)
			}
		}
	}

	return modules, nil
}

// GetChangeTypesForChangeSet retrieves all change types for a changeset.
func (g *Generator) GetChangeTypesForChangeSet(changeSetID []byte) ([]*classify.ChangeType, error) {
	edges, err := g.db.GetEdges(changeSetID, graph.EdgeHas)
	if err != nil {
		return nil, err
	}

	var changeTypes []*classify.ChangeType
	for _, edge := range edges {
		node, err := g.db.GetNode(edge.Dst)
		if err != nil {
			return nil, err
		}
		if node != nil && node.Kind == graph.KindChangeType {
			ct := payloadToChangeType(node.Payload)
			if ct != nil {
				changeTypes = append(changeTypes, ct)
			}
		}
	}

	return changeTypes, nil
}

func payloadToChangeType(payload map[string]interface{}) *classify.ChangeType {
	category, ok := payload["category"].(string)
	if !ok {
		return nil
	}

	ct := &classify.ChangeType{
		Category: classify.ChangeCategory(category),
	}

	if evidence, ok := payload["evidence"].(map[string]interface{}); ok {
		if fileRanges, ok := evidence["fileRanges"].([]interface{}); ok {
			for _, fr := range fileRanges {
				if frMap, ok := fr.(map[string]interface{}); ok {
					fileRange := classify.FileRange{}
					if path, ok := frMap["path"].(string); ok {
						fileRange.Path = path
					}
					if start, ok := frMap["start"].([]interface{}); ok && len(start) == 2 {
						fileRange.Start = [2]int{int(start[0].(float64)), int(start[1].(float64))}
					}
					if end, ok := frMap["end"].([]interface{}); ok && len(end) == 2 {
						fileRange.End = [2]int{int(end[0].(float64)), int(end[1].(float64))}
					}
					ct.Evidence.FileRanges = append(ct.Evidence.FileRanges, fileRange)
				}
			}
		}
		if symbols, ok := evidence["symbols"].([]interface{}); ok {
			for _, s := range symbols {
				if sym, ok := s.(string); ok {
					ct.Evidence.Symbols = append(ct.Evidence.Symbols, sym)
				}
			}
		}
	}

	return ct
}

// GetChangedFilesForChangeSet retrieves file paths for a changeset.
func (g *Generator) GetChangedFilesForChangeSet(changeSetID []byte) ([]string, error) {
	edges, err := g.db.GetEdges(changeSetID, graph.EdgeModifies)
	if err != nil {
		return nil, err
	}

	var paths []string
	seen := make(map[string]bool)

	for _, edge := range edges {
		node, err := g.db.GetNode(edge.Dst)
		if err != nil {
			return nil, err
		}
		if node != nil && node.Kind == graph.KindFile {
			if path, ok := node.Payload["path"].(string); ok {
				if !seen[path] {
					seen[path] = true
					paths = append(paths, path)
				}
			}
		}
	}

	return paths, nil
}

// UpdateChangeSetIntent creates or updates an Intent node linked to the changeset.
// This creates a separate Intent node to preserve content-addressing of the ChangeSet.
func (g *Generator) UpdateChangeSetIntent(changeSetID []byte, intent string) error {
	// Delete any existing intent edge
	existingEdges, err := g.db.GetEdges(changeSetID, graph.EdgeHasIntent)
	if err != nil {
		return err
	}
	for _, edge := range existingEdges {
		if err := g.db.DeleteEdgeDirect(changeSetID, graph.EdgeHasIntent, edge.Dst); err != nil {
			return err
		}
	}

	// Create intent node with content-addressed ID
	intentPayload := map[string]interface{}{
		"text":        intent,
		"changeSetID": util.BytesToHex(changeSetID),
	}
	intentID, err := g.db.InsertNodeDirect(graph.KindIntent, intentPayload)
	if err != nil {
		return err
	}

	// Create edge from ChangeSet to Intent
	return g.db.InsertEdgeDirect(changeSetID, graph.EdgeHasIntent, intentID, nil)
}

// GetChangeSetIntent retrieves the intent for a changeset via the HAS_INTENT edge.
func (g *Generator) GetChangeSetIntent(changeSetID []byte) (string, error) {
	edges, err := g.db.GetEdges(changeSetID, graph.EdgeHasIntent)
	if err != nil {
		return "", err
	}
	if len(edges) == 0 {
		return "", nil
	}

	// Get the Intent node
	intentNode, err := g.db.GetNode(edges[0].Dst)
	if err != nil {
		return "", err
	}
	if intentNode == nil {
		return "", nil
	}

	if text, ok := intentNode.Payload["text"].(string); ok {
		return text, nil
	}
	return "", nil
}

// RenderIntent renders the intent for a changeset.
// If editText is provided, it saves that text.
// If regenerate is true, it regenerates even if intent exists.
// Otherwise, returns existing intent or generates if none exists.
func (g *Generator) RenderIntent(changeSetID []byte, editText string, regenerate bool) (string, error) {
	if editText != "" {
		// Use provided text and persist it
		if err := g.UpdateChangeSetIntent(changeSetID, editText); err != nil {
			return "", err
		}
		return editText, nil
	}

	// Check if there's already a saved intent (unless regenerating)
	if !regenerate {
		existingIntent, err := g.GetChangeSetIntent(changeSetID)
		if err != nil {
			return "", err
		}
		if existingIntent != "" {
			return existingIntent, nil
		}
	}

	// Generate from data
	changeTypes, err := g.GetChangeTypesForChangeSet(changeSetID)
	if err != nil {
		return "", err
	}

	modules, err := g.GetModulesForChangeSet(changeSetID)
	if err != nil {
		return "", err
	}

	symbols, err := g.GetSymbolsForChangeSet(changeSetID)
	if err != nil {
		return "", err
	}

	files, err := g.GetChangedFilesForChangeSet(changeSetID)
	if err != nil {
		return "", err
	}

	intent := g.GenerateIntent(changeSetID, changeTypes, modules, symbols, files)

	if err := g.UpdateChangeSetIntent(changeSetID, intent); err != nil {
		return "", err
	}

	return intent, nil
}

// GenerateIntentWithConfidence generates an intent with full confidence information.
// This is the new API that provides alternatives, warnings, and reasoning.
func (g *Generator) GenerateIntentWithConfidence(changeSetID []byte, minConfidence float64) (*IntentResult, error) {
	changeTypes, err := g.GetChangeTypesForChangeSet(changeSetID)
	if err != nil {
		return nil, err
	}

	modules, err := g.GetModulesForChangeSet(changeSetID)
	if err != nil {
		return nil, err
	}

	files, err := g.GetChangedFilesForChangeSet(changeSetID)
	if err != nil {
		return nil, err
	}

	// Convert to signals
	signals := classify.ConvertToSignals(changeTypes)

	// Generate intent with engine
	result := g.engine.GenerateIntent(signals, modules, files)

	return result, nil
}

// RenderIntentWithConfidence renders intent with confidence info.
// If editText is provided, it saves that text.
// If regenerate is true, it regenerates even if intent exists.
// Otherwise, returns existing intent or generates if none exists.
// Returns the full IntentResult for access to alternatives and warnings.
func (g *Generator) RenderIntentWithConfidence(changeSetID []byte, editText string, regenerate bool, minConfidence float64) (*IntentResult, error) {
	if editText != "" {
		// Use provided text and persist it
		if err := g.UpdateChangeSetIntent(changeSetID, editText); err != nil {
			return nil, err
		}
		return &IntentResult{
			Primary: &IntentCandidate{
				Text:       editText,
				Confidence: 1.0,
				Template:   "user_provided",
				Reasoning:  "User-provided intent",
			},
		}, nil
	}

	// Check if there's already a saved intent (unless regenerating)
	if !regenerate {
		existingIntent, err := g.GetChangeSetIntent(changeSetID)
		if err != nil {
			return nil, err
		}
		if existingIntent != "" {
			return &IntentResult{
				Primary: &IntentCandidate{
					Text:       existingIntent,
					Confidence: 1.0,
					Template:   "cached",
					Reasoning:  "Previously saved intent",
				},
			}, nil
		}
	}

	// Generate with confidence
	result, err := g.GenerateIntentWithConfidence(changeSetID, minConfidence)
	if err != nil {
		return nil, err
	}

	// Fall back only when the engine produced a generic update below threshold.
	if result.Primary != nil && result.Primary.Confidence < minConfidence && result.Primary.Template == "generic_update" {
		// Try legacy generation as fallback
		changeTypes, _ := g.GetChangeTypesForChangeSet(changeSetID)
		modules, _ := g.GetModulesForChangeSet(changeSetID)
		symbols, _ := g.GetSymbolsForChangeSet(changeSetID)
		files, _ := g.GetChangedFilesForChangeSet(changeSetID)

		legacyIntent := g.legacyGenerateIntent(changeTypes, modules, symbols, files)
		result.Primary = &IntentCandidate{
			Text:       legacyIntent,
			Confidence: minConfidence, // Set to threshold since we're using fallback
			Template:   "legacy",
			Reasoning:  "Fallback to legacy generation due to low confidence",
		}
	}

	// Save the intent
	if result.Primary != nil {
		if err := g.UpdateChangeSetIntent(changeSetID, result.Primary.Text); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// legacyGenerateIntent is the original generation logic for backward compatibility.
func (g *Generator) legacyGenerateIntent(changeTypes []*classify.ChangeType, modules []string, symbols []*graph.Node, changedFiles []string) string {
	// Determine verb from change types (priority order)
	verb := determineVerb(changeTypes)

	// Determine module
	module := "General"
	if len(modules) > 0 {
		module = modules[0]
	}

	// Try to get function names from change types first (most specific)
	funcNames := extractFunctionNames(changeTypes)
	if len(funcNames) > 0 {
		area := formatFunctionNames(funcNames)
		return verb + " " + area + " in " + module
	}

	// Fall back to symbol names or file paths
	area := determineArea(symbols, changedFiles)

	return verb + " " + module + " " + area
}

// Placeholder to satisfy import
var _ = util.NowMs
