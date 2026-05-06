package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEvent_RoundTripWithNulls(t *testing.T) {
	t.Parallel()
	cwd := "/tmp"
	in := Event{
		IngestSeq:  42,
		EventID:    "ev-1",
		SessionID:  "sess-1",
		Kind:       "user_prompt",
		TsSourceMs: 1700000000000,
		TsServerMs: 1700000000100,
		Cwd:        &cwd,
		Snippet:    nil,
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"cwd":"/tmp"`) {
		t.Errorf("expected cwd populated, got %s", b)
	}
	if strings.Contains(string(b), `"snippet"`) {
		t.Errorf("expected snippet omitted (nil pointer), got %s", b)
	}

	var out Event
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Cwd == nil || *out.Cwd != "/tmp" {
		t.Errorf("Cwd round-trip lost: %+v", out.Cwd)
	}
	if out.Snippet != nil {
		t.Errorf("Snippet should remain nil")
	}
}

func TestEventListRequest_OmitsZeroFields(t *testing.T) {
	t.Parallel()
	in := EventListRequest{}
	b, _ := json.Marshal(in)
	for _, field := range []string{"session_id", "since_seq", "limit"} {
		if strings.Contains(string(b), field) {
			t.Errorf("zero EventListRequest must omit %q, got %s", field, b)
		}
	}
}

func TestEventListResponse_EmptySliceEncodesAsArray(t *testing.T) {
	t.Parallel()
	// nil-vs-empty: an empty slice must encode as `[]` not `null`
	// so clients can iterate without nil-checking. Catches a
	// regression where a handler returns nil instead of []Event{}.
	in := EventListResponse{Events: []Event{}, LatestSeq: 0}
	b, _ := json.Marshal(in)
	if !strings.Contains(string(b), `"events":[]`) {
		t.Errorf("empty events must encode as []; got %s", b)
	}
}
