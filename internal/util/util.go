// Package util re-exports content-addressable storage utilities from kai-core.
package util

import "github.com/kaicontext/kai-core/cas"

// Re-export functions from kai-core/cas
var (
	NowMs         = cas.NowMs
	CanonicalJSON = cas.CanonicalJSON
	Blake3Hash    = cas.Blake3Hash
	Blake3HashHex = cas.Blake3HashHex
	NodeID        = cas.NodeID
	NodeIDHex     = cas.NodeIDHex
	HexToBytes    = cas.HexToBytes
	BytesToHex    = cas.BytesToHex
)
