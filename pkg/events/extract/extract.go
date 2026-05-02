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
	"path/filepath"
	"regexp"
	"strings"

	"github.com/toabctl/aichronicles/pkg/events"
)

// Kind names for Extraction.Kind. Kept as exported constants so
// callers (the store, future MCP search tools) match on them.
const (
	KindURL          = "url"
	KindFilePath     = "file_path"
	KindShellCommand = "shell_command"
	KindSkillLoad    = "skill_load"
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
type Extractor func(env *events.Envelope) []Extraction

// Registered is the ordered list FromEnvelope walks. Tests and (later)
// config gating mutate this slice for isolation; production reads the
// built-in defaults below. Keep ordering stable so integration tests
// that depend on row order stay reliable.
var Registered = []Extractor{
	URLExtractor,
	FilePathExtractor,
	ShellCommandExtractor,
	WebFetchExtractor,
	SkillLoadExtractor,
}

// FromEnvelope runs every Registered extractor against env and returns
// their combined output. Nil envelope → nil slice.
func FromEnvelope(env *events.Envelope) []Extraction {
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
func URLExtractor(env *events.Envelope) []Extraction {
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
//
// Relative paths are joined with env.Cwd before storing so the
// extraction is unambiguous and grep-friendly. Claude Code's
// Read/Write/Edit tools document absolute paths as their contract,
// but other agents (Codex, MCP tools) make no such guarantee, and
// the resulting `key_files` / search hits are noticeably more
// useful when every stored path is canonical. Absolute inputs
// pass through untouched (filepath.IsAbs short-circuits).
func FilePathExtractor(env *events.Envelope) []Extraction {
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
	if !filepath.IsAbs(path) && env.Cwd != "" {
		path = filepath.Join(env.Cwd, path)
	}
	return []Extraction{{Kind: KindFilePath, Value: path}}
}

// shellToolName is the Claude Code tool that runs shell commands.
// Other tools may carry a "command" field for unrelated reasons — we
// only extract from Bash.
const shellToolName = "Bash"

// ShellCommandExtractor emits the command string from a Bash
// tool_use, attaching tool_input.description under Extra when present.
func ShellCommandExtractor(env *events.Envelope) []Extraction {
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
func toolInput(env *events.Envelope) (map[string]any, bool) {
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

// WebFetchExtractor emits the fetched URL as kind=url so the typed
// fact joins the existing URL pool. Reusing KindURL keeps the
// snippet labelling consistent — `[url] https://...` regardless of
// whether the URL was found in prose or in a WebFetch tool_input.
func WebFetchExtractor(env *events.Envelope) []Extraction {
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

// skillToolInput pulls the Skill tool's invocation arguments out of
// an envelope's payload, handling the two payload shapes we receive
// today:
//
//   - hook events (live ingest from Claude Code's hook):
//     payload.tool_input.{skill, args}
//   - imported transcripts (~/.claude/projects/*.jsonl):
//     payload.message.content[i].{type:"tool_use",name:"Skill",input:{skill,args}}
//
// Returns (skill, args, true) when both forms can be parsed; the
// args field is optional and may be empty. A future Codex / gemini
// adapter would extend this same helper rather than the extractor
// body — keeping the extractor logic itself shape-agnostic.
func skillToolInput(env *events.Envelope) (skill, args string, ok bool) {
	if env.Payload == nil {
		return "", "", false
	}

	// Hook shape: {tool_input: {skill, args}}
	if input, found := toolInput(env); found {
		s, _ := input["skill"].(string)
		a, _ := input["args"].(string)
		if s != "" {
			return s, a, true
		}
	}

	// Import shape: {message:{content:[{type:"tool_use",name:"Skill",input:{skill,args}}]}}
	msg, _ := env.Payload["message"].(map[string]any)
	if msg == nil {
		return "", "", false
	}
	blocks, _ := msg["content"].([]any)
	for _, b := range blocks {
		bm, _ := b.(map[string]any)
		if bm == nil {
			continue
		}
		if t, _ := bm["type"].(string); t != "tool_use" {
			continue
		}
		if n, _ := bm["name"].(string); n != "Skill" {
			continue
		}
		input, _ := bm["input"].(map[string]any)
		if input == nil {
			continue
		}
		s, _ := input["skill"].(string)
		a, _ := input["args"].(string)
		if s != "" {
			return s, a, true
		}
	}
	return "", "", false
}

// SkillLoadExtractor emits the skill name from a Skill tool_use,
// attaching tool_input.args under Extra when present. Skill is
// Claude Code's per-session skill-invocation primitive: a tool_use
// with name="Skill" and input.skill=<skill-name>. The actual skill
// name lives only inside the payload — events.tool_name is just
// "Skill" — so without this extractor downstream queries can count
// "skill loads happened" but not "which skills."
//
// Powers downstream features: skill-frequency reports, propose
// awareness of installed-vs-invoked skills, and skill-staleness
// detection (load + subsequent failure correlation).
func SkillLoadExtractor(env *events.Envelope) []Extraction {
	if env.Tool == nil || env.Tool.Name != "Skill" {
		return nil
	}
	skill, args, ok := skillToolInput(env)
	if !ok {
		return nil
	}
	ex := Extraction{Kind: KindSkillLoad, Value: skill}
	if args != "" {
		ex.Extra = map[string]any{"args": args}
	}
	return []Extraction{ex}
}
