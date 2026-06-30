package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/resumecmd"
)

// recordExec returns a resumeExecFn that captures the spec it's handed
// (instead of replacing the process) plus a pointer to inspect after.
func recordExec() (*resumecmd.Spec, *bool, resumeExecFn) {
	var got resumecmd.Spec
	called := false
	fn := func(s resumecmd.Spec) error {
		got = s
		called = true
		return nil
	}
	return &got, &called, fn
}

func TestRunResume_InteractiveLaunchesSelected(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	got, called, exec := recordExec()
	var out bytes.Buffer

	// "jsonl" matches only sess-foo (cwd /work/foo). Pick #1.
	opts := ResumeOptions{Query: "jsonl", Interactive: true}
	if err := RunResume(t.Context(), apiForStore(t, s), opts, strings.NewReader("1\n"), &out, exec); err != nil {
		t.Fatalf("RunResume: %v", err)
	}
	if !*called {
		t.Fatalf("exec was not called; output:\n%s", out.String())
	}
	if want := "cd /work/foo && claude --resume sess-foo"; got.Shell() != want {
		t.Errorf("launched %q, want %q", got.Shell(), want)
	}
	// The numbered table and the chosen command echo should be visible.
	for _, want := range []string{"FIRST_PROMPT", "what is jsonl format", "→ cd /work/foo && claude --resume sess-foo"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunResume_BlankAndQCancel(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"\n", "q\n", "Q\n", ""} {
		s, _ := seedStore(t)
		_, called, exec := recordExec()
		var out bytes.Buffer
		opts := ResumeOptions{Query: "jsonl", Interactive: true}
		if err := RunResume(t.Context(), apiForStore(t, s), opts, strings.NewReader(in), &out, exec); err != nil {
			t.Fatalf("RunResume(in=%q): %v", in, err)
		}
		if *called {
			t.Errorf("in=%q: exec should not run on cancel", in)
		}
		if !strings.Contains(out.String(), "(cancelled)") {
			t.Errorf("in=%q: expected (cancelled), got:\n%s", in, out.String())
		}
	}
}

func TestRunResume_ReprompsOnInvalidChoice(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	_, called, exec := recordExec()
	var out bytes.Buffer

	opts := ResumeOptions{Query: "jsonl", Interactive: true}
	// "x" is not a number, "9" is out of range, "1" is valid.
	if err := RunResume(t.Context(), apiForStore(t, s), opts, strings.NewReader("x\n9\n1\n"), &out, exec); err != nil {
		t.Fatalf("RunResume: %v", err)
	}
	if !*called {
		t.Fatalf("exec was not called after valid retry:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "sess-foo") {
		t.Errorf("expected the chosen session to launch:\n%s", out.String())
	}
	if strings.Count(out.String(), "not a valid choice") != 2 {
		t.Errorf("expected two invalid-choice notices, got:\n%s", out.String())
	}
}

func TestRunResume_PrintDoesNotLaunch(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	_, called, exec := recordExec()
	var out bytes.Buffer

	// Interactive true, but --print wins: list commands, never exec.
	opts := ResumeOptions{Query: "jsonl", Interactive: true, Print: true}
	if err := RunResume(t.Context(), apiForStore(t, s), opts, strings.NewReader("1\n"), &out, exec); err != nil {
		t.Fatalf("RunResume: %v", err)
	}
	if *called {
		t.Errorf("exec must not run under --print")
	}
	if want := "[1] cd /work/foo && claude --resume sess-foo"; !strings.Contains(out.String(), want) {
		t.Errorf("output missing %q:\n%s", want, out.String())
	}
}

func TestRunResume_NonInteractiveFallsBackToPrint(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	_, called, exec := recordExec()
	var out bytes.Buffer

	opts := ResumeOptions{Query: "jsonl", Interactive: false}
	if err := RunResume(t.Context(), apiForStore(t, s), opts, strings.NewReader("1\n"), &out, exec); err != nil {
		t.Fatalf("RunResume: %v", err)
	}
	if *called {
		t.Errorf("exec must not run when stdin is not a terminal")
	}
	for _, want := range []string{"stdin is not a terminal", "[1] cd /work/foo && claude --resume sess-foo"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunResume_NoMatch(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	_, called, exec := recordExec()
	var out bytes.Buffer

	opts := ResumeOptions{Query: "nonexistentterm", Interactive: true}
	if err := RunResume(t.Context(), apiForStore(t, s), opts, strings.NewReader("1\n"), &out, exec); err != nil {
		t.Fatalf("RunResume: %v", err)
	}
	if *called {
		t.Errorf("exec must not run when nothing matched")
	}
	if !strings.Contains(out.String(), "no sessions matched") {
		t.Errorf("expected no-match message, got:\n%s", out.String())
	}
}

func TestRunResume_AgentFilterExcludesAll(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	_, called, exec := recordExec()
	var out bytes.Buffer

	// Seed data is all claude-code; filtering to gemini-cli matches none.
	opts := ResumeOptions{Query: "jsonl", Agent: "gemini-cli", Interactive: true}
	if err := RunResume(t.Context(), apiForStore(t, s), opts, strings.NewReader("1\n"), &out, exec); err != nil {
		t.Fatalf("RunResume: %v", err)
	}
	if *called {
		t.Errorf("exec must not run under a non-matching agent filter")
	}
	if !strings.Contains(out.String(), "no sessions matched") {
		t.Errorf("expected no-match message, got:\n%s", out.String())
	}
}

func TestRunResume_EmptyQueryErrors(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	_, _, exec := recordExec()
	var out bytes.Buffer

	err := RunResume(t.Context(), apiForStore(t, s), ResumeOptions{Query: "  ", Interactive: true}, strings.NewReader(""), &out, exec)
	if err == nil {
		t.Fatal("expected an error for an empty query")
	}
}
