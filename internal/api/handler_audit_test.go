package api

import (
	"strings"
	"testing"
)

// TestBuildAuditQuery_AlwaysIncludesLIMIT pins the ceiling: every
// call site (including the limit=0 default the handler now maps
// to auditMaxRowsCeiling) must produce a query with a LIMIT clause.
// Without the clamp the handler streamed every row in `events`
// through redact.Scanner — hundreds of MB of regex work on a real
// corpus, plus SQLite write-lock contention while the scan held.
func TestBuildAuditQuery_AlwaysIncludesLIMIT(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		since int64
		limit int
	}{
		{"no since, ceiling limit", 0, auditMaxRowsCeiling},
		{"with since, ceiling limit", 1_700_000_000_000, auditMaxRowsCeiling},
		{"small client-supplied limit", 0, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q, args := buildAuditQuery(tc.since, tc.limit)
			if !strings.Contains(q, "LIMIT ?") {
				t.Errorf("query missing LIMIT clause:\n%s", q)
			}
			// Last bound argument must be the limit so SQLite sees
			// the clamp.
			if len(args) == 0 {
				t.Fatalf("args empty")
			}
			gotLimit, ok := args[len(args)-1].(int)
			if !ok {
				t.Fatalf("last arg should be int limit; got %T", args[len(args)-1])
			}
			if gotLimit != tc.limit {
				t.Errorf("LIMIT bound: got %d want %d", gotLimit, tc.limit)
			}
		})
	}
}
