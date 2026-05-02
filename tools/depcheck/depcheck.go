// Package depcheck verifies the dependency-direction invariants
// the architecture relies on. Catches accidental imports that
// would couple a package to a layer it isn't supposed to know
// about (e.g. pkg/api importing database/sql, internal/apiclient
// importing internal/store).
//
// Run via `go run ./tools/depcheck` from CI; exits non-zero with
// a list of violations on the first failure.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// rule expresses one constraint: pkg "from" must NOT (transitively
// or directly, depending on Direct) import any package whose
// import path is in Forbidden.
type rule struct {
	From      string
	Forbidden []string
	// Direct=true checks only direct imports of From; the default
	// is to walk the transitive closure of internal-only deps so
	// indirect violations also trip.
	Direct bool
	// Reason is shown in the error output to make a CI failure
	// self-explanatory.
	Reason string
}

var rules = []rule{
	{
		From:      "github.com/toabctl/aichronicles/pkg/events",
		Forbidden: []string{"database/sql", "net/http"},
		Reason:    "pkg/events is the domain core; SQL and HTTP belong to adapters",
	},
	{
		From: "github.com/toabctl/aichronicles/pkg/events",
		Forbidden: []string{
			"github.com/toabctl/aichronicles/internal/store",
			"github.com/toabctl/aichronicles/internal/api",
			"github.com/toabctl/aichronicles/internal/apiclient",
			"github.com/toabctl/aichronicles/internal/cli",
		},
		Reason: "pkg/events must not depend on any internal/* package",
	},
	{
		From:      "github.com/toabctl/aichronicles/pkg/api",
		Forbidden: []string{"database/sql", "net/http"},
		Reason:    "pkg/api is wire types only; SQL/HTTP belong to internal/api",
	},
	{
		From: "github.com/toabctl/aichronicles/pkg/api",
		Forbidden: []string{
			"github.com/toabctl/aichronicles/internal/store",
			"github.com/toabctl/aichronicles/internal/api",
			"github.com/toabctl/aichronicles/internal/apiclient",
			"github.com/toabctl/aichronicles/internal/cli",
		},
		Reason: "pkg/api must not depend on any internal/* package",
	},
	{
		From:      "github.com/toabctl/aichronicles/internal/apiclient",
		Forbidden: []string{"github.com/toabctl/aichronicles/internal/store"},
		Reason:    "apiclient must not import internal/store; it is a wire-only client",
	},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var violations []string
	for _, r := range rules {
		got, err := deps(r.From, r.Direct)
		if err != nil {
			return fmt.Errorf("collect deps for %s: %w", r.From, err)
		}
		for _, forbidden := range r.Forbidden {
			if _, hit := got[forbidden]; hit {
				violations = append(violations,
					fmt.Sprintf("✗ %s imports %s\n  reason: %s",
						r.From, forbidden, r.Reason))
			}
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		return fmt.Errorf("dependency-direction violations:\n%s",
			strings.Join(violations, "\n"))
	}
	fmt.Println("depcheck: all dependency-direction invariants hold")
	return nil
}

// deps returns the set of imports of `pkgPath` as resolved by
// `go list`. Module-aware (works under go.mod) where the legacy
// go/build package falters. When direct=true only the direct
// imports are returned ({{.Imports}}); otherwise the transitive
// closure ({{.Deps}}) is returned.
func deps(pkgPath string, direct bool) (map[string]struct{}, error) {
	tmpl := "{{range .Deps}}{{.}}\n{{end}}"
	if direct {
		tmpl = "{{range .Imports}}{{.}}\n{{end}}"
	}
	cmd := exec.Command("go", "list", "-f", tmpl, pkgPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list %s: %w (stderr=%q)", pkgPath, err, stderr.String())
	}
	out := map[string]struct{}{}
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == pkgPath {
			continue
		}
		out[line] = struct{}{}
	}
	return out, nil
}
