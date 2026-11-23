package configs

import _ "embed"

// DefaultFlowJSON is the embedded default SemEngine flow configuration for bbops.
//
//go:embed bbops.json
var DefaultFlowJSON []byte
