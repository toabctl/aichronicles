package store

import (
	"database/sql"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
)

// seedSighting ingests one event in sessionKey at ts (with cwd, or no
// cwd when empty) and attaches one extraction (kind, value) to it.
// The extraction is inserted directly so the test exercises the read
// path, not the ingest extractors. Returns the canonical session id.
func seedSighting(t *testing.T, s *Store, sessionKey string, ts time.Time, cwd, kind, value string) string {
	t.Helper()
	env := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sessionKey,
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        ts,
		Cwd:             cwd,
		Payload:         map[string]any{},
		Redaction:       &events.Redaction{Applied: true},
	}
	sessionID := events.DeriveSessionID(env.SourceAgent, env.SourceSessionID)
	withTx(t, s, func(tx *sql.Tx) {
		if _, _, err := IngestEnvelope(t.Context(), tx, env, []byte(`{"v":1}`), ts.UnixMilli()); err != nil {
			t.Fatalf("seed ingest: %v", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO extractions(event_id, session_id, kind, value) VALUES (?, ?, ?, ?)`,
			env.EventID, sessionID, kind, value,
		); err != nil {
			t.Fatalf("seed extraction: %v", err)
		}
	})
	return sessionID
}

// sightingKey flattens a result row for comparison; cwd "-" means nil.
func sightingKey(x ExtractionSighting) string {
	cwd := "-"
	if x.Cwd != nil {
		cwd = *x.Cwd
	}
	return fmt.Sprintf("%s|%s|%s|%d|%s", x.SessionID, x.Kind, x.Value, x.TsSourceMs, cwd)
}

func sightingKeys(xs []ExtractionSighting) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = sightingKey(x)
	}
	return out
}

func TestFindExtractions_EmptyKindIsError(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	if _, err := FindExtractions(t.Context(), s.DB(), FindExtractionsOpts{Value: "x"}); err == nil {
		t.Fatal("expected error for empty kind")
	}
}

func TestFindExtractions_NoMatchReturnsEmpty(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	seedSighting(t, s, "a", time.UnixMilli(1000), "/w", "pr_created", "https://g/o/r/pull/1")

	for _, opts := range []FindExtractionsOpts{
		{Kind: "pr_created", Value: "https://g/o/r/pull/2"},
		{Kind: "no_such_kind"},
	} {
		got, err := FindExtractions(t.Context(), s.DB(), opts)
		if err != nil {
			t.Fatalf("%+v: %v", opts, err)
		}
		if len(got) != 0 {
			t.Errorf("%+v: expected no rows, got %v", opts, sightingKeys(got))
		}
	}
}

// TestFindExtractions_ValueMatchesExactly pins byte-for-byte matching:
// a URL that is a prefix of, extends, or differs in case from the
// requested value must not match.
func TestFindExtractions_ValueMatchesExactly(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	const want = "https://github.com/o/r/pull/1"
	sa := seedSighting(t, s, "a", time.UnixMilli(1000), "/w", "pr_created", want)
	seedSighting(t, s, "b", time.UnixMilli(2000), "/w", "pr_created", "https://github.com/o/r/pull/10")
	seedSighting(t, s, "c", time.UnixMilli(3000), "/w", "pr_created", want+"/files")
	seedSighting(t, s, "d", time.UnixMilli(4000), "/w", "pr_created", "https://github.com/O/R/pull/1")
	seedSighting(t, s, "e", time.UnixMilli(5000), "/w", "pr_created", "https://github.com/o/r/pull/")

	got, err := FindExtractions(t.Context(), s.DB(), FindExtractionsOpts{Kind: "pr_created", Value: want})
	if err != nil {
		t.Fatalf("FindExtractions: %v", err)
	}
	wantKeys := []string{sa + "|pr_created|" + want + "|1000|/w"}
	if gotKeys := sightingKeys(got); !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Errorf("got %v, want %v", gotKeys, wantKeys)
	}
}

// TestFindExtractions_KindIsolation confirms the same value under a
// different kind (a PR merely mentioned) is not reported as the kind
// asked for (the PR created).
func TestFindExtractions_KindIsolation(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	const pr = "https://github.com/o/r/pull/7"
	creator := seedSighting(t, s, "creator", time.UnixMilli(1000), "/w/a", "pr_created", pr)
	seedSighting(t, s, "mentioner", time.UnixMilli(2000), "/w/b", "url", pr)

	got, err := FindExtractions(t.Context(), s.DB(), FindExtractionsOpts{Kind: "pr_created", Value: pr})
	if err != nil {
		t.Fatalf("FindExtractions: %v", err)
	}
	wantKeys := []string{creator + "|pr_created|" + pr + "|1000|/w/a"}
	if gotKeys := sightingKeys(got); !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Errorf("got %v, want %v", gotKeys, wantKeys)
	}
}

// TestFindExtractions_CollapsesRepeatsToEarliestSighting covers a
// session that saw the value several times. The later sighting is
// inserted first, so the earliest row is picked by timestamp, not by
// insertion order — and its cwd comes along with it.
func TestFindExtractions_CollapsesRepeatsToEarliestSighting(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	const pr = "https://github.com/o/r/pull/7"
	sid := seedSighting(t, s, "a", time.UnixMilli(5000), "/later", "pr_created", pr)
	seedSighting(t, s, "a", time.UnixMilli(1000), "/earliest", "pr_created", pr)
	seedSighting(t, s, "a", time.UnixMilli(3000), "/middle", "pr_created", pr)

	got, err := FindExtractions(t.Context(), s.DB(), FindExtractionsOpts{Kind: "pr_created", Value: pr})
	if err != nil {
		t.Fatalf("FindExtractions: %v", err)
	}
	wantKeys := []string{sid + "|pr_created|" + pr + "|1000|/earliest"}
	if gotKeys := sightingKeys(got); !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Errorf("got %v, want %v", gotKeys, wantKeys)
	}
}

// TestFindExtractions_ListsKindNewestFirst covers the value-less
// listing: one row per (session, value), newest first sighting first,
// with ties on the timestamp broken by session id then value.
func TestFindExtractions_ListsKindNewestFirst(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	s1 := seedSighting(t, s, "one", time.UnixMilli(1000), "/w1", "pr_created", "https://g/o/r/pull/1")
	s2 := seedSighting(t, s, "two", time.UnixMilli(3000), "/w2", "pr_created", "https://g/o/r/pull/2")
	// Same session, second PR: its own row.
	seedSighting(t, s, "two", time.UnixMilli(2000), "/w2", "pr_created", "https://g/o/r/pull/3")
	// Timestamp tie with s1's row.
	s3 := seedSighting(t, s, "three", time.UnixMilli(1000), "/w3", "pr_created", "https://g/o/r/pull/4")
	// Other kind: excluded.
	seedSighting(t, s, "four", time.UnixMilli(9000), "/w4", "url", "https://g/o/r/pull/5")

	got, err := FindExtractions(t.Context(), s.DB(), FindExtractionsOpts{Kind: "pr_created"})
	if err != nil {
		t.Fatalf("FindExtractions: %v", err)
	}
	tie := []string{
		s1 + "|pr_created|https://g/o/r/pull/1|1000|/w1",
		s3 + "|pr_created|https://g/o/r/pull/4|1000|/w3",
	}
	if s3 < s1 {
		tie[0], tie[1] = tie[1], tie[0]
	}
	wantKeys := append([]string{
		s2 + "|pr_created|https://g/o/r/pull/2|3000|/w2",
		s2 + "|pr_created|https://g/o/r/pull/3|2000|/w2",
	}, tie...)
	if gotKeys := sightingKeys(got); !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Errorf("got %v\nwant %v", gotKeys, wantKeys)
	}
}

// TestFindExtractions_SinceMs covers the window: sightings before
// since_ms are ignored (so a session seen only before drops out and
// one seen on both sides reports its first in-window sighting), and
// the bound is inclusive.
func TestFindExtractions_SinceMs(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	const pr = "https://github.com/o/r/pull/7"
	seedSighting(t, s, "before-only", time.UnixMilli(1000), "/b", "pr_created", pr)
	straddle := seedSighting(t, s, "straddle", time.UnixMilli(1500), "/s-before", "pr_created", pr)
	seedSighting(t, s, "straddle", time.UnixMilli(2500), "/s-after", "pr_created", pr)
	boundary := seedSighting(t, s, "boundary", time.UnixMilli(2000), "/edge", "pr_created", pr)

	got, err := FindExtractions(t.Context(), s.DB(), FindExtractionsOpts{Kind: "pr_created", Value: pr, SinceMs: 2000})
	if err != nil {
		t.Fatalf("FindExtractions: %v", err)
	}
	wantKeys := []string{
		straddle + "|pr_created|" + pr + "|2500|/s-after",
		boundary + "|pr_created|" + pr + "|2000|/edge",
	}
	if gotKeys := sightingKeys(got); !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Errorf("got %v\nwant %v", gotKeys, wantKeys)
	}
}

func TestFindExtractions_NilCwd(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	seedSighting(t, s, "a", time.UnixMilli(1000), "", "pr_created", "https://g/o/r/pull/1")

	got, err := FindExtractions(t.Context(), s.DB(), FindExtractionsOpts{Kind: "pr_created"})
	if err != nil {
		t.Fatalf("FindExtractions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Cwd != nil {
		t.Errorf("cwd: got %q, want nil", *got[0].Cwd)
	}
}

// TestFindExtractions_Paginates walks the full listing in pages of two
// and checks the pages are disjoint, ordered, and add up to the whole.
func TestFindExtractions_Paginates(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	for i := range 5 {
		seedSighting(t, s, fmt.Sprintf("s%d", i), time.UnixMilli(int64(1000*(i+1))), "/w",
			"pr_created", fmt.Sprintf("https://g/o/r/pull/%d", i))
	}
	all, err := FindExtractions(t.Context(), s.DB(), FindExtractionsOpts{Kind: "pr_created"})
	if err != nil {
		t.Fatalf("full listing: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("full listing: got %d rows, want 5", len(all))
	}

	var paged []ExtractionSighting
	for offset := 0; ; offset += 2 {
		page, err := FindExtractions(t.Context(), s.DB(), FindExtractionsOpts{Kind: "pr_created", Limit: 2, Offset: offset})
		if err != nil {
			t.Fatalf("page at %d: %v", offset, err)
		}
		if len(page) > 2 {
			t.Fatalf("page at %d: %d rows over the limit", offset, len(page))
		}
		paged = append(paged, page...)
		if len(page) < 2 {
			break
		}
	}
	if !reflect.DeepEqual(sightingKeys(paged), sightingKeys(all)) {
		t.Errorf("pages %v\n!= full listing %v", sightingKeys(paged), sightingKeys(all))
	}
}

func TestFindExtractions_NonPositiveLimitUsesDefault(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	for i := range DefaultFindExtractionsLimit + 1 {
		seedSighting(t, s, fmt.Sprintf("s%d", i), time.UnixMilli(int64(1000+i)), "/w",
			"pr_created", fmt.Sprintf("https://g/o/r/pull/%d", i))
	}
	for _, limit := range []int{0, -1} {
		got, err := FindExtractions(t.Context(), s.DB(), FindExtractionsOpts{Kind: "pr_created", Limit: limit})
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if len(got) != DefaultFindExtractionsLimit {
			t.Errorf("limit %d: got %d rows, want %d", limit, len(got), DefaultFindExtractionsLimit)
		}
	}
}
