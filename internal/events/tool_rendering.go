package events

import "strings"

// RenderToolContent produces a one-liner suitable for content_text
// and FTS indexing, derived from a hook payload's tool_name and
// tool_input. Shared between hook-shaped Sources (Claude,
// Gemini, Codex) because all three pass tool_input fields with
// identical names
// (`{command, file_path, pattern, url, ...}`); the tool naming
// differs (PascalCase vs snake_case) but the field shape doesn't.
//
// The tool name leads as its own token so queries for the tool
// itself match; the most informative tool_input field follows so
// a search like `cluster` finds Bash invocations whose command
// mentions cluster, or Grep invocations whose pattern contains
// cluster, without depending on the extractions fallback.
//
// Falls back to the bare tool name (or empty) for tools whose
// tool_input shape we don't know — preserves behaviour for
// every tool we haven't enumerated.
func RenderToolContent(hook map[string]any) string {
	name, _ := hook["tool_name"].(string)
	if name == "" {
		return ""
	}
	input, _ := hook["tool_input"].(map[string]any)
	detail := toolDetail(name, input)
	if detail == "" {
		return name
	}
	return name + " " + detail
}

// toolDetail picks the most informative single string from a known
// tool's tool_input. Returns empty for unknown tools, which makes
// RenderToolContent fall back to the bare tool name. Adding a new
// tool here should be paired with a matching extractor (in
// extractors_builtin.go + DefaultExtractors's Tool map) so the
// typed-fact tier can also reach it.
//
// Both Claude Code's tool naming (PascalCase: Bash, Read, …) and
// Gemini CLI's equivalents (snake_case: run_shell_command,
// read_file, …) are handled here. The tool_input field names are
// identical across the agents — all pass `{command, ...}` for
// shell, `{file_path, ...}` for file ops, etc. — so one switch
// with every name per case keeps the renderer consistent. Codex
// CLI reuses Claude's PascalCase names on the wire and needs no
// aliases of its own; only apply_patch, which has no Claude or
// Gemini counterpart, is Codex-specific.
func toolDetail(toolName string, input map[string]any) string {
	if input == nil {
		return ""
	}
	switch toolName {
	case "Bash", "run_shell_command":
		return stringField(input, "command")
	case "Read", "read_file",
		"Write", "write_file",
		"Edit", "replace",
		"NotebookEdit":
		// Gemini's read_file uses `absolute_path`; write_file and
		// replace use `file_path`. Both shapes covered here.
		if p := stringField(input, "file_path"); p != "" {
			return p
		}
		return stringField(input, "absolute_path")
	case "Grep", "search_file_content":
		pat := stringField(input, "pattern")
		path := stringField(input, "path")
		if pat != "" && path != "" {
			return pat + " " + path
		}
		return pat
	case "Glob", "find":
		return stringField(input, "pattern")
	case "WebFetch", "web_fetch":
		return stringField(input, "url")
	case "WebSearch", "google_web_search":
		return stringField(input, "query")
	case "apply_patch":
		// Codex CLI's edit tool. tool_input is
		// {"command": "*** Begin Patch\n*** Add File: …"} — the
		// whole patch, body included. Rendering it verbatim would
		// dump a multi-line diff into content_text; rendering the
		// touched paths gives the one thing people actually search
		// an edit by.
		return applyPatchPaths(stringField(input, "command"))
	case "Task":
		// Sub-agent launch. Both fields are typically present;
		// description is shorter and clearer for a one-line
		// preview, prompt is the full instructions.
		if d := stringField(input, "description"); d != "" {
			return d
		}
		return stringField(input, "prompt")
	}
	// Unknown tool (commonly an MCP tool — `mcp__server__name` in
	// Claude Code's namespace, with a per-server tool_input schema
	// we can't enumerate). Fall back to the longest string-valued
	// field in tool_input so the most informative payload still
	// reaches FTS without a per-server allow-list.
	return longestStringValue(input)
}

// stringField returns m[key] as a string when present and non-empty,
// "" otherwise. Avoids the awkward two-step ok-check at every call
// site without hiding the type assertion.
func stringField(m map[string]any, key string) string {
	s, ok := m[key].(string)
	if !ok {
		return ""
	}
	return s
}

// longestStringValue returns the longest string-typed value in m
// when it's clearly the informative payload, or "" otherwise. Used
// as the fallback rendering for tools we don't know per-shape —
// MCP tools typically carry one big string field (a query, a path,
// a body) alongside knobs; surfacing the longest one is a cheap
// proxy for "the informative part."
//
// Two restraints from the B8 audit fix:
//
//   - Minimum length of fallbackMinLen runes before we'll surface a
//     value at all. A short string field is more likely a flag,
//     id, or credential than a payload — we'd rather render the
//     bare tool name than risk a false-positive secret leak.
//
//   - The longest value must clearly dominate any other strings.
//     If two fields are similar lengths, neither is "obviously the
//     payload"; refuse and fall back to bare tool name. Mitigates
//     the risk of picking the wrong field when the schema gives
//     no guidance.
//
// Non-string fields (numbers, nested objects, booleans) are ignored
// deliberately to keep content_text tight.
const (
	fallbackMinLen      = 16 // shorter than this is likely a knob, not the payload
	fallbackDominanceX2 = 2  // longest must be at least 2× the runner-up to win
)

func longestStringValue(m map[string]any) string {
	if m == nil {
		return ""
	}
	var best, secondBest string
	for _, v := range m {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if len(s) > len(best) {
			secondBest = best
			best = s
		} else if len(s) > len(secondBest) {
			secondBest = s
		}
	}
	if len([]rune(best)) < fallbackMinLen {
		return ""
	}
	// "Clearly dominates" check: longest at least 2× the runner-up,
	// or there's no runner-up at all (only one string field).
	if secondBest != "" && len(best) < fallbackDominanceX2*len(secondBest) {
		return ""
	}
	return best
}

// applyPatchVerbs are the per-file headers in Codex's apply_patch
// envelope. Each introduces one path; "*** Move to:" names the
// destination of a rename, whose source came from the preceding
// "*** Update File:". Taken from the format codex-cli validates
// against ("The first line of the patch must be '*** Begin
// Patch'"), not inferred from a sample.
var applyPatchVerbs = []string{
	"*** Add File: ",
	"*** Update File: ",
	"*** Delete File: ",
	"*** Move to: ",
}

// applyPatchPaths returns the space-joined list of files a Codex
// apply_patch envelope touches, in the order they appear and
// without duplicates (a rename names its path twice — once to
// update, once to move). Returns "" for anything that isn't
// recognisably a patch envelope, which makes RenderToolContent
// fall back to the bare tool name.
//
// Deliberately literal: only lines that begin with a known verb
// count. A hunk body line that happens to start with "***" is not
// a header, and a patch we can't parse yields nothing rather than
// a half-read path list.
func applyPatchPaths(patch string) string {
	if patch == "" {
		return ""
	}
	var paths []string
	seen := make(map[string]struct{})
	for _, line := range strings.Split(patch, "\n") {
		for _, verb := range applyPatchVerbs {
			rest, ok := strings.CutPrefix(line, verb)
			if !ok {
				continue
			}
			p := strings.TrimSpace(rest)
			if p == "" {
				break
			}
			if _, dup := seen[p]; !dup {
				seen[p] = struct{}{}
				paths = append(paths, p)
			}
			break
		}
	}
	return strings.Join(paths, " ")
}
