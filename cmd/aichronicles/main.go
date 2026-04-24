// aichronicles is the client binary: the `ingest` subcommand is invoked
// by Claude Code hooks; `setup claude-code` installs those hooks.
package main

import (
	"os"

	"github.com/toabctl/aichronicles/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
