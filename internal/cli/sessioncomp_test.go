package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/events"
	"github.com/toabctl/aichronicles/pkg/redact"
)

// pointStoreEnv arranges for the completion func's
// paths.ResolveStorePath("") to land on a fresh per-test store
// by setting AICHRONICLES_DB. t.Setenv restores on cleanup.
func pointStoreEnv(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "store.db")
	t.Setenv("AICHRONICLES_DB", dbPath)
	return dbPath
}

// seedSessionForCompletion ingests one envelope and returns the
// derived session id, mirroring the helper used in
// internal/web/handlers_test.go but local to the cli package.
func seedSessionForCompletion(t *testing.T, dbPath, sourceSession, prompt string) string {
	t.Helper()
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	env := events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: sourceSession,
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        time.Now().UTC(),
		Cwd:             "/work/" + sourceSession,
		ContentText:     prompt,
		Payload:         map[string]any{},
		Transport:       "hook",
	}
	events.ApplyRedaction(&env, redact.Default())
	raw, _ := json.Marshal(&env)
	tx, _ := s.DB().Begin()
	if _, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("IngestEnvelope: %v", err)
	}
	_ = tx.Commit()
	return events.DeriveSessionID("claude-code", sourceSession)
}

func TestCompleteSessionID_ReturnsTabSeparatedDescriptions(t *testing.T) {
	dbPath := pointStoreEnv(t)
	id := seedSessionForCompletion(t, dbPath, "sess-comp", "summarise the redaction story for the docs site")

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())

	out, dir := completeSessionID(cmd, nil, "")
	if dir&cobra.ShellCompDirectiveNoFileComp == 0 {
		t.Errorf("expected NoFileComp directive, got %v", dir)
	}
	if len(out) != 1 {
		t.Fatalf("got %d candidates, want 1", len(out))
	}
	parts := strings.SplitN(out[0], "\t", 2)
	if len(parts) != 2 {
		t.Fatalf("expected id\\tdescription shape, got %q", out[0])
	}
	if parts[0] != id {
		t.Errorf("candidate id: got %q, want %q", parts[0], id)
	}
	if !strings.Contains(parts[1], "summarise the redaction story") {
		t.Errorf("description should preview the prompt, got %q", parts[1])
	}
	if !strings.Contains(parts[1], "/work/sess-comp") {
		t.Errorf("description should include cwd, got %q", parts[1])
	}
}

func TestCompleteSessionID_PrefixFilters(t *testing.T) {
	dbPath := pointStoreEnv(t)
	a := seedSessionForCompletion(t, dbPath, "sess-a", "alpha work")
	_ = seedSessionForCompletion(t, dbPath, "sess-b", "beta work")

	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())

	out, _ := completeSessionID(cmd, nil, a[:8])
	if len(out) != 1 {
		t.Fatalf("prefix should narrow to one candidate; got %d: %v", len(out), out)
	}
	if !strings.HasPrefix(out[0], a+"\t") {
		t.Errorf("candidate should start with full id %q, got %q", a, out[0])
	}
}

func TestCompleteSessionID_EmptyStoreReturnsEmpty(t *testing.T) {
	pointStoreEnv(t)
	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(context.Background())

	out, _ := completeSessionID(cmd, nil, "")
	if len(out) != 0 {
		t.Errorf("empty store should produce zero candidates, got %v", out)
	}
}
