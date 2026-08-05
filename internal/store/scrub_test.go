package store

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/redact"
)

// ingestForScrub writes one event plus its extractions through the
// real ingest path, so the triggers that maintain events_fts,
// extractions_fts and the materialised sessions columns all fire
// exactly as they do in production. Returns the derived session id.
//
// The envelope is stored deliberately UNSCRUBBED: these tests model
// the situation Scrub exists for — rows that landed before a detector
// existed, or via the payload-shape bypass fixed in internal/events.
func ingestForScrub(t *testing.T, s *Store, secret string, extra []events.Extraction) string {
	t.Helper()
	env := &events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-scrub",
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC),
		Cwd:             "/tmp/proj-" + secret,
		ContentText:     "please deploy using token " + secret,
		Payload:         map[string]any{"prompt": "please deploy using token " + secret},
		Transport:       "hook",
		Redaction:       &events.Redaction{Applied: true},
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := IngestEnvelopeWithExtractions(
		context.Background(), tx, env, raw, time.Now().UnixMilli(), extra,
	); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ingest: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return events.DeriveSessionID(env.SourceAgent, env.SourceSessionID)
}

// scrubTestSecret is a github_pat_classic-shaped value: a real
// detector shape so redact.Default() finds it without the test
// depending on a bespoke scanner.
func scrubTestSecret() string { return "ghp_" + strings.Repeat("a", 36) }

// TestScrub_LeavesNoSecretAnywhere is the whole point of the command.
// Scrub previously rewrote only raw_envelopes.envelope_json and
// events.content_text, so a secret survived in events.cwd,
// extractions.value, extractions.extra_json and — because they are
// maintained by AFTER INSERT triggers with no UPDATE counterpart —
// the materialised sessions columns. The command reported success
// while `aichronicles search <secret>` still returned the row via
// extractions_fts.
//
// Rather than asserting column by column, this sweeps every TEXT
// column in the database. A new secret-bearing column added later
// fails this test until Scrub learns about it, which is the property
// worth having.
func TestScrub_LeavesNoSecretAnywhere(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	secret := scrubTestSecret()

	ingestForScrub(t, s, secret, []events.Extraction{
		{
			Kind:  "shell_command",
			Value: "curl -H 'Authorization: Bearer " + secret + "'",
			Extra: map[string]any{"note": "token " + secret},
		},
		{Kind: "url", Value: "https://example.test/" + secret},
	})

	// Sanity: the corpus really does contain the secret before we scrub.
	if hits := scanEveryTextColumn(t, s, secret); len(hits) == 0 {
		t.Fatal("fixture did not store the secret anywhere — test is vacuous")
	}

	report, err := Scrub(context.Background(), s.DB(), redact.Default(), ScrubOptions{})
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if report.ExtractionsRewritten == 0 {
		t.Errorf("expected extractions to be rewritten, report=%+v", report)
	}

	if hits := scanEveryTextColumn(t, s, secret); len(hits) > 0 {
		t.Errorf("secret survived Scrub in:\n  %s", strings.Join(hits, "\n  "))
	}
}

// TestScrub_ClearsSecretFromFTSIndexes pins the searchability half.
// A scrubbed base column with a stale FTS shadow still answers
// `aichronicles search <secret>`, which is the user-visible symptom.
func TestScrub_ClearsSecretFromFTSIndexes(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	secret := scrubTestSecret()

	ingestForScrub(t, s, secret, []events.Extraction{
		{Kind: "shell_command", Value: "echo " + secret},
	})

	if _, err := Scrub(context.Background(), s.DB(), redact.Default(), ScrubOptions{}); err != nil {
		t.Fatalf("Scrub: %v", err)
	}

	for _, tbl := range []struct{ name, col string }{
		{"events_fts", "content_text"},
		{"extractions_fts", "value"},
	} {
		var n int
		// Quote the needle: FTS5 barewords can't contain punctuation.
		q := `SELECT COUNT(*) FROM ` + tbl.name + ` WHERE ` + tbl.name + ` MATCH ?`
		if err := s.DB().QueryRow(q, `"`+secret+`"`).Scan(&n); err != nil {
			t.Fatalf("%s match: %v", tbl.name, err)
		}
		if n != 0 {
			t.Errorf("%s still matches the secret (%d rows)", tbl.name, n)
		}
	}
}

