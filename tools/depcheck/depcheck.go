// Package depcheck verifies the dependency-direction invariants
// the architecture relies on. Catches accidental imports that
// would couple a package to a layer it isn't supposed to know
// about (e.g. internal/wire importing database/sql, internal/apiclient
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
	"path/filepath"
	"regexp"
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
		From:      "github.com/toabctl/aichronicles/internal/events",
		Forbidden: []string{"database/sql", "net/http"},
		Reason:    "internal/events is the domain core; SQL and HTTP belong to adapters",
	},
	{
		From: "github.com/toabctl/aichronicles/internal/events",
		Forbidden: []string{
			"github.com/toabctl/aichronicles/internal/store",
			"github.com/toabctl/aichronicles/internal/api",
			"github.com/toabctl/aichronicles/internal/apiclient",
			"github.com/toabctl/aichronicles/internal/cli",
		},
		Reason: "internal/events must not depend on any other internal package",
	},
	{
		From:      "github.com/toabctl/aichronicles/internal/wire",
		Forbidden: []string{"database/sql", "net/http"},
		Reason:    "internal/wire is wire types only; SQL/HTTP belong to internal/api",
	},
	{
		From: "github.com/toabctl/aichronicles/internal/wire",
		Forbidden: []string{
			"github.com/toabctl/aichronicles/internal/store",
			"github.com/toabctl/aichronicles/internal/api",
			"github.com/toabctl/aichronicles/internal/apiclient",
			"github.com/toabctl/aichronicles/internal/cli",
		},
		Reason: "internal/wire must not depend on any other internal package",
	},
	{
		From:      "github.com/toabctl/aichronicles/internal/apiclient",
		Forbidden: []string{"github.com/toabctl/aichronicles/internal/store"},
		Reason:    "apiclient must not import internal/store; it is a wire-only client",
	},
}

// callRule expresses a code-pattern invariant: in non-test files
// under Dir, the regex Forbidden must not appear. Used to enforce
// "all reads/writes go through apiclient" without forbidding the
// package from importing internal/store entirely (cli still
// legitimately imports type/enum constants from store).
//
// internal/api is intentionally NOT in this list. The api daemon
// is the store-binding side of the wire boundary by design — its
// handlers translate HTTP into store.Load/Save calls and project
// rows to wire types. See the package doc on internal/api/server.go
// for the rationale.
type callRule struct {
	Dir       string         // directory relative to repo root
	Forbidden *regexp.Regexp // pattern that signals a violation
	Reason    string
}

var callRules = []callRule{
	{
		Dir: "internal/cli",
		Forbidden: regexp.MustCompile(
			`\bstore\.(Load|Save|Insert|Update|Delete|Has|Last|Query|Vacuum|Segment)\w*\(`),
		Reason: "internal/cli must read/write through apiclient, not store directly (test files exempt)",
	},
	{
		// MCP runs in a SEPARATE process from aichronicles-api
		// (it's stdio-attached to the host editor, not embedded in
		// the daemon). Per the read-access policy: cross-process
		// surfaces go through the wire. Tests are exempt because
		// they spin up an in-process apiclient against an httptest
		// server backed by a temp *store.Store — same-process,
		// share-handle is fine.
		Dir: "internal/mcp",
		Forbidden: regexp.MustCompile(
			`\bstore\.(Load|Save|Insert|Update|Delete|Has|Last|Query|Vacuum|Segment)\w*\(`),
		Reason: "internal/mcp must read/write through apiclient (cross-process); test files exempt",
	},
	{
		// internal/web is a separate process (aichronicles-web.service),
		// same blast-radius reasoning as MCP. Web reads through
		// internal/apiclient against the api daemon's UDS. Tests
		// exercise the wire path through an httptest.Server fronting
		// a temp *store.Store; non-test files must not reach for the
		// store directly.
		Dir: "internal/web",
		Forbidden: regexp.MustCompile(
			`\bstore\.(Load|Save|Insert|Update|Delete|Has|Last|Query|Vacuum|Segment)\w*\(`),
		Reason: "internal/web must read/write through apiclient (cross-process); test files exempt",
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
	root, err := moduleRoot()
	if err != nil {
		return fmt.Errorf("locate module root: %w", err)
	}
	for _, r := range callRules {
		hits, err := scanForbiddenCalls(filepath.Join(root, r.Dir), r.Forbidden)
		if err != nil {
			return fmt.Errorf("scan %s: %w", r.Dir, err)
		}
		for _, h := range hits {
			violations = append(violations,
				fmt.Sprintf("✗ %s: forbidden call %q\n  reason: %s",
					h.location, h.match, r.Reason))
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

type callHit struct {
	location string // file:line
	match    string
}

// moduleRoot returns the directory containing go.mod, so callRules
// can be defined relative to the module root and resolved no matter
// where depcheck runs from (`go run ./tools/depcheck` from root vs
// `go test ./tools/depcheck` inside the package dir).
func moduleRoot() (string, error) {
	cmd := exec.Command("go", "env", "GOMOD")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go env GOMOD: %w (stderr=%q)", err, stderr.String())
	}
	gomod := strings.TrimSpace(stdout.String())
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("not in a module (go env GOMOD = %q)", gomod)
	}
	return filepath.Dir(gomod), nil
}

// scanForbiddenCalls walks dir's non-test .go files (top level only —
// the architecture rule is per-package, not recursive) and returns
// every line that matches the forbidden pattern. Test files are
// exempt because they exercise the store directly to set up
// fixtures and verify state.
func scanForbiddenCalls(dir string, pat *regexp.Regexp) ([]callHit, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}
	var hits []callHit
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var i int
		for line := range strings.SplitSeq(string(data), "\n") {
			i++
			// Skip comments — references in doc comments aren't
			// real call sites.
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if m := pat.FindString(line); m != "" {
				hits = append(hits, callHit{
					location: fmt.Sprintf("%s:%d", path, i),
					match:    m,
				})
			}
		}
	}
	return hits, nil
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
	for line := range strings.SplitSeq(strings.TrimSpace(stdout.String()), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == pkgPath {
			continue
		}
		out[line] = struct{}{}
	}
	return out, nil
}
