package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// OutputFormat is the rendering mode picked by --format. Two formats
// today: human-readable (default) and JSON for jq pipelines / scripts.
// Adding a new value (yaml, csv, ...) here is the one place to change
// — every consumer reads through ParseOutputFormat below.
type OutputFormat string

const (
	FormatTable OutputFormat = "table" // human-readable for terminals
	FormatJSON  OutputFormat = "json"  // raw JSON for scripts and jq
)

// ParseOutputFormat normalises a user-supplied --format value. Empty
// input yields FormatTable; anything else must match a known constant
// (case-insensitive) or it's an error.
func ParseOutputFormat(s string) (OutputFormat, error) {
	switch s {
	case "", "table", "TABLE":
		return FormatTable, nil
	case "json", "JSON":
		return FormatJSON, nil
	}
	return "", fmt.Errorf("unknown --format %q (want %q or %q)", s, FormatTable, FormatJSON)
}

// addFormatFlag registers --format on cmd, writing into target. Every
// command that supports a JSON path uses this so the flag's name,
// default, and help string stay identical across the CLI.
func addFormatFlag(cmd *cobra.Command, target *string) {
	cmd.Flags().StringVar(target, "format", "table",
		"output format: table (human-readable) or json (for jq / scripts)")
}

// emitJSON encodes v as indented JSON, suitable for machine
// consumption and human inspection alike. Trailing newline so output
// concatenates cleanly with shell prompts.
func emitJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
