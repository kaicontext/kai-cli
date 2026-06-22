// Package classify re-exports change detection from kai-core.
package classify

import (
	"github.com/kaicontext/kai-core/detect"
	coregraph "github.com/kaicontext/kai-core/graph"
)

// Re-export types from kai-core/detect
type ChangeCategory = detect.ChangeCategory
type FileRange = detect.FileRange
type Evidence = detect.Evidence
type ChangeType = detect.ChangeType
type JSONSymbol = detect.JSONSymbol
type YAMLSymbol = detect.YAMLSymbol

// Re-export new signal types
type ChangeSignal = detect.ChangeSignal
type ExtendedEvidence = detect.ExtendedEvidence
type SignatureChange = detect.SignatureChange

// Detector wraps kai-core/detect.Detector to use local graph.Node type
type Detector struct {
	inner *detect.Detector
}

// NewDetector creates a new change detector.
func NewDetector() *Detector {
	return &Detector{inner: detect.NewDetector()}
}

// SetSymbols sets the symbols for a file (used for mapping changes to symbols).
func (d *Detector) SetSymbols(fileID string, symbols []*coregraph.Node) {
	d.inner.SetSymbols(fileID, symbols)
}

// DetectChanges detects all change types between two versions of a file.
// Optional lang parameter specifies the language for proper parsing.
func (d *Detector) DetectChanges(path string, beforeContent, afterContent []byte, fileID string, lang ...string) ([]*ChangeType, error) {
	return d.inner.DetectChanges(path, beforeContent, afterContent, fileID, lang...)
}

// DetectFileChange creates a FILE_CONTENT_CHANGED for non-parseable files.
func (d *Detector) DetectFileChange(path string, lang string) *ChangeType {
	return d.inner.DetectFileChange(path, lang)
}

// Re-export constants from kai-core/detect
const (
	ConditionChanged   = detect.ConditionChanged
	ConstantUpdated    = detect.ConstantUpdated
	APISurfaceChanged  = detect.APISurfaceChanged
	FunctionAdded      = detect.FunctionAdded
	FunctionRemoved    = detect.FunctionRemoved
	FileContentChanged = detect.FileContentChanged
	FileAdded          = detect.FileAdded
	FileDeleted        = detect.FileDeleted
	JSONFieldAdded     = detect.JSONFieldAdded
	JSONFieldRemoved   = detect.JSONFieldRemoved
	JSONValueChanged   = detect.JSONValueChanged
	JSONArrayChanged   = detect.JSONArrayChanged
	YAMLKeyAdded       = detect.YAMLKeyAdded
	YAMLKeyRemoved     = detect.YAMLKeyRemoved
	YAMLValueChanged   = detect.YAMLValueChanged

	// New enhanced categories
	FunctionRenamed     = detect.FunctionRenamed
	FunctionBodyChanged = detect.FunctionBodyChanged
	ParameterAdded      = detect.ParameterAdded
	ParameterRemoved    = detect.ParameterRemoved
	ImportAdded         = detect.ImportAdded
	ImportRemoved       = detect.ImportRemoved
	DependencyAdded     = detect.DependencyAdded
	DependencyRemoved   = detect.DependencyRemoved
	DependencyUpdated   = detect.DependencyUpdated
)

// Re-export functions from kai-core/detect
var (
	GetCategoryPayload = detect.GetCategoryPayload
	NewFileChange      = detect.NewFileChange
	IsParseable        = detect.IsParseable
	ExtractJSONSymbols = detect.ExtractJSONSymbols
	DetectJSONChanges  = detect.DetectJSONChanges
	FormatJSONPath     = detect.FormatJSONPath
	IsPackageJSON      = detect.IsPackageJSON
	IsTSConfig         = detect.IsTSConfig
	ExtractYAMLSymbols = detect.ExtractYAMLSymbols
	DetectYAMLChanges  = detect.DetectYAMLChanges
	FormatYAMLPath     = detect.FormatYAMLPath

	// New signal functions
	NewChangeSignal       = detect.NewChangeSignal
	ConvertToSignals      = detect.ConvertToSignals
	GetSignalPayload      = detect.GetSignalPayload
	DetectDependencyChanges = detect.DetectDependencyChanges
)

// NewRenameDetector creates a rename detector.
func NewRenameDetector() *detect.RenameDetector {
	return detect.NewRenameDetector()
}
