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
