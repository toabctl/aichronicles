package events

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestWalkSourceFilesEmptyRoot(t *testing.T) {
	t.Parallel()
	files, err := WalkSourceFiles(context.Background(), "", ".jsonl")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if files != nil {
		t.Errorf("empty root: got %v, want nil", files)
	}
}

func TestWalkSourceFilesMissingRoot(t *testing.T) {
	t.Parallel()
	_, err := WalkSourceFiles(context.Background(), "/nope/not/a/path", ".jsonl")
	if err == nil {
		t.Fatal("missing root: expected error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "stat ") {
		t.Errorf("error prefix: %v", err)
	}
}

func TestWalkSourceFilesSingleFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "single.jsonl")
	if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Single-file mode bypasses the extension filter; pass a wrong
	// extension to prove it.
	files, err := WalkSourceFiles(context.Background(), p, ".other")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 1 || files[0] != p {
		t.Errorf("single file: got %v, want [%s]", files, p)
	}
}

func TestWalkSourceFilesDirectoryRecursive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Build a small tree:
	//   dir/a.jsonl
	//   dir/b.json       (filtered by ext)
	//   dir/sub/c.jsonl
	//   dir/sub/skip.txt (filtered)
	paths := map[string]string{
		filepath.Join(dir, "a.jsonl"):         "{}",
		filepath.Join(dir, "b.json"):          "{}",
		filepath.Join(dir, "sub", "c.jsonl"):  "{}",
		filepath.Join(dir, "sub", "skip.txt"): "x",
	}
	for p, content := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	files, err := WalkSourceFiles(context.Background(), dir, ".jsonl")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sort.Strings(files)
	want := []string{
		filepath.Join(dir, "a.jsonl"),
		filepath.Join(dir, "sub", "c.jsonl"),
	}
	if len(files) != len(want) {
		t.Fatalf("count: got %d (%v), want %d (%v)", len(files), files, len(want), want)
	}
	for i := range files {
		if files[i] != want[i] {
			t.Errorf("file[%d]: got %s, want %s", i, files[i], want[i])
		}
	}
}

func TestWalkSourceFilesContextCancelled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jsonl"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := WalkSourceFiles(ctx, dir, ".jsonl")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}
