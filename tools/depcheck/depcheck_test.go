package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestRulesAllPass is the production smoke test: today, the
// dependency-direction invariants hold. Any commit that breaks
// one trips this test in CI before code review.
func TestRulesAllPass(t *testing.T) {
	t.Parallel()
	if err := run(); err != nil {
		t.Errorf("depcheck failed: %v", err)
	}
}

// TestDeps_DetectsKnownImport seeds a check against a known-good
// package and asserts the engine surfaces the import. internal/
// store transitively imports database/sql (it IS the SQL
// adapter); used here as a positive control so a regression that
// silently drops imports trips this test before it reaches CI.
func TestDeps_DetectsKnownImport(t *testing.T) {
	t.Parallel()
	got, err := deps("github.com/toabctl/aichronicles/internal/store", false)
	if err != nil {
		t.Fatalf("deps: %v", err)
	}
	if _, ok := got["database/sql"]; !ok {
		t.Errorf("expected internal/store to transitively import database/sql; got %d entries", len(got))
	}
}

// TestRulesProvideUsefulErrors makes sure each rule's Reason is
// non-empty so a CI failure doesn't dump a bare "X imports Y"
// without context.
func TestRulesProvideUsefulErrors(t *testing.T) {
	t.Parallel()
	for _, r := range rules {
		if strings.TrimSpace(r.Reason) == "" {
			t.Errorf("rule %s -> %v has empty Reason", r.From, r.Forbidden)
		}
	}
	for _, r := range callRules {
		if strings.TrimSpace(r.Reason) == "" {
			t.Errorf("call rule %s -> %v has empty Reason", r.Dir, r.Forbidden)
		}
	}
}

// TestScanForbiddenCalls_DetectsViolation seeds a temp dir with one
// .go file containing a forbidden call and one _test.go file with
// the same call (which must be skipped). Acts as a positive control
// for the call-pattern engine: if a regression silently no-ops the
// scan, this trips before reaching CI.
func TestScanForbiddenCalls_DetectsViolation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.go"),
		[]byte("package x\n\nfunc f() { _ = store.LoadLLMOutputs(nil) }\n"), 0o600); err != nil {
		t.Fatalf("write bad.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad_test.go"),
		[]byte("package x\n\nfunc g() { _ = store.LoadLLMOutputs(nil) }\n"), 0o600); err != nil {
		t.Fatalf("write bad_test.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "doc.go"),
		[]byte("package x\n\n// store.LoadLLMOutputs lives in the comment, must be skipped\n"), 0o600); err != nil {
		t.Fatalf("write doc.go: %v", err)
	}
	pat := regexp.MustCompile(`\bstore\.(Load|Save)\w*\(`)
	hits, err := scanForbiddenCalls(dir, pat, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected exactly 1 hit (bad.go), got %d: %+v", len(hits), hits)
	}
	if !strings.HasSuffix(hits[0].location, "bad.go:3") {
		t.Errorf("expected hit at bad.go:3, got %q", hits[0].location)
	}
	if hits[0].match != "store.LoadLLMOutputs(" {
		t.Errorf("expected match %q, got %q", "store.LoadLLMOutputs(", hits[0].match)
	}
}

// TestScanForbiddenCalls_HonoursExemptions pins the escape hatch the
// .DB() rule depends on.
//
// Two maintenance commands legitimately hold their own SQLite handle
// (backfill re-derives extractions with its own SQL; scrub rewrites
// rows in place) and both refuse to run while the daemon is up. The
// exemption list is explicit filenames rather than a pattern so each
// entry is a deliberate, reviewable decision.
func TestScanForbiddenCalls_HonoursExemptions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const body = "package p\n\nfunc f() { _ = s.DB() }\n"
	for _, name := range []string{"guarded.go", "exempt.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	pat := regexp.MustCompile(`\.DB\(\)`)

	all, err := scanForbiddenCalls(dir, pat, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected both files to hit, got %d", len(all))
	}

	partial, err := scanForbiddenCalls(dir, pat, []string{"exempt.go"})
	if err != nil {
		t.Fatalf("scan with exemption: %v", err)
	}
	if len(partial) != 1 {
		t.Fatalf("expected 1 hit after exempting, got %d", len(partial))
	}
	if !strings.Contains(partial[0].location, "guarded.go") {
		t.Errorf("wrong file reported: %s", partial[0].location)
	}
}

// TestCLIHasNoDirectDBHandle is the live assertion: the rule must
// actually hold against the tree, not merely be expressible.
func TestCLIHasNoDirectDBHandle(t *testing.T) {
	t.Parallel()
	root, err := moduleRoot()
	if err != nil {
		t.Fatalf("moduleRoot: %v", err)
	}
	var rule *callRule
	for i := range callRules {
		if callRules[i].Dir == "internal/cli" &&
			callRules[i].Forbidden.String() == `\.DB\(\)` {
			rule = &callRules[i]
		}
	}
	if rule == nil {
		t.Fatal("the internal/cli .DB() rule is missing from callRules")
	}
	hits, err := scanForbiddenCalls(filepath.Join(root, rule.Dir), rule.Forbidden, rule.ExemptFiles)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, h := range hits {
		t.Errorf("internal/cli holds a direct *sql.DB at %s (%s)", h.location, h.match)
	}
}
