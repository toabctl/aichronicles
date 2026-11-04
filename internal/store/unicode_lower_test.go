package store

import (
	"context"
	"database/sql/driver"
	"testing"
)

// TestFindEpisodes_QueryContainsFoldsNonASCII is the regression gate
// for the ASCII-only lower(): Go lowercased the needle fully while
// SQLite lowercased the column only in ASCII, so a non-English intent
// was unreachable even when the case matched exactly.
func TestFindEpisodes_QueryContainsFoldsNonASCII(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	seedEpisodeRow(t, s, "00000000-0000-0000-0000-00000000eee1", 1, 1, 2, "/repo/x", "Überarbeite die Doku")
	seedEpisodeRow(t, s, "00000000-0000-0000-0000-00000000eee2", 1, 3, 4, "/repo/x", "ΣΦΑΛΜΑ στο build")

	for q, want := range map[string]int{
		"Über": 1, "über": 1, "ÜBERARBEITE": 1, "arbeite": 1,
		"σφαλμα": 1, "ΣΦΑΛΜΑ": 1, "missing": 0,
	} {
		hits, err := FindEpisodes(ctx, s.DB(), FindEpisodesOpts{QueryContains: q})
		if err != nil {
			t.Fatalf("q=%q: %v", q, err)
		}
		if len(hits) != want {
			t.Errorf("q=%q: got %d hits, want %d", q, len(hits), want)
		}
	}
}

func TestFactSubjectsLike_FoldsNonASCII(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkFactsRow(t, s, 1_700_000_000_000)
	if _, err := SaveSemanticFact(ctx, s.DB(), SemanticFact{
		SourceLLMOutputID: loID, Subject: "/work/Überblick", Predicate: "primary_language",
		Object: "Go", Confidence: 1.0, AssertedAtMs: 1_700_000_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"überblick", "ÜBERBLICK", "Überblick"} {
		got, err := FactSubjectsLike(ctx, s.DB(), needle, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("needle %q: got %v, want the one subject", needle, got)
		}
	}
}

func TestUnicodeLower_TypeSemantics(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   driver.Value
		want driver.Value
	}{
		{nil, nil},
		{"ÄÖÜ Straße", "äöü straße"},
		{[]byte("ÉCOLE"), "école"},
		{int64(7), int64(7)},
		{3.5, 3.5},
	}
	for _, tc := range cases {
		got, err := unicodeLower(nil, []driver.Value{tc.in})
		if err != nil {
			t.Fatalf("%v: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("unicodeLower(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
