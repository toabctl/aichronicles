package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/redact"
)

// TestStoreWritePaths_ScrubModelAuthoredText covers the store's
// outbound-redaction invariant across every write path that accepts
// LLM-authored free text.
//
// Only llm_outputs.body enforced this. The four siblings took a
// model's evidence quote, episode intent, link rationale or skill
// trigger straight to SQLite — and CLAUDE.md §7 explicitly tells the
// model to quote real observed values, so transcribing a token it saw
// is the documented behaviour, not a rare accident.
//
// Each case writes through the real API with a secret embedded, then
// asserts the persisted column holds the marker instead. Table-driven
// so a new write path is one entry, not a new test.
func TestStoreWritePaths_ScrubModelAuthoredText(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("a", 36)

	cases := []struct {
		name string
		// write persists a row containing secret and returns the SQL
		// that reads the stored column back.
		write func(t *testing.T, s *Store, sessionID string) (query string, args []any)
		// Angle brackets omitted: the JSON-encoded columns escape them.
		wantMarker string
	}{
		{
			name: "semantic_facts evidence_quote and object",
			write: func(t *testing.T, s *Store, sessionID string) (string, []any) {
				t.Helper()
				quote := "ran with token " + secret
				outputID := insertLLMOutputForTest(t, s, sessionID)
				if _, err := SaveSemanticFact(context.Background(), s.DB(), SemanticFact{
					SourceLLMOutputID: outputID,
					Subject:           "/tmp/proj",
					Predicate:         "deploys_via",
					Object:            "deploy.sh --token " + secret,
					Confidence:        0.9,
					EvidenceSessionID: &sessionID,
					EvidenceQuote:     &quote,
					AssertedAtMs:      time.Now().UnixMilli(),
				}); err != nil {
					t.Fatalf("SaveSemanticFact: %v", err)
				}
				return `SELECT object || ' ' || COALESCE(evidence_quote,'') FROM semantic_facts`, nil
			},
			wantMarker: "redacted:github_pat_classic",
		},
		{
			name: "episodes intent_summary",
			write: func(t *testing.T, s *Store, sessionID string) (string, []any) {
				t.Helper()
				if _, err := SaveEpisodes(context.Background(), s.DB(), sessionID, []events.Episode{{
					Ordinal:       1,
					StartedAtMs:   1,
					EndedAtMs:     2,
					IntentSummary: "user pasted " + secret,
					EventCount:    1,
					FirstEventID:  firstEventIDForTest(t, s, sessionID),
				}}); err != nil {
					t.Fatalf("SaveEpisodes: %v", err)
				}
				return `SELECT intent_summary FROM episodes`, nil
			},
			wantMarker: "redacted:github_pat_classic",
		},
		{
			name: "session_links rationale",
			write: func(t *testing.T, s *Store, sessionID string) (string, []any) {
				t.Helper()
				other := ingestSecondSessionForTest(t, s)
				if err := SaveSessionLinks(context.Background(), s.DB(), sessionID, []SessionLink{{
					ToSessionID: other,
					Kind:        "builds_on",
					Rationale:   "both used " + secret,
				}}); err != nil {
					t.Fatalf("SaveSessionLinks: %v", err)
				}
				return `SELECT rationale FROM session_links`, nil
			},
			wantMarker: "redacted:github_pat_classic",
		},
		{
			name: "skill_candidates triggers tags examples",
			write: func(t *testing.T, s *Store, sessionID string) (string, []any) {
				t.Helper()
				id := insertLLMOutputForTest(t, s, sessionID)
				if err := RecordSkillCandidateWithMetadata(context.Background(), s.DB(),
					id, "deploy-skill", time.Now().UnixMilli(),
					SkillCandidateMetadata{
						Triggers: []string{"when deploying with " + secret},
						Tags:     []string{"tok-" + secret},
						Examples: []SkillExample{{
							Input:  "run deploy " + secret,
							Output: "ok " + secret,
						}},
					}); err != nil {
					t.Fatalf("RecordSkillCandidateWithMeta: %v", err)
				}
				return `SELECT COALESCE(triggers,'') || COALESCE(tags,'') || COALESCE(examples,'')
				          FROM skill_candidates`, nil
			},
			wantMarker: "redacted:github_pat_classic",
		},
		{
			name: "llm_outputs body stays covered",
			write: func(t *testing.T, s *Store, sessionID string) (string, []any) {
				t.Helper()
				insertLLMOutputForTest(t, s, sessionID)
				return `SELECT body FROM llm_outputs`, nil
			},
			wantMarker: "redacted:github_pat_classic",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := openTestStore(t)
			sessionID := ingestForScrub(t, s, "no-secret-here", nil)

			query, args := tc.write(t, s, sessionID)

			var got string
			if err := s.DB().QueryRow(query, args...).Scan(&got); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if strings.Contains(got, secret) {
				t.Errorf("stored column holds the raw secret: %q", got)
			}
			if !strings.Contains(got, tc.wantMarker) {
				t.Errorf("expected %s in stored column, got %q", tc.wantMarker, got)
			}
		})
	}
}

