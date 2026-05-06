// Package skills walks Claude Code's on-disk skill layout
// (~/.claude/skills/<name>/SKILL.md plus per-project mirrors) and
// joins it with the skill_load extractions in the store. Pulled
// out of internal/cli so both the propose CLI path and the web
// /skills handler can use it without a cycle.
package skills

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/pkg/events"
)

// ClaudeSkillsDirName is the directory Claude Code reads SKILL.md
// files from, both globally (under $HOME/.claude/) and per-project
// (under <project>/.claude/). One canonical constant so global +
// project-local walkers stay in sync.
const ClaudeSkillsDirName = ".claude/skills"

// CollectInstalled walks every SKILL.md under
// $HOME/.claude/skills/ (global) and every distinct project root
// derivable from sessions in the window (project-local) and
// returns a deduplicated, alphabetised slice. A skill that exists
// in both layers is reported once with its project-local
// description (project-local wins because it's the more specific
// definition).
//
// Errors from individual SKILL.md files are swallowed: a malformed
// skill should not block the caller. Missing directories are
// fine — that's the common case.
func CollectInstalled(ctx context.Context, db *sql.DB, sinceMs int64) ([]prompts.InstalledSkill, error) {
	byName := make(map[string]prompts.InstalledSkill)

	// Global: $HOME/.claude/skills/*/SKILL.md
	if home, err := os.UserHomeDir(); err == nil {
		for _, s := range ScanDir(filepath.Join(home, ClaudeSkillsDirName), "global") {
			byName[s.Name] = s
		}
	}

	// Project-local: <project-root>/.claude/skills/*/SKILL.md for
	// every distinct start cwd we observed in the window.
	roots, err := LoadDistinctSessionStartCwds(ctx, db, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("CollectInstalled: %w", err)
	}
	seenRoots := make(map[string]struct{}, len(roots))
	for _, cwd := range roots {
		root := FindProjectRoot(cwd)
		if root == "" {
			continue
		}
		if _, dup := seenRoots[root]; dup {
			continue
		}
		seenRoots[root] = struct{}{}
		for _, s := range ScanDir(filepath.Join(root, ClaudeSkillsDirName), "project:"+root) {
			byName[s.Name] = s // project-local wins on collision
		}
	}

	out := make([]prompts.InstalledSkill, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// CollectInstalledForCwd returns global SKILL.md plus the
// project-local SKILL.md set discoverable from a single cwd.
// Mirrors CollectInstalled's de-dup-and-sort contract but skips
// the LoadDistinctSessionStartCwds query — caller already knows
// the cwd. Used by MCP get_project_context, where the agent
// passes its current cwd in the tool args and we don't need a
// store-side discovery walk.
//
// cwd may be empty: that yields global-only.
func CollectInstalledForCwd(cwd string) []prompts.InstalledSkill {
	byName := make(map[string]prompts.InstalledSkill)

	if home, err := os.UserHomeDir(); err == nil {
		for _, s := range ScanDir(filepath.Join(home, ClaudeSkillsDirName), "global") {
			byName[s.Name] = s
		}
	}

	if cwd != "" {
		if root := FindProjectRoot(cwd); root != "" {
			for _, s := range ScanDir(filepath.Join(root, ClaudeSkillsDirName), "project:"+root) {
				byName[s.Name] = s // project-local wins on collision
			}
		}
	}

	out := make([]prompts.InstalledSkill, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FindProjectRoot walks upward from cwd looking for the first
// ancestor that contains .claude/skills/. Returns "" when no such
// ancestor exists. Stops at the filesystem root.
func FindProjectRoot(cwd string) string {
	if cwd == "" {
		return ""
	}
	dir := filepath.Clean(cwd)
	for {
		candidate := filepath.Join(dir, ClaudeSkillsDirName)
		info, err := os.Stat(candidate)
		if err == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir { // reached root
			return ""
		}
		dir = parent
	}
}

// FindProjectRootGeneric is a broader sibling of FindProjectRoot.
// It walks upward from cwd looking for any of the conventional
// project-root markers — .claude/, .git/, or go.mod — and returns
// the first ancestor that contains one. Used by the web /projects
// page to roll sessions up to a useful grouping even when the
// project doesn't carry skills (most repos don't have
// .claude/skills, but most have .git).
//
// Returns the input cwd unchanged when no marker is found, so
// callers always get a non-empty string and can group consistently.
func FindProjectRootGeneric(cwd string) string {
	if cwd == "" {
		return ""
	}
	dir := filepath.Clean(cwd)
	for {
		for _, marker := range []string{".claude", ".git", "go.mod"} {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return cwd // no marker found; fall back to the original
		}
		dir = parent
	}
}

// ScanDir reads every immediate sub-directory of root, looks for
// a SKILL.md inside it, parses the frontmatter, and returns one
// InstalledSkill per parseable entry. The skill's canonical name
// is the directory name (kebab-case, matches what the `Skill`
// tool's `skill` arg carries) — NOT the human-readable `name`
// field in YAML, because those don't always agree
// ("Effective Go" vs "effective-go").
func ScanDir(root, source string) []prompts.InstalledSkill {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []prompts.InstalledSkill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), "SKILL.md")
		desc, err := ReadDescription(path)
		if err != nil || desc == "" {
			continue
		}
		out = append(out, prompts.InstalledSkill{
			Name:        e.Name(),
			Description: desc,
			Source:      source,
		})
	}
	return out
}

// ReadVersion extracts the `version:` field from a SKILL.md file's
// YAML frontmatter. The AutoSkill (Yang et al., 2026 —
// arXiv:2603.01145) v slot — semver-ish, e.g. "v0.1.0" — is the
// merge path's load-bearing input: the existing version gets
// patch-bumped when a candidate is merged in. Returns ("", nil)
// when the frontmatter exists but has no version key (the file was
// authored before the AutoSkill metadata shipped); callers fall
// back to InitialSkillVersion in that case.
//
// Format-tolerance is the same as ReadDescription: line-by-line,
// supports bare, "double-quoted", and 'single-quoted' values; does
// not handle multi-line / folded scalars.
func ReadVersion(path string) (string, error) {
	return readScalarFrontmatter(path, "version")
}

// readScalarFrontmatter is the shared body for ReadDescription and
// ReadVersion — extract one named scalar from the YAML frontmatter
// fence. Centralising it keeps the parser quirks (quote-stripping,
// fence detection) in one place.
func readScalarFrontmatter(path, key string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)

	if !sc.Scan() {
		return "", errors.New("empty file")
	}
	if strings.TrimSpace(sc.Text()) != "---" {
		return "", errors.New("no frontmatter fence")
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			return "", nil
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 {
			first, last := v[0], v[len(v)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				v = v[1 : len(v)-1]
			}
		}
		return v, nil
	}
	return "", sc.Err()
}

