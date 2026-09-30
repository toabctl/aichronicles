package wire

import (
	"encoding/base64"
	"testing"
)

func TestSearchCursor_RoundTrip(t *testing.T) {
	t.Parallel()
	in := SearchCursor{Off: 150, Stage: "trigram", Now: 1_700_000_000_000, Ord: 1, Dedup: true}
	enc, err := EncodeSearchCursor(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if enc == "" {
		t.Fatal("encoded cursor is empty")
	}
	got, err := DecodeSearchCursor(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != in {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, in)
	}
}

func TestSearchCursor_ZeroValueRoundTrips(t *testing.T) {
	t.Parallel()
	enc, err := EncodeSearchCursor(SearchCursor{})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeSearchCursor(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != (SearchCursor{}) {
		t.Errorf("zero round-trip: got %+v, want zero", got)
	}
}

func TestPageCursor_RoundTrip(t *testing.T) {
	t.Parallel()
	in := PageCursor{Off: 250}
	enc, err := EncodePageCursor(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if enc == "" {
		t.Fatal("encoded cursor is empty")
	}
	got, err := DecodePageCursor(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != in {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, in)
	}
}

func TestDecodePageCursor_Malformed(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]Cursor{
		"not base64":   Cursor("!!! not base64 !!!"),
		"empty string": Cursor(""),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodePageCursor(c); err == nil {
				t.Errorf("expected error decoding %q", c)
			}
		})
	}
}

func TestDecodeSearchCursor_Malformed(t *testing.T) {
	t.Parallel()
	// Well-formed base64url whose decoded bytes are not valid JSON.
	badJSON := Cursor(base64.RawURLEncoding.EncodeToString([]byte("this is not json")))
	cases := map[string]Cursor{
		"not base64":   Cursor("!!! not base64 !!!"),
		"bad json":     badJSON,
		"empty string": Cursor(""), // decodes to empty bytes → invalid json
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeSearchCursor(c); err == nil {
				t.Errorf("expected error decoding %q", c)
			}
		})
	}
}

func TestSearchRequest_QueryFingerprint(t *testing.T) {
	t.Parallel()
	base := SearchRequest{Q: "fox", Kind: "user_prompt", SinceMs: 10}
	same := SearchRequest{Q: "fox", Kind: "user_prompt", SinceMs: 10}
	if same.QueryFingerprint() != base.QueryFingerprint() {
		t.Fatal("equal requests must share a fingerprint")
	}
	paging := base
	paging.Limit, paging.Cursor, paging.NoDedup = 99, "abc", true
	if paging.QueryFingerprint() != base.QueryFingerprint() {
		t.Error("paging controls must not change the fingerprint")
	}
	for name, mut := range map[string]func(*SearchRequest){
		"q":             func(r *SearchRequest) { r.Q = "dog" },
		"kind":          func(r *SearchRequest) { r.Kind = "" },
		"session":       func(r *SearchRequest) { r.SessionID = "s" },
		"subagent":      func(r *SearchRequest) { r.SubagentID = "a" },
		"source agent":  func(r *SearchRequest) { r.SourceAgent = "x" },
		"tool":          func(r *SearchRequest) { r.ToolName = "Bash" },
		"skill":         func(r *SearchRequest) { r.SkillName = "k" },
		"file path":     func(r *SearchRequest) { r.FilePathSubstring = "f" },
		"since":         func(r *SearchRequest) { r.SinceMs = 11 },
		"with failures": func(r *SearchRequest) { r.WithFailures = true },
		// Length-prefixing keeps field boundaries: moving a byte from
		// one field to its neighbour must change the hash.
		"field boundary": func(r *SearchRequest) { r.Q, r.Kind = "foxu", "ser_prompt" },
	} {
		r := base
		mut(&r)
		if r.QueryFingerprint() == base.QueryFingerprint() {
			t.Errorf("changing %s did not change the fingerprint", name)
		}
	}
}

func TestLLMOutputKind_Known(t *testing.T) {
	t.Parallel()
	for _, k := range []LLMOutputKind{
		LLMKindSummary, LLMKindReflect, LLMKindPropose, LLMKindReflectWeekly,
		LLMKindProposeVerify, LLMKindSkillRevision, LLMKindInduction,
		LLMKindChallenge, LLMKindFacts, LLMKindSkillMerge,
	} {
		if !k.Known() {
			t.Errorf("%q should be known", k)
		}
	}
	for _, k := range []LLMOutputKind{"", "sumary", "SUMMARY"} {
		if k.Known() {
			t.Errorf("%q should not be known", k)
		}
	}
}
