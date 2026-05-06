package apiclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/api"
	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/store"
)

func validEnvelope(t *testing.T) events.Envelope {
	t.Helper()
	return events.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-abc",
		Kind:            "user_prompt",
		TsSource:        time.Now().UTC(),
		Payload:         map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "hi"},
	}
}

// newRealServerClient stands up the actual internal/api server
// over httptest (TCP, but the server's wire shape is identical).
// Returns a Client wired to it. Verifies the Ingest method end-
// to-end against the real handler instead of a stub.
func newRealServerClient(t *testing.T) (*Client, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := httptest.NewServer(api.NewServer(st, nil).Handler())
	t.Cleanup(srv.Close)

	return newClientForTests(srv.Client(), srv.URL), st
}

func TestClient_Ingest_HappyPath(t *testing.T) {
	t.Parallel()
	c, st := newRealServerClient(t)
	env := validEnvelope(t)

	ack, err := c.Ingest(context.Background(), env)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if ack.EventID != env.EventID {
		t.Errorf("Ack.EventID: got %q, want %q", ack.EventID, env.EventID)
	}
	if ack.Deduped {
		t.Errorf("first ingest must not be Deduped")
	}

	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if n != 1 {
		t.Errorf("events table: got %d, want 1", n)
	}
}

func TestClient_Ingest_DuplicateReturnsDeduped(t *testing.T) {
	t.Parallel()
	c, _ := newRealServerClient(t)
	env := validEnvelope(t)

	if _, err := c.Ingest(context.Background(), env); err != nil {
		t.Fatalf("first Ingest: %v", err)
	}
	ack, err := c.Ingest(context.Background(), env)
	if err != nil {
		t.Fatalf("second Ingest: %v", err)
	}
	if !ack.Deduped {
		t.Errorf("second Ingest must be Deduped, got %+v", ack)
	}
}

func TestClient_Ingest_InvalidEnvelope_IsBadRequestError(t *testing.T) {
	t.Parallel()
	c, _ := newRealServerClient(t)

	// Build an envelope that fails Validate: blank EventID.
	env := validEnvelope(t)
	env.EventID = ""

	_, err := c.Ingest(context.Background(), env)
	if err == nil {
		t.Fatal("expected validation error")
	}
	var herr *HTTPError
	if !errors.As(err, &herr) {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if herr.Status != http.StatusBadRequest {
		t.Errorf("Status: got %d, want 400", herr.Status)
	}
}

func TestClient_Ingest_DaemonDown_IsErrSocketUnavailable(t *testing.T) {
	t.Parallel()
	// Real UDS path that doesn't exist — exercises the
	// production constructor end-to-end.
	missing := filepath.Join(t.TempDir(), "nope.sock")
	c := NewClient(missing)
	_, err := c.Ingest(context.Background(), validEnvelope(t))
	if !errors.Is(err, ErrSocketUnavailable) {
		t.Errorf("got %v, want ErrSocketUnavailable", err)
	}
}