// insertLLMOutputForTest writes one summary row carrying a secret and
// returns its id. Shared by the skill-candidate case (which needs a
// parent row for the FK) and the llm_outputs case.
func insertLLMOutputForTest(t *testing.T, s *Store, sessionID string) int64 {
	t.Helper()
	secret := "ghp_" + strings.Repeat("a", 36)
	var id int64
	err := WithTx(context.Background(), s.DB(), func(tx *sql.Tx) error {
		var innerErr error
		id, _, innerErr = SaveLLMOutput(context.Background(), tx, &LLMOutput{
			SessionID:   &sessionID,
			Kind:        LLMKindSummary,
			Model:       "test-model",
			PromptHash:  "hash-" + sessionID,
			Body:        `{"topic":"used ` + secret + `"}`,
			CreatedAtMs: time.Now().UnixMilli(),
		})
		return innerErr
	})
	if err != nil {
		t.Fatalf("SaveLLMOutput: %v", err)
	}
	return id
}

// firstEventIDForTest returns an event id belonging to sessionID, for
// FK-bearing fixtures such as episodes.first_event_id.
func firstEventIDForTest(t *testing.T, s *Store, sessionID string) string {
	t.Helper()
	var id string
	if err := s.DB().QueryRow(
		`SELECT event_id FROM events WHERE session_id = ? ORDER BY rowid LIMIT 1`,
		sessionID).Scan(&id); err != nil {
		t.Fatalf("look up event id: %v", err)
	}
	return id
}

// ingestSecondSessionForTest creates a distinct session so link
// fixtures can point somewhere other than themselves.
func ingestSecondSessionForTest(t *testing.T, s *Store) string {
	t.Helper()
	env := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-link-target",
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC),
		ContentText:     "unrelated session",
		Transport:       "hook",
		Redaction:       &events.Redaction{Applied: true},
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := IngestEnvelopeWithExtractions(
		context.Background(), tx, env, raw, time.Now().UnixMilli(), nil); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ingest: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return events.DeriveSessionID(env.SourceAgent, env.SourceSessionID)
}

// TestScrubStoredHelpers pins the helper semantics the write paths
// depend on, especially the nil-vs-empty distinction: collapsing them
// would change what NULL means for callers that branch on it.
func TestScrubStoredHelpers(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("a", 36)

	t.Run("scrubStored replaces and passes through", func(t *testing.T) {
		t.Parallel()
		if got := scrubStored("tok " + secret); strings.Contains(got, secret) {
			t.Errorf("secret survived: %q", got)
		}
		if got := scrubStored("nothing to see"); got != "nothing to see" {
			t.Errorf("clean input mutated: %q", got)
		}
		if got := scrubStored(""); got != "" {
			t.Errorf("empty input mutated: %q", got)
		}
	})

	t.Run("scrubStoredPtr keeps nil distinct from empty", func(t *testing.T) {
		t.Parallel()
		if got := scrubStoredPtr(nil); got != nil {
			t.Errorf("nil must stay nil, got %v", got)
		}
		empty := ""
		if got := scrubStoredPtr(&empty); got == nil || *got != "" {
			t.Errorf("empty must stay a non-nil empty string, got %v", got)
		}
		dirty := "tok " + secret
		got := scrubStoredPtr(&dirty)
		if got == nil || strings.Contains(*got, secret) {
			t.Errorf("secret survived: %v", got)
		}
		if dirty != "tok "+secret {
			t.Errorf("caller's string was mutated: %q", dirty)
		}
	})

	t.Run("scrubStoredList does not mutate input", func(t *testing.T) {
		t.Parallel()
		in := []string{"tok " + secret, "clean"}
		out := scrubStoredList(in)
		if in[0] != "tok "+secret {
			t.Errorf("input slice mutated: %q", in[0])
		}
		if strings.Contains(out[0], secret) {
			t.Errorf("secret survived: %q", out[0])
		}
		if out[1] != "clean" {
			t.Errorf("clean element changed: %q", out[1])
		}
	})

	t.Run("scrubStoredList and Examples handle empty", func(t *testing.T) {
		t.Parallel()
		if got := scrubStoredList(nil); got != nil {
			t.Errorf("nil slice should stay nil, got %v", got)
		}
		if got := scrubStoredExamples(nil); got != nil {
			t.Errorf("nil examples should stay nil, got %v", got)
		}
	})

	t.Run("quote-bearing secret is caught before encoding", func(t *testing.T) {
		t.Parallel()
		// The reason list elements are scrubbed rather than the
		// encoded JSON: an encoder would escape the quote and the
		// detector would miss the escaped form.
		raw := `say "` + secret + `" out loud`
		if got := scrubStored(raw); strings.Contains(got, secret) {
			t.Errorf("secret survived alongside quotes: %q", got)
		}
		if len(redact.Default().Scan(raw)) == 0 {
			t.Fatal("fixture no longer matches a detector")
		}
	})
}
