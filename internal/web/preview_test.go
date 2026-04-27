package web

import (
	"strings"
	"testing"
)

func TestPickRowPreview_SummaryTopicWins(t *testing.T) {
	t.Parallel()
	text, kind := pickRowPreview("Implementing OAuth refresh-token rotation",
		"some long substantive first prompt the user actually typed")
	if kind != "topic" {
		t.Errorf("kind: got %q, want topic", kind)
	}
	if text != "Implementing OAuth refresh-token rotation" {
		t.Errorf("topic should be picked over first_prompt; got %q", text)
	}
}

func TestPickRowPreview_FallsBackToSubstantiveFirstPrompt(t *testing.T) {
	t.Parallel()
	text, kind := pickRowPreview("", "Implementing OAuth refresh-token rotation in our auth layer")
	if kind != "prompt" {
		t.Errorf("kind: got %q, want prompt", kind)
	}
	if !strings.Contains(text, "OAuth") {
		t.Errorf("prompt should pass through; got %q", text)
	}
}

func TestPickRowPreview_FallsBackToMutedForFiller(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"  ",
		"yes",
		"go ahead",
		"do plan",
		"/loop",
		"/plan",
		"what's next",
	}
	for _, in := range cases {
		text, kind := pickRowPreview("", in)
		if kind != "muted" {
			t.Errorf("input %q: kind got %q, want muted", in, kind)
		}
		if !strings.Contains(text, "no summary yet") {
			t.Errorf("input %q: text got %q, want '(no summary yet)'", in, text)
		}
	}
}

func TestPickRowPreview_TrimsTopicWhitespace(t *testing.T) {
	t.Parallel()
	text, kind := pickRowPreview("   topic with surrounding space   ", "")
	if kind != "topic" {
		t.Errorf("kind: got %q, want topic", kind)
	}
	if text != "topic with surrounding space" {
		t.Errorf("topic should be trimmed; got %q", text)
	}
}

func TestIsSubstantivePrompt(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"":                                     false,
		"   ":                                  false,
		"yes":                                  false,
		"go ahead":                             false,
		"/loop":                                false,
		"/plan":                                false,
		"/plan with args do something here":    true, // slash + space → substantive (it's a real prompt)
		"this is exactly thirty runes!!!":      true, // 31 runes
		"two words":                            false,
		"implement the OAuth refresh rotation": true,
	}
	for in, want := range cases {
		if got := isSubstantivePrompt(in); got != want {
			t.Errorf("isSubstantivePrompt(%q) = %v, want %v", in, got, want)
		}
	}
}
