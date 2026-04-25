// Package extract pulls structured facts out of an ingest envelope —
// URLs, file paths the agent touched, shell commands it ran — for
// storage in the extractions table so they can be filtered/queried
// cheaply without re-scanning content.
//
// Extractors are deliberately conservative: false negatives are
// acceptable (we'll miss some real things), false positives are not
// (we don't want to clutter the index with junk that looks like a URL
// but isn't). If in doubt, don't emit.
//
// Extension model: every extractor satisfies the Extractor function
// type. FromEnvelope runs each element of Registered in order. To
// add a new one, define a func with the right signature and append
// it to Registered (typically in an init or in a test via a helper).
package extract

import (
	"regexp"
	"strings"

	"github.com/toabctl/aichronicles/pkg/ingest"
)

// Kind names for Extraction.Kind. Kept as exported constants so
// callers (the store, future MCP search tools) match on them.
const (
	KindURL          = "url"
	KindFilePath     = "file_path"
	KindShellCommand = "shell_command"
	KindGrepPattern  = "grep_pattern"
	KindGlobPattern  = "glob_pattern"
	KindWebQuery     = "web_query"
)

// Extraction is one fact derived from an envelope. The caller writes
// it to the extractions table; Extra serializes to extra_json.
type Extraction struct {
	Kind  string
	Value string
	Extra map[string]any
}

// Extractor is the uniform contract every extractor satisfies:
// pure (no state, no I/O), total (no error — best-effort), safe for
// concurrent use. Returns zero or more extractions.
type Extractor func(env *ingest.Envelope) []Extraction

// Registered is the ordered list FromEnvelope walks. Tests and (later)
// config gating mutate this slice for isolation; production reads the
// built-in defaults below. Keep ordering stable so integration tests
// that depend on row order stay reliable.
var Registered = []Extractor{
	URLExtractor,
	FilePathExtractor,
	ShellCommandExtractor,
	GrepExtractor,
	GlobExtractor,
	WebFetchExtractor,
	WebSearchExtractor,
}

// FromEnvelope runs every Registered extractor against env and returns
// their combined output. Nil envelope → nil slice.
func FromEnvelope(env *ingest.Envelope) []Extraction {
	if env == nil {
		return nil
	}
	var out []Extraction
	for _, e := range Registered {
		out = append(out, e(env)...)
	}
	return out
}

// urlRE matches http/https URLs conservatively. The character class
// excludes whitespace and common trailing punctuation that often
// surrounds URLs in prose. Trailing punctuation is further trimmed
// below in case it slipped into the match.
var urlRE = regexp.MustCompile(`https?://[^\s<>"'\x60]+`)

// URLExtractor emits one row per distinct http(s) URL found in
// ContentText. Deduped within the envelope so duplicate text doesn't
// inflate extraction counts.
func URLExtractor(env *ingest.Envelope) []Extraction {
	if env.ContentText == "" {
		return nil
	}
	matches := urlRE.FindAllString(env.ContentText, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]Extraction, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		m = strings.TrimRight(m, ".,;:!?)]}")
		if m == "" {
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, Extraction{Kind: KindURL, Value: m})
	}
	return out
}

// fileToolNames are the Claude Code tools whose tool_input.file_path
// represents a genuine file reference we want indexed.
var fileToolNames = map[string]struct{}{
	"Read":         {},
	"Write":        {},
	"Edit":         {},
	"NotebookEdit": {},
}

// FilePathExtractor emits one extraction per file_path carried by a
// canonical file-touching tool's tool_input.
func FilePathExtractor(env *ingest.Envelope) []Extraction {
	if env.Tool == nil {
		return nil
	}
	if _, ok := fileToolNames[env.Tool.Name]; !ok {
		return nil
	}
	input, ok := toolInput(env)
	if !ok {
		return nil
	}
	path, ok := input["file_path"].(string)
	if !ok || path == "" {
		return nil
	}
	return []Extraction{{Kind: KindFilePath, Value: path}}
}

