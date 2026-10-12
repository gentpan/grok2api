// Command grok2api is the OpenAI/Anthropic-compatible API gateway for Grok.
//
// All wiring, scheduling and shutdown lives in internal/app; this file only
// carries what the binary itself knows (version, embedded default config).
package main

import (
	_ "embed"
	"os"

	"github.com/gentpan/grok2api/internal/app"
)

// Version is the project version (overridden at build time via -ldflags).
var Version = "dev"

// embeddedDefaults is used when config.defaults.toml is not present next to
// the working directory (e.g. a bare binary or the Docker image).
//
//go:embed config.defaults.toml
var embeddedDefaults []byte

func main() {
	os.Exit(app.Main(app.Options{
		Version:          Version,
		EmbeddedDefaults: embeddedDefaults,
	}))
}
