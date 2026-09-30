package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

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

// TestTools_ApplyDeclaredSchemaBounds pins the maxima the tool schemas
// advertise to what the tools send: find_episodes since_days (365),
// get_skill_staleness window_minutes (240) and get_insights top_tools
// / top_skills (50) were declared but passed through unclamped.
func TestTools_ApplyDeclaredSchemaBounds(t *testing.T) {
	t.Parallel()
	st := openSeededStore(t)
	s := New(ServerInfo{Name: "ac", Version: "0.1"}, slog.New(slog.DiscardHandler))
	cq := registerAllToolsCapturing(t, s, st)
	now := time.Now()

	callTool(t, s, "get_insights", `{"top_tools":500,"top_skills":900}`)
	q := cq.get("/v1/insights")
	if len(q) != 1 || q[0].Get("top_tools") != "50" || q[0].Get("top_skills") != "50" {
		t.Errorf("get_insights sent %v, want top_tools=50 top_skills=50", q)
	}
	callTool(t, s, "get_skill_staleness", `{"window_minutes":10000}`)
	q = cq.get("/v1/skills/staleness")
	if len(q) != 1 || q[0].Get("window_ms") != strconv.Itoa(240*60*1000) {
		t.Errorf("get_skill_staleness sent %v, want window_ms for 240 minutes", q)
	}
	callTool(t, s, "find_episodes", `{"since_days":100000}`)
	q = cq.get("/v1/episodes")
	if len(q) != 1 {
		t.Fatalf("find_episodes made %d episode calls", len(q))
	}
	since, err := strconv.ParseInt(q[0].Get("since_ms"), 10, 64)
	if err != nil {
		t.Fatalf("since_ms: %v", err)
	}
	if floor := now.Add(-366 * 24 * time.Hour).UnixMilli(); since < floor {
		t.Errorf("find_episodes since_ms %d reaches back past 365 days", since)
	}
}
