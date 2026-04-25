package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Version is the human-readable release tag. Today there are no
// releases yet; we treat all commits on main as 0.0.0-dev. The
// version subcommand pairs this string with the VCS-stamped commit
// from runtime/debug.BuildInfo so the user always sees both.
const Version = "0.0.0-dev"

// newVersionCmd prints the build version, commit, and Go toolchain.
// `aichronicles --version` (set on the root command's Version field)
// gives a one-liner; `aichronicles version` prints the long form.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build version, commit, and Go toolchain",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "aichronicles %s\n", Version)
			commit, dirty, when := vcsInfo()
			if commit != "" {
				suffix := ""
				if dirty {
					suffix = " (dirty)"
				}
				_, _ = fmt.Fprintf(out, "  commit:  %s%s\n", commit, suffix)
			}
			if when != "" {
				_, _ = fmt.Fprintf(out, "  built:   %s\n", when)
			}
			_, _ = fmt.Fprintf(out, "  go:      %s %s/%s\n",
				runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return nil
		},
	}
}

// vcsInfo extracts the commit, dirty flag, and build timestamp that
// the Go toolchain stamps into every binary built from a git checkout.
// Empty strings when the binary was built outside a VCS — `go run`
// usually carries the data; rare custom build pipelines may not.
func vcsInfo() (commit string, dirty bool, when string) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false, ""
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			commit = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		case "vcs.time":
			when = s.Value
		}
	}
	return commit, dirty, when
}
