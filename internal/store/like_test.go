package store

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
)

func TestLikePatterns_EscapeMetacharacters(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, contains, prefix string }{
		{"plain", `%plain%`, `plain%`},
		{"my_file.go", `%my\_file.go%`, `my\_file.go%`},
		{"100%", `%100\%%`, `100\%%`},
		{`C:\dir`, `%C:\\dir%`, `C:\\dir%`},
		{"", `%%`, `%`},
	}
	for _, tc := range cases {
		if got := likeContains(tc.in); got != tc.contains {
			t.Errorf("likeContains(%q) = %q, want %q", tc.in, got, tc.contains)
		}
		if got := likePrefix(tc.in); got != tc.prefix {
			t.Errorf("likePrefix(%q) = %q, want %q", tc.in, got, tc.prefix)
		}
	}
}

// TestFactSubjectsLike_WildcardsMatchLiterally is the regression gate
// for unescaped LIKE patterns: `_` matched any single character, so a
// needle naming one path also returned its look-alikes.
func TestFactSubjectsLike_WildcardsMatchLiterally(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ctx := context.Background()
	loID := mkFactsRow(t, s, 1_700_000_000_000)
	for _, sub := range []string{"/work/my_proj", "/work/myXproj", "/work/100%done", "/work/100Xdone"} {
		if _, err := SaveSemanticFact(ctx, s.DB(), SemanticFact{
			SourceLLMOutputID: loID, Subject: sub, Predicate: "primary_language",
			Object: "Go", Confidence: 1.0, AssertedAtMs: 1_700_000_000_000,
		}); err != nil {
			t.Fatalf("save %s: %v", sub, err)
		}
	}
	for needle, want := range map[string][]string{
		"my_proj": {"/work/my_proj"},
		"100%":    {"/work/100%done"},
	} {
		got, err := FactSubjectsLike(ctx, s.DB(), needle, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("needle %q: got %v, want %v", needle, got, want)
		}
	}
}

// TestLoadSessionsForListFaceted_ProjectAndFileFacetsAreLiteral pins
// the two session-list LIKE facets: project is a literal path prefix
// and file_path_substring a literal substring.
func TestLoadSessionsForListFaceted_ProjectAndFileFacetsAreLiteral(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	idUnderscore := ingestAt(t, s, "src-a", "/work/my_proj/sub", "internal/my_file.go")
	ingestAt(t, s, "src-b", "/work/myXproj/sub", "internal/myXfile.go")

	for name, f := range map[string]SessionListFacets{
		"project":   {Project: "/work/my_proj"},
		"file path": {FilePathSubstring: "my_file"},
	} {
		rows, err := LoadSessionsForListFaceted(ctx, s.DB(), f, 0, 50, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ID != idUnderscore {
			ids := make([]string, 0, len(rows))
			for _, r := range rows {
				ids = append(ids, r.ID)
			}
			t.Errorf("%s facet: got %v, want only %s", name, ids, idUnderscore)
		}
	}
}

// ingestAt ingests one Read tool_use in cwd touching filePath (which
// IngestEnvelope's extractors turn into a file_path extraction) and
// returns the derived session id.
func ingestAt(t *testing.T, s *Store, sourceSession, cwd, filePath string) string {
	t.Helper()
	env := &events.Envelope{
		V: 1, EventID: uuid.Must(uuid.NewV7()).String(),
		SourceAgent: "claude-code", SourceSessionID: sourceSession,
		Kind: "tool_use", Role: "tool", TsSource: time.Now().UTC(), Cwd: cwd,
		Tool:      &events.Tool{Name: "Read"},
		Payload:   map[string]any{"tool_input": map[string]any{"file_path": filePath}},
		Transport: "hook", Redaction: &events.Redaction{Applied: true},
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := IngestEnvelope(t.Context(), tx, env, raw, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return events.DeriveSessionID("claude-code", sourceSession)
}
