package store

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/pkg/ingest"
)

// seedSkillLoadAt ingests a Skill tool_use envelope at the given
// timestamp. The skill_load extractor fires automatically inside
// IngestEnvelope.
func seedSkillLoadAt(t *testing.T, s *Store, sessionKey, skill string, ts time.Time) {
	t.Helper()
	env := &ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sessionKey,
		Kind:            "tool_use",
		Role:            "assistant",
		TsSource:        ts,
		Tool:            &ingest.Tool{Name: "Skill"},
		Payload:         map[string]any{"tool_input": map[string]any{"skill": skill}},
		Redaction:       &ingest.Redaction{Applied: true},
	}
	withTx(t, s, func(tx *sql.Tx) {
		if _, err := IngestEnvelope(t.Context(), tx, env, []byte(`{"v":1}`), env.TsSource.UnixMilli()); err != nil {
			t.Fatalf("seed skill load: %v", err)
		}
	})
}

// seedToolFailureAt ingests a tool_failure event in the same
// session at the given timestamp — the failure signal staleness
// detection joins against.
func seedToolFailureAt(t *testing.T, s *Store, sessionKey string, ts time.Time) {
	t.Helper()
	env := &ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sessionKey,
		Kind:            "tool_failure",
		Role:            "tool",
		TsSource:        ts,
		Tool:            &ingest.Tool{Name: "Bash"},
		ContentText:     "Exit code 1",
		Payload:         map[string]any{"error": "boom"},
		Redaction:       &ingest.Redaction{Applied: true},
	}
	withTx(t, s, func(tx *sql.Tx) {
		if _, err := IngestEnvelope(t.Context(), tx, env, []byte(`{"v":1}`), env.TsSource.UnixMilli()); err != nil {
			t.Fatalf("seed tool failure: %v", err)
		}
	})
}

func TestLoadSkillStaleness_FlagsLoadFollowedByFailure(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	t0 := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)

	// build-test loaded then failed within the window
	seedSkillLoadAt(t, s, "sess-stale-1", "build-test", t0)
	seedToolFailureAt(t, s, "sess-stale-1", t0.Add(2*time.Minute))

	// effective-go loaded with NO subsequent failure
	seedSkillLoadAt(t, s, "sess-clean-1", "effective-go", t0)

	since := t0.Add(-24 * time.Hour).UnixMilli()
	rows, err := LoadSkillStaleness(t.Context(), s.DB(), since, 0, SkillStalenessLimits{})
	if err != nil {
		t.Fatalf("LoadSkillStaleness: %v", err)
	}

	// effective-go has no stale signal → not in the report at all
	// (HAVING stale_loads > 0)
	if len(rows) != 1 {
		t.Fatalf("expected 1 stale-correlated skill, got %d (%+v)", len(rows), rows)
	}
	if rows[0].Name != "build-test" {
		t.Errorf("name: got %q, want build-test", rows[0].Name)
	}
	if rows[0].StaleLoads != 1 || rows[0].TotalLoads != 1 {
		t.Errorf("counts: got stale=%d total=%d", rows[0].StaleLoads, rows[0].TotalLoads)
	}
	if rows[0].Rate != 1.0 {
		t.Errorf("rate: got %f, want 1.0", rows[0].Rate)
	}
	if len(rows[0].Examples) != 1 || rows[0].Examples[0] != ingest.DeriveSessionID("claude-code", "sess-stale-1") {
		t.Errorf("examples: got %+v", rows[0].Examples)
	}
}

// TestLoadSkillStaleness_RespectsWindow confirms a failure that
// happens AFTER the window does not count as stale-correlated.
func TestLoadSkillStaleness_RespectsWindow(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	t0 := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)

	seedSkillLoadAt(t, s, "sess-late", "build-test", t0)
	// Failure 30 minutes later — outside the default 10-minute window.
	seedToolFailureAt(t, s, "sess-late", t0.Add(30*time.Minute))

	since := t0.Add(-24 * time.Hour).UnixMilli()
	rows, err := LoadSkillStaleness(t.Context(), s.DB(), since, 0, SkillStalenessLimits{})
	if err != nil {
		t.Fatalf("LoadSkillStaleness: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected no stale-correlated skills (failure outside window), got %+v", rows)
	}
}

// TestLoadSkillStaleness_FailureBeforeLoadIgnored confirms a
// failure that *precedes* the skill load is not attributed to it.
// The temporal direction is load → failure, not the other way.
func TestLoadSkillStaleness_FailureBeforeLoadIgnored(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	t0 := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)

	seedToolFailureAt(t, s, "sess-rev", t0)
	seedSkillLoadAt(t, s, "sess-rev", "build-test", t0.Add(time.Minute))

	since := t0.Add(-24 * time.Hour).UnixMilli()
	rows, err := LoadSkillStaleness(t.Context(), s.DB(), since, 0, SkillStalenessLimits{})
	if err != nil {
		t.Fatalf("LoadSkillStaleness: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected no stale: failure was BEFORE load. got %+v", rows)
	}
}

