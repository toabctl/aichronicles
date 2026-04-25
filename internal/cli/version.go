package cli

// Version is the human-readable release tag. Both binaries
// (aichronicles, aichroniclesd) wire this into their cobra
// command's Version field, which gives them a --version flag.
//
// Declared as a var so build tooling can override it via
// `-ldflags '-X github.com/toabctl/aichronicles/internal/cli.Version=...'`
// — the Makefile injects `git describe --tags --always --dirty`
// at build time so a built binary always carries its provenance.
var Version = "0.0.0-dev"
