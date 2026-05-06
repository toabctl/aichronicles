package events

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func validEnvelope() Envelope {
	return Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-abc",
		Kind:            "user_prompt",
		TsSource:        time.Now().UTC(),
		Payload:         map[string]any{},
	}
}

func TestValidate_Valid(t *testing.T) {
	t.Parallel()
	e := validEnvelope()
	if err := e.Validate(); err != nil {
		t.Fatalf("expected valid envelope, got error: %v", err)
	}
}

func TestValidate_InvalidCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*Envelope)
		want   string // substring of the reported issue
	}{
		{"wrong v", func(e *Envelope) { e.V = 2 }, "v must be 1"},
		{"bad event_id", func(e *Envelope) { e.EventID = "not-a-uuid" }, "event_id must be a UUID"},
		{"uppercase agent", func(e *Envelope) { e.SourceAgent = "Claude-Code" }, "source_agent"},
		{"empty agent", func(e *Envelope) { e.SourceAgent = "" }, "source_agent"},
		{"agent starts with digit", func(e *Envelope) { e.SourceAgent = "1claude" }, "source_agent"},
		{"missing source_session_id", func(e *Envelope) { e.SourceSessionID = "" }, "source_session_id"},
		{"missing kind", func(e *Envelope) { e.Kind = "" }, "kind is required"},
		{"unknown kind", func(e *Envelope) { e.Kind = "tool_us" }, `kind "tool_us" is not a known canonical kind`},
		{"uppercase kind", func(e *Envelope) { e.Kind = "USER_PROMPT" }, "not a known canonical kind"},
		{"unknown role", func(e *Envelope) { e.Role = "USER" }, `role "USER" is not a known canonical role`},
		{"zero ts_source", func(e *Envelope) { e.TsSource = time.Time{} }, "ts_source is required"},
		{"nil payload", func(e *Envelope) { e.Payload = nil }, "payload is required"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := validEnvelope()
			tc.mutate(&e)

			err := e.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("expected errors.Is(err, ErrInvalid), got %v", err)
			}
			var vErr *ValidationError
			if !errors.As(err, &vErr) {
				t.Fatalf("expected ValidationError, got %T", err)
			}
			found := false
			for _, issue := range vErr.Issues {
				if strings.Contains(issue, tc.want) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("no issue contained %q; issues=%v", tc.want, vErr.Issues)
			}
		})
	}
}

func TestValidate_CollectsMultipleIssues(t *testing.T) {
	t.Parallel()
	e := Envelope{} // every required field missing
	err := e.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected ValidationError, got %T", err)
	}
	if len(vErr.Issues) < 5 {
		t.Fatalf("expected multiple issues collected in one pass, got %d: %v", len(vErr.Issues), vErr.Issues)
	}
}

func TestDeriveSessionID_Deterministic(t *testing.T) {
	t.Parallel()
	a := DeriveSessionID("claude-code", "sess-abc")
	b := DeriveSessionID("claude-code", "sess-abc")
	if a != b {
		t.Fatalf("expected identical IDs for same input, got %q vs %q", a, b)
	}
	if _, err := uuid.Parse(a); err != nil {
		t.Fatalf("expected a UUID, got %q", a)
	}
}

func TestDeriveSessionID_DistinctForDifferentInputs(t *testing.T) {
	t.Parallel()
	same := DeriveSessionID("claude-code", "sess-abc")
	diffSession := DeriveSessionID("claude-code", "sess-xyz")
	diffAgent := DeriveSessionID("crush", "sess-abc")

	if same == diffSession {
		t.Error("expected different session IDs for different source_session_id")
	}
	if same == diffAgent {
		t.Error("expected different session IDs for different source_agent (collision risk)")
	}
}
