package cli

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateGolden rewrites the .golden fixtures instead of comparing.
// Run: go test ./internal/cli/ -run TestGolden -update-golden
var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/golden/* fixtures")

// assertGolden compares got against testdata/golden/<name>, or rewrites
// it under -update-golden. A trailing newline is enforced so the files
// stay diff-friendly. Used by the Layer-4 regression tests to snapshot
// the post-grounding structured output of the LLM pipelines: a prompt,
// schema, or grounding edit that silently changes what gets persisted
// shows up as a golden diff in review.
func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	if len(got) == 0 || got[len(got)-1] != '\n' {
		got = append(got, '\n')
	}
	path := filepath.Join("testdata", "golden", name)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %q: %v\nregenerate with: go test ./internal/cli/ -run %s -update-golden",
			path, err, t.Name())
	}
	if !bytes.Equal(want, got) {
		t.Errorf("golden %q mismatch (regenerate with -update-golden if the change is intended):\n--- want ---\n%s\n--- got ---\n%s",
			name, want, got)
	}
}
