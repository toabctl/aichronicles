package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/mcp"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
)

// TestMCPServe_EndToEnd pipes a minimal MCP handshake + tools/list
// sequence through the assembled server. Covers the wiring between
// mcp.New, RegisterAichroniclesTools, and *store.Store without
// stubbing any of them.
func TestMCPServe_EndToEnd(t *testing.T) {
	t.Parallel()

	s, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	// Seed one searchable event so tools/call on search_events has
	// something real to return.
	env := ingest.Envelope{
		V:               1,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-mcp",
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        time.Now().UTC(),
		Cwd:             "/work/mcp",
		ContentText:     "mcp end to end test marker",
		Payload:         map[string]any{},
		Redaction:       &ingest.Redaction{Applied: true},
	}
	raw, _ := json.Marshal(env)
	tx, _ := s.DB().Begin()
	if _, err := store.IngestEnvelope(t.Context(), tx, &env, raw, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed: %v", err)
	}
	_ = tx.Commit()

	srv := mcp.New(mcp.ServerInfo{Name: mcpServerName, Version: mcpServerVersion}, nil)
	mcp.RegisterAichroniclesTools(srv, s)

	in, inW := io.Pipe()
	out := &bytes.Buffer{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = srv.Run(context.Background(), in, out) }()

	// 1. initialize
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"t","version":"0"}}}`+"\n")
	// 2. initialized (notification, no response)
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","method":"initialized"}`+"\n")
	// 3. tools/list
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n")
	// 4. tools/call search_events
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_events","arguments":{"query":"marker"}}}`+"\n")
	_ = inW.Close()
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// Expect exactly 3 responses (initialize, tools/list, tools/call).
	// The "initialized" notification does NOT produce a response.
	if len(lines) != 3 {
		t.Fatalf("expected 3 responses, got %d:\n%s", len(lines), out.String())
	}

	var initResp, listResp, callResp map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &initResp)
	_ = json.Unmarshal([]byte(lines[1]), &listResp)
	_ = json.Unmarshal([]byte(lines[2]), &callResp)

	// Sanity: initialize echoes back our server name.
	if initResp["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != mcpServerName {
		t.Errorf("serverInfo.name: got %+v", initResp["result"])
	}
	// Sanity: tools/list returns the aichronicles set (currently 8).
	tools := listResp["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 8 {
		t.Errorf("tools/list returned %d tools, want 8", len(tools))
	}
	// Sanity: tools/call search_events finds our seeded event.
	content := callResp["result"].(map[string]any)["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "marker") {
		t.Errorf("search_events did not return seeded event:\n%s", text)
	}
}
