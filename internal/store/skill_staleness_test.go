package store

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
)

// seedSkillLoadAt ingests a Skill tool_use envelope at the given
// timestamp. The skill_load extractor fires automatically inside
// IngestEnvelope.
func seedSkillLoadAt(t *testing.T, s *Store, sessionKey, skill string, ts time.Time) {
	t.Helper()
	env := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sessionKey,
		Kind:            "tool_use",
		Role:            "assistant",
		TsSource:        ts,
		Tool:            &events.Tool{Name: "Skill"},
		Payload:         map[string]any{"tool_input": map[string]any{"skill": skill}},
		Redaction:       &events.Redaction{Applied: true},
	}
	withTx(t, s, func(tx *sql.Tx) {
		if _, _, err := IngestEnvelope(t.Context(), tx, env, []byte(`{"v":1}`), env.TsSource.UnixMilli()); err != nil {
			t.Fatalf("seed skill load: %v", err)
		}
	})
}

// seedToolFailureAt ingests a tool_failure event in the same
// session at the given timestamp — the failure signal staleness
// detection joins against.
func seedToolFailureAt(t *testing.T, s *Store, sessionKey string, ts time.Time) {
	t.Helper()
	env := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sessionKey,
		Kind:            "tool_failure",
		Role:            "tool",
		TsSource:        ts,
		Tool:            &events.Tool{Name: "Bash"},
		ContentText:     "Exit code 1",
		Payload:         map[string]any{"error": "boom"},
		Redaction:       &events.Redaction{Applied: true},
	}
	withTx(t, s, func(tx *sql.Tx) {
		if _, _, err := IngestEnvelope(t.Context(), tx, env, []byte(`{"v":1}`), env.TsSource.UnixMilli()); err != nil {
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
	if len(rows[0].Examples) != 1 || rows[0].Examples[0] != events.DeriveSessionID("claude-code", "sess-stale-1") {
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

// TestWilsonLowerBound_OutOfRange pins the defensive clamp: if a
// caller passes successes < 0 or successes > total the function
// returns 0 instead of NaN. NaN-poisoning is the worst failure mode
// because the `if lb < 0` guard inside the function does NOT catch
// it (NaN comparisons are always false), so the bad value would
// silently propagate into ranking and sort keys.
func TestWilsonLowerBound_OutOfRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		successes, total int
	}{
		{"successes>total", 2, 1},
		{"successes>total big gap", 1000, 1},
		{"negative successes", -1, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wilsonLowerBound(tc.successes, tc.total)
			if got != 0 {
				t.Errorf("wilsonLowerBound(%d,%d) = %v, want 0", tc.successes, tc.total, got)
			}
		})
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

// TestAutoRefineScore covers the safe properties of the AutoRefine
// (Qiu et al., 2026 — arXiv:2601.22758) compound ranking under
// aichronicles' usage. The paper's metadata tracks (r retrieved,
// u utilised, s successful) as three independent counters; we
// derive u_aichronicles = retrieved − failed (utilised-and-
// successful collapsed), so the formula's regime is constrained.
// The properties below are the ones that still hold:
//
//  1. Zero retrieved → 0 (no data).
//  2. All-failures → 0 (effectiveness = 0 forces the product to 0).
//  3. Higher success count at the SAME retrieval count → higher
//     score (the log(1+u) factor monotone in u, the second factor
//     bounded above 1).
//  4. The score is finite and non-negative for any sensible input.
func TestAutoRefineScore(t *testing.T) {
	t.Parallel()

	// Property 1: zero retrieved → 0.
	if got := autoRefineScore(0, 0); got != 0 {
		t.Errorf("zero-data: got %f, want 0", got)
	}

	// Property 2: all-failures → 0.
	if got := autoRefineScore(10, 10); got != 0 {
		t.Errorf("all-failures: got %f, want 0", got)
	}

	// Property 3: at the same retrieval count, fewer failures
	// (more successes) scores higher.
	worse := autoRefineScore(10, 8)  // 2 successes
	better := autoRefineScore(10, 0) // 10 successes
	if !(better > worse) {
		t.Errorf("expected better(%f) > worse(%f) at same retrieval count", better, worse)
	}

	// Property 4: non-negativity / finiteness.
	for _, tc := range []struct{ r, fails int }{
		{1, 0}, {1, 1}, {2, 1}, {100, 1}, {100, 50}, {1000, 0},
	} {
		got := autoRefineScore(tc.r, tc.fails)
		if got < 0 {
			t.Errorf("negative score for r=%d fails=%d: %f", tc.r, tc.fails, got)
		}
		if got != got { // NaN test
			t.Errorf("NaN score for r=%d fails=%d", tc.r, tc.fails)
		}
	}
}

// TestAutoRefineScore_PopulatedOnLoad asserts the score is
// computed and populated when LoadSkillStaleness returns a row,
// alongside Wilson — both signals available for downstream
// retire decisions.
func TestAutoRefineScore_PopulatedOnLoad(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	t0 := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)

	// Seed: 3 loads, 1 followed by failure → stale rate 1/3.
	seedSkillLoadAt(t, s, "sess-arf-1", "build-test", t0)
	seedToolFailureAt(t, s, "sess-arf-1", t0.Add(2*time.Minute))
	seedSkillLoadAt(t, s, "sess-arf-2", "build-test", t0.Add(time.Hour))
	seedSkillLoadAt(t, s, "sess-arf-3", "build-test", t0.Add(2*time.Hour))

	since := t0.Add(-24 * time.Hour).UnixMilli()
	rows, err := LoadSkillStaleness(t.Context(), s.DB(), since, 0, SkillStalenessLimits{})
	if err != nil {
		t.Fatalf("LoadSkillStaleness: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d want 1", len(rows))
	}
	r := rows[0]
	if r.AutoRefineScore <= 0 {
		t.Errorf("AutoRefineScore should be positive for a 2/3-success skill, got %f", r.AutoRefineScore)
	}
	// Sanity: matches the pure-helper computation.
	if want := autoRefineScore(r.TotalLoads, r.StaleLoads); want != r.AutoRefineScore {
		t.Errorf("score drift: row=%f helper=%f", r.AutoRefineScore, want)
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
