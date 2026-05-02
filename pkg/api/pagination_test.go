package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPageRequest_RoundTrip(t *testing.T) {
	t.Parallel()
	in := PageRequest{Limit: 100, Cursor: "abc"}
	b, _ := json.Marshal(in)
	var out PageRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch: %+v != %+v", out, in)
	}
}

func TestPageRequest_OmitsZeroFields(t *testing.T) {
	t.Parallel()
	in := PageRequest{}
	b, _ := json.Marshal(in)
	if strings.Contains(string(b), "limit") || strings.Contains(string(b), "cursor") {
		t.Errorf("expected zero PageRequest to encode as empty object, got %s", b)
	}
}

func TestPageResponse_OmitsEmptyNextCursor(t *testing.T) {
	t.Parallel()
	// next_cursor empty means "no more pages" - a client uses
	// presence/absence to decide whether to fetch again. Wrong-
	// sense regression (sending "" instead of omitting) breaks
	// that contract.
	in := PageResponse{}
	b, _ := json.Marshal(in)
	if strings.Contains(string(b), "next_cursor") {
		t.Errorf("expected next_cursor omitted, got %s", b)
	}
}

func TestPageResponse_RoundTripWithCursor(t *testing.T) {
	t.Parallel()
	in := PageResponse{NextCursor: "next-page"}
	b, _ := json.Marshal(in)
	var out PageResponse
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch: %+v != %+v", out, in)
	}
}

func TestCursor_IsString(t *testing.T) {
	t.Parallel()
	// Cursor must JSON-encode as a string so clients pass it
	// through opaque without unwrapping. Catches a regression
	// where a struct or []byte alias sneaks in.
	in := Cursor("hello")
	b, _ := json.Marshal(in)
	if string(b) != `"hello"` {
		t.Errorf("Cursor must encode as JSON string, got %s", b)
	}
}

func TestPageLimits_AreSane(t *testing.T) {
	t.Parallel()
	// Sanity guard against a copy-paste regression that flips
	// the constants.
	if DefaultPageLimit <= 0 {
		t.Errorf("DefaultPageLimit must be positive, got %d", DefaultPageLimit)
	}
	if MaxPageLimit < DefaultPageLimit {
		t.Errorf("MaxPageLimit (%d) must be >= DefaultPageLimit (%d)", MaxPageLimit, DefaultPageLimit)
	}
}
