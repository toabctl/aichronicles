package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
)

// runOne pipes a single request into Server.Run and returns the
// response it emitted. Helper used across handshake tests.
func runOne(t *testing.T, s *Server, req string) map[string]any {
	t.Helper()
	in, inW := io.Pipe()
	out := &bytes.Buffer{}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.Run(context.Background(), in, out)
	}()

	if _, err := io.WriteString(inW, req+"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = inW.Close()
	wg.Wait()

	var resp map[string]any
	// Only the first line is the request's response; notifications
	// (no id) produce nothing, so some calls return an empty buffer.
	trimmed := strings.TrimSpace(out.String())
	if trimmed == "" {
		return nil
	}
	// First newline-delimited object is our response.
	first := strings.SplitN(trimmed, "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &resp); err != nil {
		t.Fatalf("decode response %q: %v", first, err)
	}
	return resp
}

func TestServer_Initialize_NegotiatesProtocol(t *testing.T) {
	t.Parallel()
	s := New(ServerInfo{Name: "test", Version: "0.1"}, nil)

	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"claude-desktop","version":"0.5"}}}`
	resp := runOne(t, s, req)

	if resp["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc: got %v", resp["jsonrpc"])
	}
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result missing: %+v", resp)
	}
	if result["protocolVersion"] != ProtocolVersion {
		t.Errorf("protocolVersion: got %v, want %s", result["protocolVersion"], ProtocolVersion)
	}
	srvInfo := result["serverInfo"].(map[string]any)
	if srvInfo["name"] != "test" {
		t.Errorf("serverInfo.name: got %v", srvInfo["name"])
	}
	caps := result["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Error("tools capability missing")
	}
}

func TestServer_Initialized_NotificationHasNoResponse(t *testing.T) {
	t.Parallel()
	s := New(ServerInfo{Name: "test", Version: "0.1"}, nil)

	// Notifications have no id — per JSON-RPC rules, no response.
	resp := runOne(t, s, `{"jsonrpc":"2.0","method":"initialized"}`)
	if resp != nil {
		t.Errorf("notification produced a response: %+v", resp)
	}
	if !s.Initialized() {
		t.Error("Initialized() should be true after the notification")
	}
}

func TestServer_Ping_ReturnsEmptyObject(t *testing.T) {
	t.Parallel()
	s := New(ServerInfo{Name: "test", Version: "0.1"}, nil)
	resp := runOne(t, s, `{"jsonrpc":"2.0","id":"p1","method":"ping"}`)
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	if _, ok := resp["result"]; !ok {
		t.Error("result missing")
	}
}

func TestServer_UnknownMethod_ReturnsMethodNotFound(t *testing.T) {
	t.Parallel()
	s := New(ServerInfo{Name: "test", Version: "0.1"}, nil)
	resp := runOne(t, s, `{"jsonrpc":"2.0","id":7,"method":"does_not_exist"}`)
	errObj := resp["error"].(map[string]any)
	code := int(errObj["code"].(float64))
	if code != MethodNotFound {
		t.Errorf("code: got %d, want %d", code, MethodNotFound)
	}
}

func TestServer_MalformedJSON_ReturnsParseError(t *testing.T) {
	t.Parallel()
	s := New(ServerInfo{Name: "test", Version: "0.1"}, nil)
	resp := runOne(t, s, `{"jsonrpc":"2.0","id":1,`) // truncated
	errObj := resp["error"].(map[string]any)
	code := int(errObj["code"].(float64))
	if code != ParseError {
		t.Errorf("code: got %d, want %d", code, ParseError)
	}
	// Parse error id must be null per spec — we emit json null.
	if resp["id"] != nil {
		t.Errorf("id on parse error: got %v, want null", resp["id"])
	}
}

func TestServer_WrongJSONRPCVersion_Rejected(t *testing.T) {
	t.Parallel()
	s := New(ServerInfo{Name: "test", Version: "0.1"}, nil)
	resp := runOne(t, s, `{"jsonrpc":"1.0","id":1,"method":"ping"}`)
	errObj := resp["error"].(map[string]any)
	code := int(errObj["code"].(float64))
	if code != InvalidRequest {
		t.Errorf("code: got %d, want %d", code, InvalidRequest)
	}
}

func TestServer_EmptyLinesIgnored(t *testing.T) {
	t.Parallel()
	s := New(ServerInfo{Name: "test", Version: "0.1"}, nil)

	in, inW := io.Pipe()
	out := &bytes.Buffer{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = s.Run(context.Background(), in, out) }()

	_, _ = io.WriteString(inW, "\n\n\n")
	_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n")
	_ = inW.Close()
	wg.Wait()

	// Exactly one response line — the ping. Empties must NOT emit
	// parse errors.
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 response, got %d:\n%s", len(lines), out.String())
	}
}

func TestServer_MultipleRequestsSequentially(t *testing.T) {
	t.Parallel()
	s := New(ServerInfo{Name: "test", Version: "0.1"}, nil)

	in, inW := io.Pipe()
	out := &bytes.Buffer{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = s.Run(context.Background(), in, out) }()

	for i := 0; i < 3; i++ {
		_, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":`+string(rune('0'+i))+`,"method":"ping"}`+"\n")
	}
	_ = inW.Close()
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 responses, got %d:\n%s", len(lines), out.String())
	}
}
