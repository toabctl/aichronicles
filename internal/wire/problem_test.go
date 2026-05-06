package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProblem_RoundTrip(t *testing.T) {
	t.Parallel()
	in := Problem{Title: "Bad Request", Status: 400, Detail: "missing field foo"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Problem
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch: %+v != %+v", out, in)
	}
}

func TestProblem_OmitsEmptyDetail(t *testing.T) {
	t.Parallel()
	// Detail uses omitempty so a server returning Status-only
	// problems doesn't ship `"detail":""` noise. Catches a
	// regression where the json tag drops omitempty.
	in := Problem{Title: "Internal Server Error", Status: 500}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"detail"`) {
		t.Errorf("expected detail omitted, got %s", b)
	}
}

func TestProblem_RejectsUnknownFieldsOnDecode(t *testing.T) {
	t.Parallel()
	// Disallow-unknown-fields decoding catches schema drift between
	// server and client. Verify the type is well-formed for that
	// stricter consumption.
	body := []byte(`{"title":"x","status":400,"surprise":1}`)
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	var p Problem
	if err := dec.Decode(&p); err == nil {
		t.Errorf("expected DisallowUnknownFields to reject unknown field")
	}
}
