package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/toabctl/aichronicles/pkg/events"
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
	src := &JSONLSource{Root: path, Redactor: events.NewScannerRedactor(nil)}

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
	src := &JSONLSource{Root: path, Redactor: events.NewScannerRedactor(nil)}

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
	src := &JSONLSource{Root: path, Redactor: events.NewScannerRedactor(nil)}

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
	src := &JSONLSource{Root: path, Redactor: events.NewScannerRedactor(nil)}

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
	src := &JSONLSource{Root: dir, Redactor: events.NewScannerRedactor(nil)}

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
	src := &JSONLSource{Redactor: events.NewScannerRedactor(nil)}
	got := collect(t, src)
	if len(got) != 0 {
		t.Errorf("empty root: got %d events, want 0", len(got))
	}
}

func TestJSONLSource_RedactionAppliedSetsFlag(t *testing.T) {
	t.Parallel()
	path := writeJSONL(t, userEntryFixture)
	src := &JSONLSource{Root: path, Redactor: events.NewScannerRedactor(nil)}

	got := collect(t, src)
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if got[0].Envelope.Redaction == nil || !got[0].Envelope.Redaction.Applied {
		t.Errorf("Redaction.Applied not set")
	}
}
