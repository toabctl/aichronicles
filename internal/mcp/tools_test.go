package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/wire"
)

// TestRegisterTool_PanicsOnDuplicate pins the invariant that the
// three Register*Tools entry points (RegisterAichroniclesTools,
// RegisterAichroniclesAPITools, RegisterAichroniclesAnalyticsTools,
// plus RegisterAichroniclesLLMTools) cannot accidentally claim the
// same tool name. Without the panic, the second registration would
// silently shadow the first and the conflict would only surface as
// a "wrong handler ran" mystery at request time.
func TestRegisterTool_PanicsOnDuplicate(t *testing.T) {
	t.Parallel()
	s := New(ServerInfo{Name: "test", Version: "0.1"}, slog.New(slog.DiscardHandler))
	noopHandler := func(_ context.Context, _ json.RawMessage) (*ToolResult, *Error) {
		return TextResult(""), nil
	}
	s.RegisterTool(Tool{Name: "echo", Handler: noopHandler})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("RegisterTool did not panic on duplicate name")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value: got %T, want string", r)
		}
		if !strings.Contains(msg, "echo") {
			t.Errorf("panic message %q does not name the offending tool", msg)
		}
	}()
	s.RegisterTool(Tool{Name: "echo", Handler: noopHandler})
}

// TestMapAPIError_ClassifiesByStatus pins which api failures reach the
// agent as a tool error (fix your arguments) versus a protocol error
// (the server failed). 4xx used to be protocol errors, so a typo'd id
// looked like an internal failure.
func TestMapAPIError_ClassifiesByStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		err      error
		wantTool string // substring of the tool error text; "" = protocol error
	}{
		{"not found", &apiclient.HTTPError{Status: 404, Problem: wire.Problem{Title: "Session not found", Detail: "abcd"}}, "list_sessions: Session not found: abcd"},
		{"bad request, no detail", &apiclient.HTTPError{Status: 400, Problem: wire.Problem{Title: "Invalid prefix"}}, "list_sessions: Invalid prefix"},
		{"conflict", &apiclient.HTTPError{Status: 409, Problem: wire.Problem{Title: "Ambiguous prefix", Detail: "2 matches"}}, "Ambiguous prefix: 2 matches"},
		{"socket down", fmt.Errorf("dial: %w", apiclient.ErrSocketUnavailable), "unreachable"},
		{"server error", &apiclient.HTTPError{Status: 500, Problem: wire.Problem{Title: "Storage error"}}, ""},
		{"transport", errors.New("connection reset"), ""},
	}
	for _, tc := range cases {
		res, perr := mapAPIError("list_sessions", tc.err)
		if tc.wantTool == "" {
			if perr == nil || res != nil {
				t.Errorf("%s: want a protocol error, got res=%+v err=%+v", tc.name, res, perr)
			}
			continue
		}
		if perr != nil || res == nil || !res.IsError || !strings.Contains(res.Content[0].Text, tc.wantTool) {
			t.Errorf("%s: want tool error containing %q, got res=%+v err=%+v", tc.name, tc.wantTool, res, perr)
		}
	}
	if res, perr := mapAPIError("x", nil); res != nil || perr != nil {
		t.Errorf("nil error must map to (nil, nil)")
	}
}
