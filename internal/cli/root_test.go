package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestExecuteCmd_PrintsErrorToStderr locks down the contract that the
// root surfaces RunE errors to stderr instead of silently exiting 1.
// Cobra's SilenceErrors is on so we don't double-print; Execute() owns
// the printing. Without this every subcommand failure looks like a
// bare rc=1 with no diagnostic.
func TestExecuteCmd_PrintsErrorToStderr(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("synthetic failure")

	root := &cobra.Command{
		Use:           "aichronicles",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(&cobra.Command{
		Use: "boom",
		RunE: func(_ *cobra.Command, _ []string) error {
			return sentinel
		},
	})
	root.SetArgs([]string{"boom"})

	var stderr bytes.Buffer
	code := executeCmd(root, &stderr)

	if code != 1 {
		t.Errorf("exit code: got %d, want 1", code)
	}
	got := stderr.String()
	if !strings.HasPrefix(got, "aichronicles:") {
		t.Errorf("stderr should start with 'aichronicles:' prefix, got %q", got)
	}
	if !strings.Contains(got, "synthetic failure") {
		t.Errorf("stderr should contain the underlying error, got %q", got)
	}
}

// TestExecuteCmd_CleanRunIsSilent proves we don't prepend our prefix
// on the happy path, so successful commands keep producing only their
// own output.
func TestExecuteCmd_CleanRunIsSilent(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{Use: "aichronicles"}
	root.AddCommand(&cobra.Command{
		Use:  "ok",
		RunE: func(_ *cobra.Command, _ []string) error { return nil },
	})
	root.SetArgs([]string{"ok"})

	var stderr bytes.Buffer
	code := executeCmd(root, &stderr)

	if code != 0 {
		t.Errorf("exit code: got %d, want 0", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr should be empty on success, got %q", stderr.String())
	}
}
