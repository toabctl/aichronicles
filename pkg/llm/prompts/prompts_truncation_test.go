package prompts

import (
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/store"
)

// TestRenderEvents_PerKindCaps confirms the per-kind cap actually
// trims the bulky kinds (tool_result above all) while leaving
// high-signal kinds (user_prompt, assistant_message) effectively
// untouched at typical sizes. Without the per-kind cap a single
// tool_result returning a 1MB file dump produces a 1M+ token
// prompt that bounces off the API as 400 prompt-too-long.
func TestRenderEvents_PerKindCaps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind         string
		bodyRunes    int
		expectTrunc  bool
		expectedHead int // exact size after truncation when truncated
	}{
		// Way over cap → truncated to the per-kind ceiling.
		{"tool_result", 50000, true, maxRunesToolResult},
		{"tool_use", 50000, true, maxRunesToolUse},
		{"tool_failure", 50000, true, maxRunesToolFailure},
		{"user_prompt", 50000, true, maxRunesUserPrompt},
		{"assistant_message", 50000, true, maxRunesAssistantMessage},
		// Under their kind's cap → passes through verbatim.
		{"tool_result", 200, false, 200},
		{"user_prompt", 1000, false, 1000},
		// Unknown kind → default cap.
		{"weird_new_kind", 50000, true, maxRunesDefault},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			body := strings.Repeat("x", tc.bodyRunes)
			events := []store.EventView{
				{EventID: "e1", Kind: tc.kind, ContentText: nullS(body), TsSourceMs: 1},
			}
			out := renderEvents(events, patternSet{})
			if tc.expectTrunc {
				if !strings.Contains(out, "body truncated") {
					t.Errorf("%s @ %d runes: expected truncation marker; got %d output runes",
						tc.kind, tc.bodyRunes, len([]rune(out)))
				}
				// Body should be capped at expectedHead — count
				// the consecutive 'x' runs to validate.
				runs := strings.Count(out, "x")
				if runs > tc.expectedHead+50 { // small slack for formatting
					t.Errorf("%s: kept %d 'x'es, expected ≤ %d", tc.kind, runs, tc.expectedHead+50)
				}
			} else {
				if strings.Contains(out, "body truncated") {
					t.Errorf("%s @ %d runes: should NOT have triggered truncation:\n%s",
						tc.kind, tc.bodyRunes, out)
				}
			}
		})
	}
}

// TestRenderEvents_HugeToolResultDoesNotBlowBudget pins the bug
// that triggered this work: a single `Read` tool_result returning
// a megabyte of file content used to land in the prompt verbatim,
// producing 1M+ token user messages that the API rejected. With
// per-kind caps in place, the rendered transcript stays tiny.
func TestRenderEvents_HugeToolResultDoesNotBlowBudget(t *testing.T) {
	t.Parallel()
	events := []store.EventView{
		{EventID: "e1", Kind: "user_prompt", Role: nullS("user"),
			ContentText: nullS("read the file"), TsSourceMs: 1},
		{EventID: "e2", Kind: "tool_use", ToolName: nullS("Read"),
			ContentText: nullS(`{"file_path":"/big.log"}`), TsSourceMs: 2},
		{EventID: "e3", Kind: "tool_result", ToolName: nullS("Read"),
			ContentText: nullS(strings.Repeat("L", 2_000_000)), TsSourceMs: 3},
		{EventID: "e4", Kind: "assistant_message", Role: nullS("assistant"),
			ContentText: nullS("found the error on line 12"), TsSourceMs: 4},
	}
	out := renderEvents(events, patternSet{})
	runes := len([]rune(out))
	// Total should be roughly maxRunesToolResult plus the small
	// other events plus headers. Anything within 2× of that is
	// fine; what matters is that it's not 2M.
	if runes > 4*maxRunesToolResult {
		t.Errorf("rendered transcript is %d runes — expected ≤ ~%d after the tool_result cap fired",
			runes, 4*maxRunesToolResult)
	}
	// The high-signal events MUST survive verbatim.
	for _, want := range []string{
		"read the file",
		"found the error on line 12",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("high-signal content missing after capping: %q\n%s", want, out)
		}
	}
	// The truncation marker must be present so the model knows
	// the tool_result was elided.
	if !strings.Contains(out, "tool_result body truncated") {
		t.Errorf("expected truncation marker for tool_result:\n%s", out)
	}
}

// TestRenderEvents_NoTotalCap pins the deliberate non-decision:
// if the per-kind-capped events still sum past the API context
// window, renderEvents emits the full transcript anyway and the
// caller surfaces the API's "prompt too long" 400. We tested
// silent middle-elision and decided it risked dropping decisions
// the user cared about; an explicit failure is the better default
// until we have a chunked-summarisation story.
func TestRenderEvents_NoTotalCap(t *testing.T) {
	t.Parallel()
	// 100 full-cap user_prompt events ≈ 400k runes — well past any
	// "reasonable" total cap we might re-introduce. Confirm the
	// renderer still emits all of them with no truncation marker
	// (per-kind caps don't fire because each event is at the cap,
	// not over it).
	const n = 100
	events := make([]store.EventView, 0, n)
	for i := 0; i < n; i++ {
		body := strings.Repeat("x", maxRunesUserPrompt)
		events = append(events, store.EventView{
			EventID: "e", Kind: "user_prompt", Role: nullS("user"),
			ContentText: nullS(body), TsSourceMs: int64(i),
		})
	}
	out := renderEvents(events, patternSet{})
	if strings.Contains(out, "transcript") && strings.Contains(out, "truncated") {
		t.Errorf("no transcript-level truncation should fire — caller surfaces 400 instead:\n(first 200 runes)\n%s",
			string([]rune(out)[:200]))
	}
	// Output should contain at least n × maxRunesUserPrompt runes.
	if r := len([]rune(out)); r < n*maxRunesUserPrompt {
		t.Errorf("expected ≥ %d runes (no total cap), got %d", n*maxRunesUserPrompt, r)
	}
}