// ReadDescription extracts the `description:` field from a
// SKILL.md file's YAML frontmatter. The frontmatter format is
// constrained enough that a line-by-line scan is robust; we
// avoid a YAML parser to keep the dep surface small.
//
// Accepts: `description: foo`, `description: "foo"`, `description: 'foo'`.
// Multi-line / folded descriptions are not parsed (returned empty),
// which biases toward "no entry" over "wrong entry."
func ReadDescription(path string) (string, error) {
	return readScalarFrontmatter(path, "description")
}

// LoadDistinctSessionStartCwds returns the start cwd of every
// session whose ended_at is within the window. The "start cwd"
// is the first event's cwd, not the trigger-maintained
// sessions.cwd (which holds the latest cwd). Start cwd is what
// Claude Code roots the project for skill resolution.
//
// Bound to the window so a long-lived dotfile cwd from years ago
// doesn't drag in a useless project root.
func LoadDistinctSessionStartCwds(ctx context.Context, db *sql.DB, sinceMs int64) ([]string, error) {
	const q = `
WITH first_event AS (
    SELECT e.session_id,
           MIN(e.ts_source_ms) AS ts
      FROM events e
      JOIN sessions s ON s.id = e.session_id
     WHERE s.ended_at_ms >= ?
       AND e.cwd IS NOT NULL
     GROUP BY e.session_id
)
SELECT DISTINCT e.cwd
  FROM events e
  JOIN first_event fe
    ON fe.session_id = e.session_id AND fe.ts = e.ts_source_ms
 WHERE e.cwd IS NOT NULL`
	rows, err := db.QueryContext(ctx, q, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("query distinct cwds: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var cwd string
		if err := rows.Scan(&cwd); err != nil {
			return nil, fmt.Errorf("scan cwd: %w", err)
		}
		out = append(out, cwd)
	}
	return out, rows.Err()
}

// LoadInvoked returns the skill_load extraction counts from the
// given window, sorted by descending count. Empty slice is fine
// — many users don't load skills explicitly via the Skill tool.
func LoadInvoked(ctx context.Context, db *sql.DB, sinceMs int64) ([]prompts.InvokedSkill, error) {
	const q = `
SELECT x.value, COUNT(*) AS c
  FROM extractions x
  JOIN sessions s ON s.id = x.session_id
 WHERE x.kind = ? AND s.ended_at_ms >= ?
 GROUP BY x.value
 ORDER BY c DESC, x.value ASC`
	rows, err := db.QueryContext(ctx, q, events.ExtractionKindSkillLoad, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("query invoked skills: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []prompts.InvokedSkill
	for rows.Next() {
		var s prompts.InvokedSkill
		if err := rows.Scan(&s.Name, &s.Count); err != nil {
			return nil, fmt.Errorf("scan invoked skill: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
