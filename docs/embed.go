// Package docs exposes the files in this directory that Go code must read at build time.
package docs

import _ "embed"

// ConfigSchema is docs/lumencortex.schema.json, the one schema for lumencortex.json. Package
// config validates every file it loads or saves against these exact bytes.
//
//go:embed lumencortex.schema.json
var ConfigSchema []byte
