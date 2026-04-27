package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeGeminiSession writes a session-*.json fixture under
// <root>/<label>/chats/. Mirrors gemini-cli's actual layout.
func writeGeminiSession(t *testing.T, root, label string, sess map[string]any) string {
	t.Helper()
	chats := filepath.Join(root, label, "chats")
	if err := os.MkdirAll(chats, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body, _ := json.Marshal(sess)
	path := filepath.Join(chats, "session-"+sess["sessionId"].(string)+".json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestImportGeminiTranscripts_HappyPath_UserAndAssistant(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	tmp := t.TempDir()

	writeGeminiSession(t, tmp, "project-a", map[string]any{
		"sessionId":   "abc12345-aaaa-bbbb-cccc-dddddddddddd",
		"projectHash": "deadbeef",
		"startTime":   "2026-04-26T10:00:00.000Z",
		"lastUpdated": "2026-04-26T10:00:30.000Z",
		"kind":        "main",
		"messages": []map[string]any{
			{
				"id":        "u1111111-1111-1111-1111-111111111111",
				"timestamp": "2026-04-26T10:00:00.500Z",
				"type":      "user",
				"content":   []map[string]any{{"text": "what's 2+2?"}},
			},
			{
				"id":        "a2222222-2222-2222-2222-222222222222",
				"timestamp": "2026-04-26T10:00:01.500Z",
				"type":      "gemini",
				"content":   "2 + 2 is 4.",
				"model":     "gemini-3-flash-preview",
			},
		},
	})

	report, err := ImportGeminiTranscripts(t.Context(), tmp, s, nil)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if report.FilesRead != 1 {
		t.Errorf("FilesRead: got %d, want 1", report.FilesRead)
	}
	if report.MessagesRead != 2 {
		t.Errorf("MessagesRead: got %d, want 2", report.MessagesRead)
	}
	if report.Imported != 2 {
		t.Errorf("Imported: got %d, want 2", report.Imported)
	}

	// One user_prompt + one assistant_message under gemini-cli.
	var n int
	_ = s.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE source_agent = ?`, geminiSourceAgent,
	).Scan(&n)
	if n != 2 {
		t.Errorf("rows: got %d, want 2", n)
	}
}

// TestImportGeminiTranscripts_ToolCallProducesUseAndResult covers
// the fan-out: one assistant message with N toolCalls produces a
// (possibly-empty) assistant_message + 2N tool envelopes.
func TestImportGeminiTranscripts_ToolCallProducesUseAndResult(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	tmp := t.TempDir()

	writeGeminiSession(t, tmp, "project-b", map[string]any{
		"sessionId": "bbb22222-aaaa-bbbb-cccc-dddddddddddd",
		"startTime": "2026-04-26T10:00:00.000Z",
		"messages": []map[string]any{
			{
				"id":        "u3333333-1111-1111-1111-111111111111",
				"timestamp": "2026-04-26T10:00:00.000Z",
				"type":      "user",
				"content":   []map[string]any{{"text": "list files"}},
			},
			{
				"id":        "a4444444-2222-2222-2222-222222222222",
				"timestamp": "2026-04-26T10:00:01.000Z",
				"type":      "gemini",
				"content":   "I'll list the files.",
				"model":     "gemini-3-flash-preview",
				"toolCalls": []map[string]any{
					{
						"id":        "run_shell_command_1",
						"name":      "run_shell_command",
						"args":      map[string]any{"command": "ls"},
						"status":    "success",
						"timestamp": "2026-04-26T10:00:01.500Z",
						"result": []map[string]any{
							{"functionResponse": map[string]any{
								"id":   "run_shell_command_1",
								"name": "run_shell_command",
								"response": map[string]any{
									"output": "a\nb\nc",
								},
							}},
						},
					},
				},
			},
		},
	})

	report, err := ImportGeminiTranscripts(t.Context(), tmp, s, nil)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// 2 messages read; envelopes = user_prompt + assistant_message + tool_use + tool_result = 4
	if report.MessagesRead != 2 {
		t.Errorf("MessagesRead: got %d, want 2", report.MessagesRead)
	}
	if report.Imported != 4 {
		t.Errorf("Imported: got %d, want 4 (user + assistant + tool_use + tool_result)", report.Imported)
	}

	// Check kinds landed correctly.
	rows, err := s.DB().Query(
		`SELECT kind, content_text FROM events WHERE source_agent = ? ORDER BY ts_source_ms ASC, kind ASC`,
		geminiSourceAgent,
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var k, c string
		_ = rows.Scan(&k, &c)
		got = append(got, k)
	}
	want := []string{"user_prompt", "assistant_message", "tool_use", "tool_result"}
	if len(got) != len(want) {
		t.Fatalf("kinds: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("kind[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestImportGeminiTranscripts_IsIdempotent(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	tmp := t.TempDir()
	writeGeminiSession(t, tmp, "p", map[string]any{
		"sessionId": "ccc33333-aaaa-bbbb-cccc-dddddddddddd",
		"startTime": "2026-04-26T10:00:00.000Z",
		"messages": []map[string]any{
			{
				"id":        "u5555555-1111-1111-1111-111111111111",
				"timestamp": "2026-04-26T10:00:00.000Z",
				"type":      "user",
				"content":   []map[string]any{{"text": "hi"}},
			},
		},
	})
	for range 3 {
		if _, err := ImportGeminiTranscripts(t.Context(), tmp, s, nil); err != nil {
			t.Fatalf("import: %v", err)
		}
	}
	var n int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE source_agent = ?`, geminiSourceAgent).Scan(&n)
	if n != 1 {
		t.Errorf("after 3 imports: got %d events, want 1", n)
	}
}

func TestImportGeminiTranscripts_HandlesMalformedJSON(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	tmp := t.TempDir()

	// Bad file
	badDir := filepath.Join(tmp, "bad", "chats")
	_ = os.MkdirAll(badDir, 0o755)
	_ = os.WriteFile(filepath.Join(badDir, "session-bad.json"), []byte("{not json"), 0o644)

	// Good file
	writeGeminiSession(t, tmp, "good", map[string]any{
		"sessionId": "ddd44444-aaaa-bbbb-cccc-dddddddddddd",
		"startTime": "2026-04-26T10:00:00.000Z",
		"messages": []map[string]any{
			{
				"id":        "u6666666-1111-1111-1111-111111111111",
				"timestamp": "2026-04-26T10:00:00.000Z",
				"type":      "user",
				"content":   []map[string]any{{"text": "ok"}},
			},
		},
	})

	report, err := ImportGeminiTranscripts(t.Context(), tmp, s, nil)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if report.Invalid < 1 {
		t.Errorf("expected >=1 invalid, got %d", report.Invalid)
	}
	if report.Imported != 1 {
		t.Errorf("good file should have imported 1, got %d", report.Imported)
	}
}

func TestImportGeminiTranscripts_ToolFailureMappedToFailureKind(t *testing.T) {
	t.Parallel()
	s := openTempCLIStore(t)
	tmp := t.TempDir()
	writeGeminiSession(t, tmp, "p", map[string]any{
		"sessionId": "eee55555-aaaa-bbbb-cccc-dddddddddddd",
		"startTime": "2026-04-26T10:00:00.000Z",
		"messages": []map[string]any{
			{
				"id":        "a7777777-1111-1111-1111-111111111111",
				"timestamp": "2026-04-26T10:00:00.000Z",
				"type":      "gemini",
				"content":   "trying...",
				"toolCalls": []map[string]any{
					{
						"id":     "tc-fail",
						"name":   "run_shell_command",
						"args":   map[string]any{"command": "false"},
						"status": "error",
						"result": []map[string]any{
							{"functionResponse": map[string]any{
								"name": "run_shell_command",
								"response": map[string]any{
									"output": "exit 1",
								},
							}},
						},
					},
				},
			},
		},
	})
	if _, err := ImportGeminiTranscripts(t.Context(), tmp, s, nil); err != nil {
		t.Fatalf("import: %v", err)
	}
	var n int
	_ = s.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE source_agent = ? AND kind = 'tool_failure'`,
		geminiSourceAgent,
	).Scan(&n)
	if n != 1 {
		t.Errorf("tool_failure rows: got %d, want 1", n)
	}
}

// TestGeminiMessageToEnvelopes_StableEventID confirms two runs over
// the same message produce the same envelopes — the load-bearing
// invariant for idempotent re-import.
func TestGeminiMessageToEnvelopes_StableEventID(t *testing.T) {
	t.Parallel()
	sess := &geminiSession{SessionID: "s1"}
	m := &geminiMessage{
		ID:        "u8888888-1111-1111-1111-111111111111",
		Timestamp: "2026-04-26T10:00:00.000Z",
		Type:      "user",
		Content:   json.RawMessage(`[{"text":"hi"}]`),
	}
	first, err := geminiMessageToEnvelopes(sess, m, "/work")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := geminiMessageToEnvelopes(sess, m, "/work")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first[0].EventID != second[0].EventID {
		t.Errorf("event_id not stable: %q vs %q", first[0].EventID, second[0].EventID)
	}
	if first[0].EventID != m.ID {
		t.Errorf("user event_id should be the message UUID, got %q", first[0].EventID)
	}
}
