package events

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// WalkSourceFiles enumerates the files a Source should iterate over.
// Used by adapters (Claude transcripts, Gemini sessions, future
// codex / opencode) that walk a directory tree picking files by
// extension.
//
// Semantics:
//   - Empty root returns (nil, nil) — caller short-circuits.
//   - root is a regular file → returns [root] regardless of extension
//     (single-file mode, e.g. `aichronicles import-claude /tmp/x.jsonl`).
//   - root is a directory → recursively collects every regular file
//     whose path ends in ext, preserving discovery order.
//   - root does not exist → returns (nil, error) with a stat: prefix.
//
// The walk is one-shot and resolves before iteration begins (vs
// streaming) so the caller can branch on len(files) == 0 without
// racing the filesystem. At realistic Claude/Gemini transcript
// counts (low thousands) the slice is a few hundred KB at most.
func WalkSourceFiles(ctx context.Context, root, ext string) ([]string, error) {
	if root == "" {
		return nil, nil
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", root, err)
	}
	if !info.IsDir() {
		return []string{root}, nil
	}
	var files []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ext) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	return files, nil
}
