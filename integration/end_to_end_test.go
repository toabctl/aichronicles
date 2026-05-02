//go:build integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/toabctl/aichronicles/internal/api"
	"github.com/toabctl/aichronicles/internal/cli"
	"github.com/toabctl/aichronicles/internal/mcp"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/events"
)

// TestE2E_IngestSummarizeFetchViaMCP exercises Block A → B → C in a
// single run. The scenario models the worst-case composition:
//
//  1. A user pastes an API key into a prompt. Block A MUST scrub it
//     before it reaches raw_envelopes / events.
//  2. We simulate Block B summarizing that session. The simulated LLM
//     response HALLUCINATES a fresh credential-shaped string (a
//     realistic failure mode: "your typical key format is sk-ant-...").
//     We persist that output verbatim to model an unscrubbed LLM reply.
//  3. An MCP client calls list_sessions and get_summary for the
//     session. Block C's egress scrub in handleToolsCall MUST catch
//     the hallucinated credential — even though Block A/B never saw
//     it — before it crosses the MCP boundary.
//
// The test asserts that at no point in the pipeline do raw secrets
// reach the reader, and that the hallucinated one picked up by Block
// C's egress guard is rewritten to a marker.
func TestE2E_IngestSummarizeFetchViaMCP(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")

	s, err := store.Open(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	// --- Block A: ingest via the daemon path, secret in prompt ---
	srv := api.NewServer(s, nil)
	shutdown, err := api.ListenAndServe(sock, srv.Handler())
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	pastedKey := "sk-ant-" + strings.Repeat("a", 40)
	hook := []byte(`{
		"session_id": "e2e-pipeline",
		"hook_event_name": "UserPromptSubmit",
		"cwd": "/work/e2e",
		"prompt": "here is my key ` + pastedKey + ` please help"
	}`)

	var stderr bytes.Buffer
	if err := cli.RunHook(bytes.NewReader(hook), &stderr, sock, ""); err != nil {
		t.Fatalf("RunIngest: %v", err)
	}

	sessID := events.DeriveSessionID("claude-code", "e2e-pipeline")

	// Sanity: the ingested event is scrubbed.
	var content string
	_ = s.DB().QueryRow(
		`SELECT content_text FROM events WHERE session_id = ?`, sessID,
	).Scan(&content)
	if strings.Contains(content, "sk-ant-") {
		t.Fatalf("ingest leaked raw key into events.content_text: %q", content)
	}

	// --- Block B simulation: a summary whose body hallucinates a key ---
	// We skip the actual LLM round-trip and plant the result directly
	// via SaveLLMOutput. The fresh secret here is different from the
	// one in the prompt — it's exactly the "LLM makes one up" scenario
	// the SaveLLMOutput body-scrub exists for. Read paths (MCP, web,
	// CLI) no longer re-scrub; the write path is the single
	// enforcement point.
	hallucinated := "sk-ant-" + strings.Repeat("h", 40)
	summaryBody := "Topic: API key handling.\n" +
		"Example of the format that was discussed: " + hallucinated + ".\n" +
		"Recommendation: do not paste keys."
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, _, err = store.SaveLLMOutput(t.Context(), tx, &store.LLMOutput{
		SessionID:   sql.NullString{String: sessID, Valid: true},
		Kind:        store.LLMKindSummary,
		Model:       "claude-sonnet-4-6",
		PromptHash:  "e2e-hash",
		Body:        summaryBody,
		CreatedAtMs: time.Now().UnixMilli(),
	})
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed llm_output: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// --- Block C: fetch via MCP, assert nothing leaks ---
	// Both secrets (the originally-pasted one scrubbed at edge, and
	// the LLM-hallucinated one scrubbed at SaveLLMOutput) should be
	// gone by the time the wire sees them. The MCP dispatcher itself
	// passes content through verbatim — the protection is upstream
	// at the two write paths.
	mcpSrv := mcp.New(mcp.ServerInfo{Name: "e2e", Version: "0.0.1"}, nil)
	mcp.RegisterAichroniclesTools(mcpSrv, s)

	// Drive the MCP server through the JSON-RPC wire as a real
	// client would. The dispatcher passes content through verbatim
	// — secrets must already be gone by the time data reaches it.
	in, inW := io.Pipe()
	out := &bytes.Buffer{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = mcpSrv.Run(context.Background(), in, out) }()

	writeLn(inW, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"t","version":"0"}}}`)
	writeLn(inW, `{"jsonrpc":"2.0","method":"initialized"}`)
	writeLn(inW, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_sessions","arguments":{}}}`)
	writeLn(inW, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_summary","arguments":{"session_id":"`+sessID+`"}}}`)
	_ = inW.Close()
	wg.Wait()

	wire := out.String()
	// Block A guarantee: no trace of the originally-pasted key.
	if strings.Contains(wire, pastedKey) {
		t.Fatalf("originally-pasted key reached the MCP wire:\n%s", wire)
	}
	if strings.Contains(wire, "sk-ant-a") {
		t.Fatalf("originally-pasted key substring reached the MCP wire:\n%s", wire)
	}
	// SaveLLMOutput guarantee: the hallucinated key (never seen by
	// the edge redactor at ingest, since LLM responses don't go
	// through /v1/ingest) is caught at the llm_outputs write path.
	if strings.Contains(wire, hallucinated) {
		t.Fatalf("LLM-hallucinated key leaked to MCP client:\n%s", wire)
	}
	if strings.Contains(wire, "sk-ant-h") {
		t.Fatalf("LLM-hallucinated key substring leaked to MCP client:\n%s", wire)
	}
	// And the marker should be visible somewhere so the client can
	// reason about what happened. JSON HTML-escape makes the chevrons
	// encoded; match the pattern name either way.
	if !strings.Contains(wire, "redacted:anthropic_api_key") {
		t.Errorf("expected anthropic_api_key marker in MCP wire output:\n%s", wire)
	}
}

// writeLn appends a newline-terminated JSON-RPC frame to the pipe.
// Ignores write errors — EOF on the pipe is the expected wind-down.
func writeLn(w io.Writer, s string) {
	_, _ = io.WriteString(w, s+"\n")
}