// TestScrub_DryRunWritesNothing guards the safe default: a dry run
// must report what it would do without mutating a single row.
func TestScrub_DryRunWritesNothing(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	secret := scrubTestSecret()

	ingestForScrub(t, s, secret, []events.Extraction{
		{Kind: "shell_command", Value: "echo " + secret},
	})
	before := scanEveryTextColumn(t, s, secret)

	report, err := Scrub(context.Background(), s.DB(), redact.Default(), ScrubOptions{DryRun: true})
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if !report.DryRun {
		t.Error("report.DryRun must be true")
	}
	if report.ExtractionsRewritten == 0 {
		t.Error("dry run should still report what it would rewrite")
	}

	after := scanEveryTextColumn(t, s, secret)
	if len(after) != len(before) {
		t.Errorf("dry run mutated the database: %d hits before, %d after", len(before), len(after))
	}
}

// TestScrub_CleanCorpusIsANoop guards against Scrub churning rows (and
// so invalidating the ingest-time pattern lists) when it finds nothing.
func TestScrub_CleanCorpusIsANoop(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ingestForScrub(t, s, "no-secrets-here", []events.Extraction{
		{Kind: "shell_command", Value: "go test ./..."},
	})

	report, err := Scrub(context.Background(), s.DB(), redact.Default(), ScrubOptions{})
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if report.EnvelopesRewritten != 0 || report.ExtractionsRewritten != 0 || report.LLMOutputsRewritten != 0 {
		t.Errorf("clean corpus should rewrite nothing, got %+v", report)
	}
	if report.ExtractionsScanned == 0 {
		t.Error("expected extractions to be scanned even when clean")
	}
}

// scanEveryTextColumn returns "table.column (rowid=N)" for every TEXT
// column in the schema whose value contains needle. Discovering the
// columns from sqlite_master rather than hardcoding them is what makes
// the caller a real invariant instead of a checklist that rots.
func scanEveryTextColumn(t *testing.T, s *Store, needle string) []string {
	t.Helper()
	var hits []string

	tables, err := s.DB().Query(
		`SELECT name FROM sqlite_master WHERE type = 'table'
		   AND name NOT LIKE 'sqlite_%'
		 ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var names []string
	for tables.Next() {
		var n string
		if err := tables.Scan(&n); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, n)
	}
	if err := tables.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	_ = tables.Close()

	for _, tbl := range names {
		cols, err := s.DB().Query(`SELECT name, type FROM pragma_table_info(?)`, tbl)
		if err != nil {
			// FTS shadow tables reject table_info; they are covered
			// by the dedicated MATCH assertions instead.
			continue
		}
		var textCols []string
		for cols.Next() {
			var name, typ string
			if err := cols.Scan(&name, &typ); err != nil {
				t.Fatalf("scan column info: %v", err)
			}
			switch strings.ToUpper(typ) {
			case "TEXT", "BLOB", "":
				textCols = append(textCols, name)
			}
		}
		_ = cols.Close()

		for _, col := range textCols {
			rows, err := s.DB().Query(
				`SELECT rowid FROM "`+tbl+`" WHERE CAST("`+col+`" AS TEXT) LIKE ?`,
				"%"+needle+"%")
			if err != nil {
				continue // virtual/shadow table without a rowid
			}
			for rows.Next() {
				var rowid int64
				if err := rows.Scan(&rowid); err != nil {
					break
				}
				hits = append(hits, tbl+"."+col+" (rowid="+strconv.FormatInt(rowid, 10)+")")
			}
			_ = rows.Close()
		}
	}
	return hits
}
