package main

import (
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
}
