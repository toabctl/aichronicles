package store

import (
	"strings"
	"testing"
)

func TestLoadSessionsForCompletion_EmptyPrefixListsAll(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	// First prompts must be ≥30 runes to count as substantive — see
	// the substantiveFirstPromptMinRunes contract in events.go.
	// Shorter prompts get masked as "(no summary)" because the
	// completion description should not surface filler like "yes" /
	// "/loop" as a session's identity.
	a := ingestText(t, s, "sess-alpha", "alpha thread first prompt about onboarding")
	b := ingestText(t, s, "sess-beta", "beta thread first prompt about deployments")

	rows, err := LoadSessionsForCompletion(t.Context(), s.DB(), "", 10)
	if err != nil {
		t.Fatalf("LoadSessionsForCompletion: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: got %d, want 2", len(rows))
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.ID] = r.Description
	}
	if !strings.Contains(got[a], "alpha thread first prompt") {
		t.Errorf("alpha description missing prompt: %q", got[a])
	}
	if !strings.Contains(got[b], "/work/sess-beta") {
		t.Errorf("beta description missing cwd: %q", got[b])
	}
}

func TestLoadSessionsForCompletion_PrefixFilters(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	a := ingestText(t, s, "sess-alpha", "alpha")
	_ = ingestText(t, s, "sess-beta", "beta")

	rows, err := LoadSessionsForCompletion(t.Context(), s.DB(), a[:8], 10)
	if err != nil {
		t.Fatalf("LoadSessionsForCompletion: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != a {
		t.Errorf("prefix should match exactly one row; got %+v", rows)
	}
}

func TestLoadSessionsForCompletion_RespectsLimit(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	for i := 0; i < 15; i++ {
		ingestText(t, s, string(rune('a'+i))+"-bulk", "content")
	}
	rows, err := LoadSessionsForCompletion(t.Context(), s.DB(), "", 5)
	if err != nil {
		t.Fatalf("LoadSessionsForCompletion: %v", err)
	}
	if len(rows) != 5 {
		t.Errorf("limit: got %d rows, want 5", len(rows))
	}
}

func TestLoadSessionsForCompletion_DefaultLimit(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ingestText(t, s, "sess-only", "x")
	rows, err := LoadSessionsForCompletion(t.Context(), s.DB(), "", 0)
	if err != nil {
		t.Fatalf("LoadSessionsForCompletion: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("zero limit should default; got %d rows", len(rows))
	}
}

func TestLoadSessionsForCompletion_NonHexInputReturnsEmpty(t *testing.T) {
	t.Parallel()
	s := openTemp(t)
	ingestText(t, s, "sess-real", "x")
	// A user mid-typing something that obviously isn't a UUID
	// (e.g. "%' OR 1=1 --") must not error out the completion
	// flow nor reach the SQL layer.
	rows, err := LoadSessionsForCompletion(t.Context(), s.DB(), "%not-a-uuid", 10)
	if err != nil {
		t.Errorf("expected silent empty result, got err: %v", err)
	}
	if rows != nil {
		t.Errorf("expected nil rows for non-hex prefix; got %+v", rows)
	}
}

func TestFormatCompletionDescription_FlattensWhitespace(t *testing.T) {
	t.Parallel()
	// Long enough to count as substantive (≥30 runes) so it falls
	// through to the prompt-as-preview branch and we can assert on
	// whitespace flattening.
	prompt := "first\nline\tlast — implement the OAuth refresh-token rotation"
	got := formatCompletionDescription("/x", prompt, "")
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("whitespace not flattened: %q", got)
	}
}

func TestFormatCompletionDescription_TruncatesLongPreview(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 200)
	got := formatCompletionDescription("/x", long, "")
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis on truncation, got %q", got)
	}
}

func TestFormatCompletionDescription_PrefersSummaryTopic(t *testing.T) {
	t.Parallel()
	body := `{"topic":"Refactoring the ingest pipeline","what_was_done":["..."]}`
	got := formatCompletionDescription("/x", "go ahead", body)
	if !strings.Contains(got, "Refactoring the ingest pipeline") {
		t.Errorf("summary topic should win over short first_prompt: %q", got)
	}
	if strings.Contains(got, "go ahead") {
		t.Errorf("filler first_prompt leaked: %q", got)
	}
}

func TestFormatCompletionDescription_FallsBackToPlaceholderWhenNeitherSubstantive(t *testing.T) {
	t.Parallel()
	got := formatCompletionDescription("/x", "go ahead", "")
	if !strings.Contains(got, "(no summary)") {
		t.Errorf("expected placeholder, got %q", got)
	}
}

func TestFormatCompletionDescription_RejectsBareSlashCommands(t *testing.T) {
	t.Parallel()
	got := formatCompletionDescription("/x", "/loop", "")
	if strings.Contains(got, "/loop") {
		t.Errorf("bare slash command should be rejected as filler: %q", got)
	}
}
