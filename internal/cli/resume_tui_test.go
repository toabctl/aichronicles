package cli

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/resumecmd"
	"github.com/toabctl/aichronicles/internal/wire"
)

// tuiCand builds a candidate from plain values so the model tests need
// no store or API.
func tuiCand(id, cwd, prompt string, tail []wire.SessionEvent) resumeCandidate {
	cwdCopy, promptCopy := cwd, prompt
	return resumeCandidate{
		digest: wire.SessionDigest{ID: id, Cwd: &cwdCopy, FirstPrompt: &promptCopy, SourceAgent: "claude-code"},
		spec:   resumecmd.Spec{Bin: "claude", Args: []string{"--resume", id}},
		tail:   tail,
	}
}

func msg(kind, text string) wire.SessionEvent {
	t := text
	return wire.SessionEvent{Kind: kind, ContentText: &t}
}

func sendKey(t *testing.T, m resumeModel, k tea.KeyMsg) resumeModel {
	t.Helper()
	next, _ := m.Update(k)
	rm, ok := next.(resumeModel)
	if !ok {
		t.Fatalf("Update returned %T, want resumeModel", next)
	}
	return rm
}

func threeCands() []resumeCandidate {
	return []resumeCandidate{
		tuiCand("aaaaaaaa-1", "/work/a", "prompt a", nil),
		tuiCand("bbbbbbbb-2", "/work/b", "prompt b", nil),
		tuiCand("cccccccc-3", "/work/c", "prompt c", nil),
	}
}

func TestResumeModel_NavigationClamps(t *testing.T) {
	t.Parallel()
	m := newResumeModel(threeCands())
	if m.cursor != 0 {
		t.Fatalf("initial cursor = %d, want 0", m.cursor)
	}
	// Up at the top is a no-op.
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.cursor != 0 {
		t.Errorf("up at top: cursor = %d, want 0", m.cursor)
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 2 {
		t.Errorf("after two downs: cursor = %d, want 2", m.cursor)
	}
	// Down at the bottom is a no-op (clamped).
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.cursor != 2 {
		t.Errorf("down at bottom: cursor = %d, want 2", m.cursor)
	}
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.cursor != 1 {
		t.Errorf("after up: cursor = %d, want 1", m.cursor)
	}
}

func TestResumeModel_EnterSelectsCursor(t *testing.T) {
	t.Parallel()
	m := newResumeModel(threeCands())
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown}) // cursor 1
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.quitting {
		t.Error("enter should set quitting")
	}
	if m.chosen != 1 {
		t.Errorf("chosen = %d, want 1", m.chosen)
	}
}

func TestResumeModel_CancelKeys(t *testing.T) {
	t.Parallel()
	for _, k := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyEsc},
		{Type: tea.KeyCtrlC},
	} {
		m := newResumeModel(threeCands())
		m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyDown}) // move off 0 first
		m = sendKey(t, m, k)
		if !m.quitting {
			t.Errorf("%v: should set quitting", k)
		}
		if m.chosen != -1 {
			t.Errorf("%v: chosen = %d, want -1 (cancelled)", k, m.chosen)
		}
	}
}

func TestResumeModel_ViewShowsListAndPreview(t *testing.T) {
	t.Parallel()
	cands := []resumeCandidate{
		tuiCand("aaaaaaaa-1", "/work/alpha", "open alpha", []wire.SessionEvent{
			msg(events.KindUserPrompt, "fix the bug"),
			msg(events.KindAssistantMessage, "done, shipped a fix"),
		}),
		tuiCand("bbbbbbbb-2", "/work/beta", "open beta", nil),
	}
	m := newResumeModel(cands)
	// Simulate a real terminal size so the layout is exercised.
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(resumeModel)
	view := m.View()

	for _, want := range []string{
		"/work/alpha", // list row + preview header for selected
		"/work/beta",  // second list row
		"open alpha",  // selected session's opening prompt
		"fix the bug", // selected session's tail
		"done, shipped a fix",
		"you",          // speaker label for the user turn
		"claude",       // speaker label for the assistant turn (claude-code)
		"enter resume", // footer hint
	} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}

	// The non-selected session's preview content must not show.
	if strings.Contains(view, "open beta") {
		t.Errorf("view leaked the non-selected session's preview:\n%s", view)
	}
}

func TestWrapWords(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		s        string
		width    int
		maxLines int
		want     []string
	}{
		{"empty", "", 10, 3, nil},
		{"fits one line", "hello world", 20, 3, []string{"hello world"}},
		{"wraps on spaces", "alpha beta gamma delta", 11, 3, []string{"alpha beta", "gamma delta"}},
		{"caps lines with ellipsis", "one two three four five six", 7, 2, []string{"one two", "three…"}},
		{"hard-splits long word", "supercalifragilistic", 6, 3, []string{"superc", "alifra", "gilis…"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := wrapWords(tc.s, tc.width, tc.maxLines)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("wrapWords(%q,%d,%d) = %q, want %q", tc.s, tc.width, tc.maxLines, got, tc.want)
			}
			for i, ln := range got {
				if n := len([]rune(ln)); n > tc.width {
					t.Errorf("line %d %q has %d runes, exceeds width %d", i, ln, n, tc.width)
				}
			}
		})
	}
}

func TestResumeModel_QuittingViewIsEmpty(t *testing.T) {
	t.Parallel()
	m := newResumeModel(threeCands())
	m = sendKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.View(); got != "" {
		t.Errorf("view after quit = %q, want empty", got)
	}
}
