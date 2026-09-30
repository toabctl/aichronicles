package store

import (
	"errors"
	"fmt"
	"testing"
)

// TestWriteErrorClassification pins which write failures count as the
// caller's fault. Handlers turn IsUnknownReference / IsInvalidValue
// into 400s and everything else into 500s, so a misclassification
// either blames the client for a storage fault or hides bad input
// behind "Storage error".
func TestWriteErrorClassification(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	db := s.DB()
	for _, q := range []string{
		`INSERT INTO sessions(id, source_agent, source_session_id) VALUES ('known', 'a', 'b')`,
		`CREATE TEMP TABLE guarded(x)`,
		`CREATE TEMP TRIGGER guard BEFORE INSERT ON guarded BEGIN SELECT RAISE(ABORT, 'refused'); END`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}
	exec := func(q string) error {
		_, err := db.Exec(q)
		if err == nil {
			t.Fatalf("expected %q to fail", q)
		}
		return fmt.Errorf("wrapped by a caller: %w", err)
	}
	cases := []struct {
		name         string
		err          error
		invalid, ref bool
	}{
		{"foreign key", exec(`INSERT INTO session_outcomes(session_id, computed_at_ms, outcome) VALUES ('ghost', 1, 'unknown')`), false, true},
		{"check", exec(`INSERT INTO session_outcomes(session_id, computed_at_ms, outcome) VALUES ('known', 1, 'bogus')`), true, false},
		{"not null", exec(`INSERT INTO session_outcomes(session_id, outcome) VALUES ('known', 'unknown')`), true, false},
		{"trigger", exec(`INSERT INTO guarded VALUES (1)`), true, false},
		{"unique is neither", exec(`INSERT INTO sessions(id, source_agent, source_session_id) VALUES ('other', 'a', 'b')`), false, false},
		{"store validation", invalidf("SaveX: field %s is required", "y"), true, false},
		{"plain error", errors.New("disk I/O error"), false, false},
	}
	for _, tc := range cases {
		if got := IsInvalidValue(tc.err); got != tc.invalid {
			t.Errorf("%s: IsInvalidValue=%v, want %v (%v)", tc.name, got, tc.invalid, tc.err)
		}
		if got := IsUnknownReference(tc.err); got != tc.ref {
			t.Errorf("%s: IsUnknownReference=%v, want %v (%v)", tc.name, got, tc.ref, tc.err)
		}
	}
}
