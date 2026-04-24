package daemon

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLogger_AppendAndClose(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	lg, err := OpenLogger(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })

	for i := 0; i < 3; i++ {
		if err := lg.AppendJSON(map[string]any{"i": i}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := lg.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	lines := readLines(t, path)
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	for idx, line := range lines {
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d: unmarshal %v", idx, err)
		}
		if got["i"].(float64) != float64(idx) {
			t.Fatalf("line %d: expected i=%d, got %v", idx, idx, got["i"])
		}
	}
}

func TestLogger_ConcurrentAppendsAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	lg, err := OpenLogger(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	const workers, perWorker = 8, 25
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(wid int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				_ = lg.AppendJSON(map[string]any{"w": wid, "i": i})
			}
		}(w)
	}
	wg.Wait()
	_ = lg.Close()

	lines := readLines(t, path)
	if got, want := len(lines), workers*perWorker; got != want {
		t.Fatalf("expected %d lines, got %d", want, got)
	}
	for idx, line := range lines {
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d parse failed (torn write?): %v", idx, err)
		}
	}
}

func TestOpenLogger_CreatesFileWithTightPerms(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	lg, err := OpenLogger(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = lg.Close()

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected perm 0600, got %o", got)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open for read: %v", err)
	}
	defer func() { _ = f.Close() }()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}