// shellToolName is the Claude Code tool that runs shell commands.
// Other tools may carry a "command" field for unrelated reasons — we
// only extract from Bash.
const shellToolName = "Bash"

// ShellCommandExtractor emits the command string from a Bash
// tool_use, attaching tool_input.description under Extra when present.
func ShellCommandExtractor(env *ingest.Envelope) []Extraction {
	if env.Tool == nil || env.Tool.Name != shellToolName {
		return nil
	}
	input, ok := toolInput(env)
	if !ok {
		return nil
	}
	cmd, ok := input["command"].(string)
	if !ok || cmd == "" {
		return nil
	}
	extra := map[string]any{}
	if desc, ok := input["description"].(string); ok && desc != "" {
		extra["description"] = desc
	}
	ex := Extraction{Kind: KindShellCommand, Value: cmd}
	if len(extra) > 0 {
		ex.Extra = extra
	}
	return []Extraction{ex}
}

// toolInput pulls the "tool_input" map out of an envelope's payload.
// Returns (nil, false) if the payload isn't shaped as expected —
// every extractor treats that as "nothing to extract here."
func toolInput(env *ingest.Envelope) (map[string]any, bool) {
	if env.Payload == nil {
		return nil, false
	}
	raw, ok := env.Payload["tool_input"]
	if !ok {
		return nil, false
	}
	m, ok := raw.(map[string]any)
	return m, ok
}

// GrepExtractor emits the regex pattern from a Grep tool_use, with
// the optional path attached as Extra.path so callers can filter to
// "patterns I ran inside internal/" later. Path-less Grep
// invocations still produce one extraction with the bare pattern.
func GrepExtractor(env *ingest.Envelope) []Extraction {
	if env.Tool == nil || env.Tool.Name != "Grep" {
		return nil
	}
	input, ok := toolInput(env)
	if !ok {
		return nil
	}
	pat, ok := input["pattern"].(string)
	if !ok || pat == "" {
		return nil
	}
	ex := Extraction{Kind: KindGrepPattern, Value: pat}
	if path, ok := input["path"].(string); ok && path != "" {
		ex.Extra = map[string]any{"path": path}
	}
	return []Extraction{ex}
}

// GlobExtractor emits the glob pattern from a Glob tool_use. Glob
// has no separate path argument worth attaching.
func GlobExtractor(env *ingest.Envelope) []Extraction {
	if env.Tool == nil || env.Tool.Name != "Glob" {
		return nil
	}
	input, ok := toolInput(env)
	if !ok {
		return nil
	}
	pat, ok := input["pattern"].(string)
	if !ok || pat == "" {
		return nil
	}
	return []Extraction{{Kind: KindGlobPattern, Value: pat}}
}

// WebFetchExtractor emits the fetched URL as kind=url so the typed
// fact joins the existing URL pool. Reusing KindURL keeps the
// snippet labelling consistent — `[url] https://...` regardless of
// whether the URL was found in prose or in a WebFetch tool_input.
func WebFetchExtractor(env *ingest.Envelope) []Extraction {
	if env.Tool == nil || env.Tool.Name != "WebFetch" {
		return nil
	}
	input, ok := toolInput(env)
	if !ok {
		return nil
	}
	url, ok := input["url"].(string)
	if !ok || url == "" {
		return nil
	}
	return []Extraction{{Kind: KindURL, Value: url}}
}

// WebSearchExtractor emits the search query from a WebSearch
// tool_use. Distinct kind because a search query isn't a URL or a
// pattern — it's a natural-language string that's useful to recall
// on its own ("what did I search for last week").
func WebSearchExtractor(env *ingest.Envelope) []Extraction {
	if env.Tool == nil || env.Tool.Name != "WebSearch" {
		return nil
	}
	input, ok := toolInput(env)
	if !ok {
		return nil
	}
	q, ok := input["query"].(string)
	if !ok || q == "" {
		return nil
	}
	return []Extraction{{Kind: KindWebQuery, Value: q}}
}
