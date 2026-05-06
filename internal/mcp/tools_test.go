package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
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
	s := New(ServerInfo{Name: "test", Version: "0.1"}, nil)
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
