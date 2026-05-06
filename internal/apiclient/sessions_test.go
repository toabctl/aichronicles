package apiclient

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/wire"
)

func TestClient_Sessions_HappyPath(t *testing.T) {
	t.Parallel()
	c, _ := newRealServerClient(t)

	// Seed two distinct sessions.
	for _, sid := range []string{"sess-A", "sess-B"} {
		env := validEnvelope(t)
		env.SourceSessionID = sid
		env.EventID = uuid.Must(uuid.NewV7()).String()
		if _, err := c.Ingest(context.Background(), env); err != nil {
			t.Fatalf("seed Ingest: %v", err)
		}
	}

	out, err := c.Sessions(context.Background(), wire.SessionListRequest{})
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(out.Sessions) != 2 {
		t.Errorf("Sessions: got %d, want 2", len(out.Sessions))
	}
}

func TestClient_Session_NotFoundIsErrNotFound(t *testing.T) {
	t.Parallel()
	c, _ := newRealServerClient(t)
	_, err := c.Session(context.Background(), "no-such-session")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestClient_Session_FoundReturnsDigest(t *testing.T) {
	t.Parallel()
	c, _ := newRealServerClient(t)
	env := validEnvelope(t)
	env.SourceSessionID = "sess-rt"
	if _, err := c.Ingest(context.Background(), env); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id := events.DeriveSessionID("claude-code", "sess-rt")
	got, err := c.Session(context.Background(), id)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if got.ID != id {
		t.Errorf("ID: got %q, want %q", got.ID, id)
	}
}

func TestClient_RelatedSessions_EmptyForUnknown(t *testing.T) {
	t.Parallel()
	c, _ := newRealServerClient(t)
	out, err := c.RelatedSessions(context.Background(), "ghost", 0)
	if err != nil {
		t.Fatalf("RelatedSessions: %v", err)
	}
	if len(out.Candidates) != 0 {
		t.Errorf("got %d candidates, want 0", len(out.Candidates))
	}
}
