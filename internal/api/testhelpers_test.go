package api

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// Test-only helpers shared across handler_*_test.go files. Keep
// them tiny — each helper exists because the same boilerplate
// shows up in 3+ tests.

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
