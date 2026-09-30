package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/resumecmd"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/internal/wire"
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

// pickCancel is a fake picker that simulates the user cancelling.
func pickCancel(_ []resumeCandidate, _ io.Reader, _ io.Writer) (int, bool, error) {
	return 0, false, nil
}

// pickNever fails the test if the picker is reached — used on paths
// (print, non-TTY, no matches) that must never prompt.
func pickNever(t *testing.T) resumePicker {
	return func([]resumeCandidate, io.Reader, io.Writer) (int, bool, error) {
		t.Helper()
		t.Fatal("picker must not be called on this path")
		return 0, false, nil
	}
}

func TestRunResume_InteractiveLaunchesSelected(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	got, called, exec := recordExec()
	var out bytes.Buffer

	// Picker also asserts RunResume fetched the tail preview before
	// handing candidates off.
	pick := func(cands []resumeCandidate, _ io.Reader, _ io.Writer) (int, bool, error) {
		if len(cands) == 0 {
			t.Fatal("no candidates passed to picker")
		}
		if len(cands[0].tail) == 0 {
			t.Errorf("expected tail preview fetched for candidate 0")
		}
		return 0, true, nil
	}

	opts := ResumeOptions{Query: "jsonl", Interactive: true}
	if err := RunResume(t.Context(), apiForStore(t, s), opts, strings.NewReader(""), &out, pick, exec); err != nil {
		t.Fatalf("RunResume: %v", err)
	}
	if !*called {
		t.Fatalf("exec was not called; output:\n%s", out.String())
	}
	if want := "cd /work/foo && claude --resume sess-foo"; got.Shell() != want {
		t.Errorf("launched %q, want %q", got.Shell(), want)
	}
	if !strings.Contains(out.String(), "→ cd /work/foo && claude --resume sess-foo") {
		t.Errorf("missing launch echo:\n%s", out.String())
	}
}

func TestRunResume_CancelDoesNotLaunch(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	_, called, exec := recordExec()
	var out bytes.Buffer

	opts := ResumeOptions{Query: "jsonl", Interactive: true}
	if err := RunResume(t.Context(), apiForStore(t, s), opts, strings.NewReader(""), &out, pickCancel, exec); err != nil {
		t.Fatalf("RunResume: %v", err)
	}
	if *called {
		t.Error("exec should not run when the picker is cancelled")
	}
	if !strings.Contains(out.String(), "(cancelled)") {
		t.Errorf("expected (cancelled), got:\n%s", out.String())
	}
}

