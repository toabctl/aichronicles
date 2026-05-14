package claude

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/events"
)

const userEntryFixture = `{"type":"user","uuid":"01970000-0000-7000-8000-000000000001","sessionId":"s1","timestamp":"2026-05-02T12:00:00.000Z","cwd":"/p","version":"1.2.3","message":{"role":"user","content":"hello"}}`

const assistantEntryFixture = `{"type":"assistant","uuid":"01970000-0000-7000-8000-000000000002","sessionId":"s1","timestamp":"2026-05-02T12:00:01.000Z","cwd":"/p","version":"1.2.3","message":{"role":"assistant","content":[{"type":"text","text":"hi back"}]}}`

const internalEntryFixture = `{"type":"file-history-snapshot","sessionId":"s1","timestamp":"2026-05-02T12:00:02.000Z"}`

const malformedFixture = `{this is not json`

const missingUUIDFixture = `{"type":"user","sessionId":"s1","timestamp":"2026-05-02T12:00:03.000Z","message":{"role":"user","content":"oops"}}`

func writeJSONL(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func collect(t *testing.T, src *JSONLSource) []events.Event {
	t.Helper()
	var got []events.Event
	for evt, err := range src.Events(context.Background()) {
		if err != nil {
			t.Fatalf("source error: %v", err)
		}
		got = append(got, evt)
	}
	return got
}

func TestJSONLSource_YieldsCanonicalEntries(t *testing.T) {
	t.Parallel()
	path := writeJSONL(t, userEntryFixture, assistantEntryFixture)
	src := &JSONLSource{Root: path}

	got := collect(t, src)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Envelope.Kind != events.KindUserPrompt {
		t.Errorf("first event Kind: got %q, want %q", got[0].Envelope.Kind, events.KindUserPrompt)
	}
	if got[0].Envelope.ContentText != "hello" {
		t.Errorf("first event ContentText: got %q", got[0].Envelope.ContentText)
	}
	if got[1].Envelope.Kind != events.KindAssistantMessage {
		t.Errorf("second event Kind: got %q", got[1].Envelope.Kind)
	}
	if got[1].Envelope.ContentText != "hi back" {
		t.Errorf("second event ContentText: got %q", got[1].Envelope.ContentText)
	}
	if src.Stats.LinesRead != 2 || src.Stats.FilesRead != 1 {
		t.Errorf("stats: got %+v", src.Stats)
	}
}

func TestJSONLSource_SkipsInternalEntryTypes(t *testing.T) {
	t.Parallel()
	path := writeJSONL(t, userEntryFixture, internalEntryFixture, assistantEntryFixture)
	src := &JSONLSource{Root: path}

	got := collect(t, src)
	if len(got) != 2 {
		t.Errorf("got %d events, want 2 (internal entry should be silently skipped)", len(got))
	}
	if src.Stats.Invalid != 0 {
		t.Errorf("internal entry should not count as Invalid: got %d", src.Stats.Invalid)
	}
}

func TestJSONLSource_CountsMalformedAsInvalid(t *testing.T) {
	t.Parallel()
	path := writeJSONL(t, userEntryFixture, malformedFixture, assistantEntryFixture)
	src := &JSONLSource{Root: path}

	got := collect(t, src)
	if len(got) != 2 {
		t.Errorf("got %d events, want 2 (malformed line should be skipped)", len(got))
	}
	if src.Stats.Invalid != 1 {
		t.Errorf("Invalid count: got %d, want 1", src.Stats.Invalid)
	}
}

func TestJSONLSource_CountsMissingUUID(t *testing.T) {
	t.Parallel()
	path := writeJSONL(t, missingUUIDFixture)
	src := &JSONLSource{Root: path}

	got := collect(t, src)
	if len(got) != 0 {
		t.Errorf("got %d events, want 0", len(got))
	}
	if src.Stats.SkippedMissingUUID != 1 {
		t.Errorf("SkippedMissingUUID: got %d, want 1", src.Stats.SkippedMissingUUID)
	}
}

func TestJSONLSource_RootDirectoryWalksJSONLFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"a.jsonl", "b.jsonl", "ignored.txt"} {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(userEntryFixture + "\n"); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	src := &JSONLSource{Root: dir}

	got := collect(t, src)
	if len(got) != 2 {
		t.Errorf("got %d events, want 2 (one per .jsonl)", len(got))
	}
	if src.Stats.FilesRead != 2 {
		t.Errorf("FilesRead: got %d, want 2", src.Stats.FilesRead)
	}
}

