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

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/store"
)

// pointStoreEnv arranges for the completion func's
// paths.ResolveStorePath("") to land on a fresh per-test store
// by setting AICHRONICLES_DB AND points the apiclient at a
// non-existent UDS via AICHRONICLES_API_SOCKET. Without the
// socket override the completion command dials the running
// daemon's production socket (the default location), making
// the test depend on whether `aichronicles-api.service` is
// active — empty-store assertions fail under a real daemon
// even with AICHRONICLES_DB set. t.Setenv restores both on
// cleanup.
func pointStoreEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "store.db")
	t.Setenv("AICHRONICLES_DB", dbPath)
	t.Setenv("AICHRONICLES_API_SOCKET", filepath.Join(dir, "nope.sock"))
	return dbPath
}

// seedSessionForCompletion ingests one envelope and returns the
// derived session id, mirroring the helper used in
// internal/web/handlers_test.go but local to the cli package.
func seedSessionForCompletion(t *testing.T, dbPath, sourceSession, prompt string) string {
	t.Helper()
	s, err := store.OpenMigrate(dbPath)
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
	if _, _, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli()); err != nil {
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

	st, err := store.OpenMigrate(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	c := apiForStore(t, st)
	out, dir := completeSessionIDFrom(cmd, c, "")
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

	st, err := store.OpenMigrate(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	c := apiForStore(t, st)
	out, _ := completeSessionIDFrom(cmd, c, a[:8])
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
