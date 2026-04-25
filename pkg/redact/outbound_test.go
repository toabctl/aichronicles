package redact

import (
	"reflect"
	"strings"
	"testing"
)

func TestOutbound_CleanStringPassesThrough(t *testing.T) {
	t.Parallel()
	s, p := Outbound("please summarize the last five prompts")
	if s != "please summarize the last five prompts" {
		t.Errorf("clean string mutated: %q", s)
	}
	if len(p) != 0 {
		t.Errorf("expected no patterns, got %v", p)
	}
}

func TestOutbound_ScrubsAndReturnsPatterns(t *testing.T) {
	t.Parallel()
	in := "here is the key sk-ant-" + strings.Repeat("a", 40) + " end"
	out, p := Outbound(in)
	if strings.Contains(out, "sk-ant-") {
		t.Errorf("secret not scrubbed: %q", out)
	}
	if !strings.Contains(out, "<redacted:anthropic_api_key>") {
		t.Errorf("marker missing: %q", out)
	}
	if !reflect.DeepEqual(p, []string{"anthropic_api_key"}) {
		t.Errorf("patterns: %v", p)
	}
}

func TestOutbound_MultiplePatternsAllReported(t *testing.T) {
	t.Parallel()
	in := "aws AKIAIOSFODNN7EXAMPLE and google AIzaSyA-abcdefghijklmnopqrstuvwxyz12345"
	_, p := Outbound(in)
	want := []string{"aws_access_key", "google_api_key"}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("patterns: got %v, want %v", p, want)
	}
}

func TestMustClean_CleanReturnsStringAndNilPatterns(t *testing.T) {
	t.Parallel()
	s, p := MustClean("just a benign prompt")
	if s != "just a benign prompt" {
		t.Errorf("clean string mutated: %q", s)
	}
	if p != nil {
		t.Errorf("expected nil patterns, got %v", p)
	}
}

func TestMustClean_AnyFindingReturnsEmptyStringAndPatterns(t *testing.T) {
	t.Parallel()
	in := "something AKIAIOSFODNN7EXAMPLE"
	s, p := MustClean(in)
	if s != "" {
		t.Errorf("MustClean must return empty string when patterns fired, got %q", s)
	}
	if len(p) == 0 {
		t.Error("MustClean must report which patterns triggered the abort")
	}
}