func TestJSONLSource_EmptyRootYieldsNothing(t *testing.T) {
	t.Parallel()
	src := &JSONLSource{}
	got := collect(t, src)
	if len(got) != 0 {
		t.Errorf("empty root: got %d events, want 0", len(got))
	}
}

// TestJSONLSource_OversizeLineIsSkippedNotBuffered verifies that a
// line exceeding the per-source bound is counted as Invalid and
// skipped without being fully buffered in memory — the bound runs
// during read, not post-hoc. With the old behavior, `br.ReadBytes('\n')`
// would have allocated the entire oversize line first and then
// dropped it, OOM-able on a pathological input. The shrunk bound +
// surrounding-valid-lines test exercises both the discard-and-resume
// path and that good lines on either side of the bad one survive.
func TestJSONLSource_OversizeLineIsSkippedNotBuffered(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// First valid line, then 4 KiB of garbage with no newline + a
	// trailing newline (the "huge line"), then second valid line.
	if _, err := f.WriteString(userEntryFixture + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Repeat("x", 4096) + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(assistantEntryFixture + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	// Cap below the garbage line size but above the real fixtures
	// (each fixture is ~200 bytes).
	src := &JSONLSource{Root: path, maxLineBytes: 1024}
	got := collect(t, src)

	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (oversize line must be skipped, good lines preserved)", len(got))
	}
	if src.Stats.Invalid != 1 {
		t.Errorf("Invalid: got %d, want 1", src.Stats.Invalid)
	}
	if src.Stats.LinesRead != 3 {
		t.Errorf("LinesRead: got %d, want 3 (oversize line counts toward lines read)", src.Stats.LinesRead)
	}
}

func TestReadBoundedLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		input     string
		max       int
		wantLines []string
		wantOver  []bool
	}{
		{
			name:      "single short line",
			input:     "abc\n",
			max:       16,
			wantLines: []string{"abc\n"},
			wantOver:  []bool{false},
		},
		{
			name:      "exactly at bound",
			input:     "abcd\n",
			max:       4,
			wantLines: []string{"abcd\n"},
			wantOver:  []bool{false},
		},
		{
			name:      "over bound: discard and resume",
			input:     "ok1\n" + strings.Repeat("x", 10) + "\nok2\n",
			max:       4,
			wantLines: []string{"ok1\n", "", "ok2\n"},
			wantOver:  []bool{false, true, false},
		},
		{
			name:      "over bound at EOF without trailing newline",
			input:     strings.Repeat("x", 10),
			max:       4,
			wantLines: []string{""},
			wantOver:  []bool{true},
		},
		{
			name:      "empty",
			input:     "",
			max:       4,
			wantLines: nil,
			wantOver:  nil,
		},
		{
			name:      "trailing partial line without newline",
			input:     "abc",
			max:       16,
			wantLines: []string{"abc"},
			wantOver:  []bool{false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			br := bufio.NewReader(strings.NewReader(tt.input))
			var gotLines []string
			var gotOver []bool
			for {
				line, over, err := readBoundedLine(br, tt.max)
				if !over && len(line) == 0 && errors.Is(err, io.EOF) {
					break
				}
				gotLines = append(gotLines, string(line))
				gotOver = append(gotOver, over)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
			if len(gotLines) != len(tt.wantLines) {
				t.Fatalf("line count: got %d %v, want %d %v",
					len(gotLines), gotLines, len(tt.wantLines), tt.wantLines)
			}
			for i := range tt.wantLines {
				if gotLines[i] != tt.wantLines[i] {
					t.Errorf("line[%d]: got %q, want %q", i, gotLines[i], tt.wantLines[i])
				}
				if gotOver[i] != tt.wantOver[i] {
					t.Errorf("oversize[%d]: got %v, want %v", i, gotOver[i], tt.wantOver[i])
				}
			}
		})
	}
}

// TestJSONLSource_EmitsUnredactedEnvelopes guards against a
// regression where the Source re-acquires its own Redactor.
// Translation is pure; redaction is the consuming Pipeline's job.
// If a future change adds edge redaction back to the Source, this
// test fails and forces the discussion.
func TestJSONLSource_EmitsUnredactedEnvelopes(t *testing.T) {
	t.Parallel()
	path := writeJSONL(t, userEntryFixture)
	src := &JSONLSource{Root: path}

	got := collect(t, src)
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if got[0].Envelope.Redaction != nil {
		t.Errorf("Source must emit unredacted envelopes; got Redaction=%+v",
			got[0].Envelope.Redaction)
	}
}