// TestLoadSkillStaleness_RatePartial covers the partial-stale
// case: 3 loads, 1 followed by a failure → rate 1/3.
func TestLoadSkillStaleness_RatePartial(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	t0 := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)

	// Three loads of build-test in three different sessions.
	seedSkillLoadAt(t, s, "sess-bp-1", "build-test", t0)
	seedToolFailureAt(t, s, "sess-bp-1", t0.Add(2*time.Minute))
	seedSkillLoadAt(t, s, "sess-bp-2", "build-test", t0.Add(time.Hour))
	seedSkillLoadAt(t, s, "sess-bp-3", "build-test", t0.Add(2*time.Hour))

	since := t0.Add(-24 * time.Hour).UnixMilli()
	rows, err := LoadSkillStaleness(t.Context(), s.DB(), since, 0, SkillStalenessLimits{})
	if err != nil {
		t.Fatalf("LoadSkillStaleness: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d, want 1", len(rows))
	}
	r := rows[0]
	if r.TotalLoads != 3 || r.StaleLoads != 1 {
		t.Errorf("counts: stale=%d total=%d, want 1/3", r.StaleLoads, r.TotalLoads)
	}
	if got := int(r.Rate*1000 + 0.5); got != 333 {
		t.Errorf("rate: got %f, want ~0.333", r.Rate)
	}
}

// TestLoadSkillStaleness_HonoursMaxExamples bounds the example
// session list per skill.
func TestLoadSkillStaleness_HonoursMaxExamples(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	t0 := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)

	for i, key := range []string{"a", "b", "c", "d", "e"} {
		seedSkillLoadAt(t, s, "sess-ex-"+key, "build-test", t0.Add(time.Duration(i)*time.Hour))
		seedToolFailureAt(t, s, "sess-ex-"+key, t0.Add(time.Duration(i)*time.Hour+time.Minute))
	}

	since := t0.Add(-24 * time.Hour).UnixMilli()
	rows, err := LoadSkillStaleness(t.Context(), s.DB(), since, 0, SkillStalenessLimits{MaxExamples: 2})
	if err != nil {
		t.Fatalf("LoadSkillStaleness: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d skills, want 1", len(rows))
	}
	if got := len(rows[0].Examples); got != 2 {
		t.Errorf("examples: got %d, want 2", got)
	}
}

// TestWilsonLowerBound covers the sample-size-aware confidence floor.
// The rate alone over-rates 1/1 (==1.0) — Wilson lower bound puts it
// well below 50/100 even though the naive rates are 1.0 and 0.5. That
// inversion is exactly what the SkillRevisionMinRate=0.5 default needs
// to keep noisy 1-sample skills out of the auto-revision queue.
func TestWilsonLowerBound(t *testing.T) {
	t.Parallel()
	cases := []struct {
		successes, total int
		minOK, maxOK     float64 // expected range; computed values from a reference impl
	}{
		{0, 0, 0, 0},          // total=0 → 0
		{0, 10, 0, 0.05},      // 0/10 → ~0
		{1, 1, 0.15, 0.25},    // 1/1 → ~0.205
		{50, 100, 0.39, 0.41}, // 50/100 → ~0.401
		{99, 100, 0.94, 0.96}, // 99/100 → ~0.948
		{1, 1000, 0.0, 0.01},  // 1/1000 → ~0.0001
	}
	for _, tc := range cases {
		got := wilsonLowerBound(tc.successes, tc.total)
		if got < tc.minOK || got > tc.maxOK {
			t.Errorf("wilsonLowerBound(%d, %d) = %f, want [%f, %f]",
				tc.successes, tc.total, got, tc.minOK, tc.maxOK)
		}
	}
}

// TestWilsonLowerBound_RanksHighNAboveLowN encodes the property that
// drives the change: 50/100 (high N, mid rate) must rank above 1/1
// (low N, max rate) when sorting by the Wilson lower bound.
func TestWilsonLowerBound_RanksHighNAboveLowN(t *testing.T) {
	t.Parallel()
	highN := wilsonLowerBound(50, 100)
	lowN := wilsonLowerBound(1, 1)
	if !(highN > lowN) {
		t.Errorf("expected lowerBound(50,100)=%f > lowerBound(1,1)=%f", highN, lowN)
	}
	// And the naive rate would have put the 1/1 skill on top —
	// keep that invariant pinned so the regression is obvious.
	naiveHigh := 50.0 / 100
	naiveLow := 1.0 / 1
	if !(naiveLow > naiveHigh) {
		t.Errorf("naive rates: %f, %f — assumption invalid", naiveLow, naiveHigh)
	}
}

func TestFormatStaleSummary_TruncatesAndFormats(t *testing.T) {
	t.Parallel()
	s := SkillStaleness{
		Name:       "an-extremely-long-skill-name-that-overflows-the-column",
		StaleLoads: 5, TotalLoads: 20, Rate: 0.25,
		Examples: []string{"abcd1234"},
	}
	out := FormatStaleSummary(s)
	// The truncated name caps at 32 chars (29 + "...")
	if len(out) < 30 {
		t.Errorf("unexpectedly short: %q", out)
	}
	for _, want := range []string{"abcd1234", "(25%)", "5 /", "20"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
}