func TestTruncateTextRunes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in    string
		n     int
		want  string
		trunc bool
	}{
		{"hello", 10, "hello", false},
		{"hello", 5, "hello", false},
		{"hello", 4, "hell", true},
		{"", 10, "", false},
		{"abc", 0, "", true},
		{"αβγδ", 2, "αβ", true}, // multibyte UTF-8 stays intact
	}
	for _, tc := range cases {
		got, trunc := truncateTextRunes(tc.in, tc.n)
		if got != tc.want || trunc != tc.trunc {
			t.Errorf("truncateTextRunes(%q, %d) = (%q, %v), want (%q, %v)",
				tc.in, tc.n, got, trunc, tc.want, tc.trunc)
		}
	}
}

// TestTruncateMiddleRunes covers the head/tail-preserving path. The
// invariant is that both the first and last runes of the input
// survive whenever truncation fires; that's the property head-only
// truncation lacks for tool output where the exit code lives at the
// tail.
func TestTruncateMiddleRunes(t *testing.T) {
	t.Parallel()
	t.Run("under cap passes through", func(t *testing.T) {
		got, trunc := truncateMiddleRunes("hello", 10)
		if got != "hello" || trunc {
			t.Errorf("got (%q, %v), want (%q, false)", got, trunc, "hello")
		}
	})
	t.Run("zero cap returns empty truncated", func(t *testing.T) {
		got, trunc := truncateMiddleRunes("hello", 0)
		if got != "" || !trunc {
			t.Errorf("got (%q, %v), want (\"\", true)", got, trunc)
		}
	})
	t.Run("preserves head and tail", func(t *testing.T) {
		// 1000 runes: "AAA…BBB…CCC". The first 'A' and last 'C'
		// must both appear in the output; middle 'B' must not.
		head := strings.Repeat("A", 100)
		mid := strings.Repeat("B", 800)
		tail := strings.Repeat("C", 100)
		input := head + mid + tail
		got, trunc := truncateMiddleRunes(input, 80)
		if !trunc {
			t.Fatalf("expected truncation, got %q", got)
		}
		if !strings.HasPrefix(got, "A") {
			t.Errorf("head 'A' missing: %q", got)
		}
		if !strings.HasSuffix(got, "C") {
			t.Errorf("tail 'C' missing: %q", got)
		}
		if strings.Contains(got, "B") {
			t.Errorf("middle 'B' should be elided, got %q", got)
		}
		// Output length stays within ±20% of the requested cap;
		// the elision marker adds a few runes but the budget is
		// 80 so we should be in [60, 100].
		runes := len([]rune(got))
		if runes < 60 || runes > 100 {
			t.Errorf("output length %d out of expected range [60,100]", runes)
		}
	})
	t.Run("multibyte stays intact", func(t *testing.T) {
		// Mix multibyte runes head and tail to check splitting.
		input := "αβγδεζηθικλμνξοπρστυφχψω"
		got, trunc := truncateMiddleRunes(input, 10)
		if !trunc {
			t.Fatalf("expected truncation, got %q", got)
		}
		// Must still parse as valid UTF-8 (Go strings always do,
		// but assert we didn't panic on the rune slice indexing).
		if len(got) == 0 {
			t.Errorf("non-empty truncation expected, got empty")
		}
	})
	t.Run("tiny cap falls back to head-only", func(t *testing.T) {
		// With n=4 the marker ("\n…\n", 3 runes) plus head/tail
		// can't fit; we expect head-truncate behaviour.
		got, trunc := truncateMiddleRunes("abcdefghij", 4)
		if !trunc {
			t.Fatalf("expected truncation")
		}
		if got != "abcd" {
			t.Errorf("tiny-cap fallback: got %q, want %q", got, "abcd")
		}
	})
}

// TestTruncateForKind ensures tool_result and tool_failure go
// through middle-elision while everything else stays head-only.
// Without this routing the per-kind decision drifts whenever a
// new kind is added to capForKind.
func TestTruncateForKind(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("A", 50)
	tail := strings.Repeat("Z", 50)
	body := head + strings.Repeat("M", 400) + tail
	cases := []struct {
		kind         string
		expectMiddle bool
	}{
		{"tool_result", true},
		{"tool_failure", true},
		{"user_prompt", false},
		{"assistant_message", false},
		{"tool_use", false},
		{"weird_unknown_kind", false},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			got, trunc := truncateForKind(tc.kind, body, 80)
			if !trunc {
				t.Fatalf("expected truncation for %s", tc.kind)
			}
			hasTail := strings.HasSuffix(got, "Z")
			if tc.expectMiddle && !hasTail {
				t.Errorf("%s: middle-elision expected, tail 'Z' missing: %q", tc.kind, got)
			}
			if !tc.expectMiddle && hasTail {
				t.Errorf("%s: head-only expected, tail 'Z' should be cut: %q", tc.kind, got)
			}
		})
	}
}