func TestRunResume_PrintDoesNotLaunch(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	_, called, exec := recordExec()
	var out bytes.Buffer

	// Interactive true, but --print wins: list commands, never prompt.
	opts := ResumeOptions{Query: "jsonl", Interactive: true, Print: true}
	if err := RunResume(t.Context(), apiForStore(t, s), opts, strings.NewReader(""), &out, pickNever(t), exec); err != nil {
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
	if err := RunResume(t.Context(), apiForStore(t, s), opts, strings.NewReader(""), &out, pickNever(t), exec); err != nil {
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
	if err := RunResume(t.Context(), apiForStore(t, s), opts, strings.NewReader(""), &out, pickNever(t), exec); err != nil {
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
	if err := RunResume(t.Context(), apiForStore(t, s), opts, strings.NewReader(""), &out, pickNever(t), exec); err != nil {
		t.Fatalf("RunResume: %v", err)
	}
	if *called {
		t.Errorf("exec must not run under a non-matching agent filter")
	}
	if !strings.Contains(out.String(), "no sessions matched") {
		t.Errorf("expected no-match message, got:\n%s", out.String())
	}
}

func TestResumeListWhen(t *testing.T) {
	t.Parallel()
	// Wednesday 2026-07-01 12:00 local as the reference "now".
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.Local)
	ms := func(tm time.Time) *int64 { v := tm.UnixMilli(); return &v }
	cases := []struct {
		name string
		d    wire.SessionDigest
		want string
	}{
		{
			name: "hours ago, ended same day",
			d:    wire.SessionDigest{EndedAtMs: ms(now.Add(-5 * time.Hour))},
			want: "wed 5h ago",
		},
		{
			name: "minutes ago under an hour",
			d:    wire.SessionDigest{EndedAtMs: ms(now.Add(-12 * time.Minute))},
			want: "wed 12m ago",
		},
		{
			name: "days back shows that weekday + hours",
			d:    wire.SessionDigest{EndedAtMs: ms(now.Add(-48 * time.Hour))}, // Mon
			want: "mon 48h ago",
		},
		{
			name: "falls back to started_at",
			d:    wire.SessionDigest{StartedAtMs: ms(now.Add(-2 * time.Hour))},
			want: "wed 2h ago",
		},
		{
			name: "no timestamp",
			d:    wire.SessionDigest{},
			want: "-",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resumeListWhen(tc.d, now); got != tc.want {
				t.Errorf("resumeListWhen = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResumeCmd_DefaultSinceIsSixWeeks(t *testing.T) {
	t.Parallel()
	cmd := newResumeCmd()
	f := cmd.Flags().Lookup("since")
	if f == nil {
		t.Fatal("--since flag not registered")
	}
	// 6 weeks = 1008h; pinned so the default can't silently regress to
	// "no limit" (0s) or some other window.
	if got, want := f.DefValue, "42d"; got != want {
		t.Errorf("--since default = %q, want %q (6 weeks)", got, want)
	}
}

func TestRunResume_EmptyQueryErrors(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	_, _, exec := recordExec()
	var out bytes.Buffer

	err := RunResume(t.Context(), apiForStore(t, s), ResumeOptions{Query: "  ", Interactive: true}, strings.NewReader(""), &out, pickNever(t), exec)
	if err == nil {
		t.Fatal("expected an error for an empty query")
	}
}

// ingestPRCreated records a Bash tool_use in sessionKey whose result
// carries Claude Code's gitOperation record for a newly created PR —
// the only source of pr_created extractions.
func ingestPRCreated(t *testing.T, s *store.Store, sessionKey, cwd, prURL string) {
	t.Helper()
	e := events.Envelope{
		V: 1, EventID: uuid.Must(uuid.NewV7()).String(),
		SourceAgent: "claude-code", SourceSessionID: sessionKey,
		Kind: "tool_use", TsSource: time.Now().UTC().Add(-46 * time.Hour),
		Cwd: cwd, ContentText: "Bash gh pr create --fill",
		Tool: &events.Tool{Name: "Bash"},
		Payload: map[string]any{
			"tool_input": map[string]any{"command": "gh pr create --fill"},
			"tool_response": map[string]any{
				"stdout": prURL,
				"gitOperation": map[string]any{
					"pr": map[string]any{"action": "created", "number": 7, "url": prURL},
				},
			},
		},
	}
	events.ApplyRedaction(&e, redact.Default())
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, _, err := store.IngestEnvelope(t.Context(), tx, &e, raw, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ingest: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestRunResume_PrintListsCreatedPRs(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	const prURL = "https://github.com/acme/widgets/pull/7"
	ingestPRCreated(t, s, "sess-foo", "/work/foo", prURL)
	var out bytes.Buffer

	opts := ResumeOptions{Query: "jsonl", Print: true}
	if err := RunResume(t.Context(), apiForStore(t, s), opts, strings.NewReader(""), &out, pickNever(t), nil); err != nil {
		t.Fatalf("RunResume: %v", err)
	}
	want := "[1] cd /work/foo && claude --resume sess-foo\n      PR " + prURL + "\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("output missing %q:\n%s", want, out.String())
	}
}

func TestRunResume_InteractivePassesCreatedPRsToPicker(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	const prURL = "https://github.com/acme/widgets/pull/7"
	ingestPRCreated(t, s, "sess-foo", "/work/foo", prURL)
	_, _, exec := recordExec()
	var out bytes.Buffer

	var got []string
	pick := func(cands []resumeCandidate, _ io.Reader, _ io.Writer) (int, bool, error) {
		got = cands[0].prs
		return 0, true, nil
	}
	opts := ResumeOptions{Query: "jsonl", Interactive: true}
	if err := RunResume(t.Context(), apiForStore(t, s), opts, strings.NewReader(""), &out, pick, exec); err != nil {
		t.Fatalf("RunResume: %v", err)
	}
	if want := []string{prURL}; !reflect.DeepEqual(got, want) {
		t.Errorf("candidate prs: got %v, want %v", got, want)
	}
}

func TestRunResume_NoCreatedPRsPrintsNoPRLines(t *testing.T) {
	t.Parallel()
	s, _ := seedStore(t)
	var out bytes.Buffer

	opts := ResumeOptions{Query: "jsonl", Print: true}
	if err := RunResume(t.Context(), apiForStore(t, s), opts, strings.NewReader(""), &out, pickNever(t), nil); err != nil {
		t.Fatalf("RunResume: %v", err)
	}
	if strings.Contains(out.String(), "PR ") {
		t.Errorf("unexpected PR line without pr_created rows:\n%s", out.String())
	}
}
