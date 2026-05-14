package events

import (
	"path/filepath"
	"regexp"
)

// urlRE matches http/https URLs conservatively. The character class
// excludes whitespace and common trailing punctuation that often
// surrounds URLs in prose. Trailing punctuation is further trimmed
// below in case it slipped into the match.
var urlRE = regexp.MustCompile(`https?://[^\s<>"'\x60]+`)

// URLExtractor emits one row per distinct http(s) URL found in
// ContentText. Deduped within the envelope so duplicate text doesn't
// inflate extraction counts. Registered as a Content extractor
// because URLs surface in user prompts and assistant messages, not
// just in tool_input fields.
func URLExtractor(env *Envelope) []Extraction {
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
		m = trimURLTail(m)
		if m == "" {
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, Extraction{Kind: ExtractionKindURL, Value: m})
	}
	return out
}

// trimURLTail removes trailing prose punctuation from a captured URL
// without breaking URLs that legitimately end in a closing bracket.
// Always-strip chars (.,;:!?) are unconditional. Brackets ()[]{} are
// stripped only when unbalanced — Wikipedia-style URLs such as
// https://en.wikipedia.org/wiki/Foo_(bar) end in a balanced `)` that
// previously got mangled to "...Foo_(bar" by a naive TrimRight.
func trimURLTail(s string) string {
	for len(s) > 0 {
		last := s[len(s)-1]
		switch last {
		case '.', ',', ';', ':', '!', '?':
			s = s[:len(s)-1]
			continue
		case ')':
			if countByte(s, '(') < countByte(s, ')') {
				s = s[:len(s)-1]
				continue
			}
		case ']':
			if countByte(s, '[') < countByte(s, ']') {
				s = s[:len(s)-1]
				continue
			}
		case '}':
			if countByte(s, '{') < countByte(s, '}') {
				s = s[:len(s)-1]
				continue
			}
		}
		return s
	}
	return s
}

func countByte(s string, b byte) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			n++
		}
	}
	return n
}

// FilePathExtractor emits one extraction per file_path carried by a
// file-touching tool's tool_input. Registered for Read / Write /
// Edit / NotebookEdit; the tool-name guard lives in the registry,
// so by the time this runs env.Tool is non-nil and the tool name
// matches one of the registered keys.
//
// Relative paths are joined with env.Cwd before storing so the
// extraction is unambiguous and grep-friendly. Claude Code's
// Read/Write/Edit tools document absolute paths as their contract,
// but other agents (Codex, MCP tools) make no such guarantee, and
// the resulting `key_files` / search hits are noticeably more
// useful when every stored path is canonical. Absolute inputs
// pass through untouched (filepath.IsAbs short-circuits).
//
// When the path is relative AND env.Cwd is empty OR non-absolute
// the extraction is dropped: storing an unanchored "config.go"
// string would collide across every project that has a config.go
// file, and joining with a relative env.Cwd ("aichronicles/")
// produces a fabricated path indistinguishable from a real
// extraction. CLAUDE.md §7 prefers no extraction over a wrong one
// — the LLM can still infer the path from prose context
// downstream if it matters.
func FilePathExtractor(env *Envelope) []Extraction {
	input, ok := toolInput(env)
	if !ok {
		return nil
	}
	path, ok := input["file_path"].(string)
	if !ok || path == "" {
		return nil
	}
	if !filepath.IsAbs(path) {
		if env.Cwd == "" || !filepath.IsAbs(env.Cwd) {
			return nil
		}
		path = filepath.Join(env.Cwd, path)
	}
	return []Extraction{{Kind: ExtractionKindFilePath, Value: path}}
}

// ShellCommandExtractor emits the command string from a Bash
// tool_use, attaching tool_input.description under Extra when
// present. Registered for "Bash"; the tool-name guard lives in the
// registry.
func ShellCommandExtractor(env *Envelope) []Extraction {
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
	ex := Extraction{Kind: ExtractionKindShellCommand, Value: cmd}
	if len(extra) > 0 {
		ex.Extra = extra
	}
	return []Extraction{ex}
}

// WebFetchExtractor emits the fetched URL as kind=url so the typed
// fact joins the existing URL pool. Reusing ExtractionKindURL keeps
// the snippet labelling consistent — `[url] https://...` regardless
// of whether the URL was found in prose or in a WebFetch tool_input.
// Registered for "WebFetch"; tool-name guard lives in the registry.
func WebFetchExtractor(env *Envelope) []Extraction {
	input, ok := toolInput(env)
	if !ok {
		return nil
	}
	url, ok := input["url"].(string)
	if !ok || url == "" {
		return nil
	}
	return []Extraction{{Kind: ExtractionKindURL, Value: url}}
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
//
// Registered for "Skill"; tool-name guard lives in the registry.
// The function still handles two payload shapes (hook vs JSONL)
// because the tool name is stable across both but the input shape
// is not.
func SkillLoadExtractor(env *Envelope) []Extraction {
	skill, args, ok := skillToolInput(env)
	if !ok {
		return nil
	}
	ex := Extraction{Kind: ExtractionKindSkillLoad, Value: skill}
	if args != "" {
		ex.Extra = map[string]any{"args": args}
	}
	return []Extraction{ex}
}

// toolInput pulls the "tool_input" map out of an envelope's payload.
// Returns (nil, false) if the payload isn't shaped as expected —
// every tool extractor treats that as "nothing to extract here."
func toolInput(env *Envelope) (map[string]any, bool) {
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

// skillToolInput pulls the Skill tool's invocation arguments out of
// an envelope's payload, handling the two payload shapes we receive
// today:
//
//   - hook events (live ingest from Claude Code's hook):
//     payload.tool_input.{skill, args}
//   - imported transcripts (~/.claude/projects/*.jsonl):
//     payload.message.content[i].{type:"tool_use",name:"Skill",input:{skill,args}}
//
// Returns (skill, args, true) when either form can be parsed; the
// args field is optional and may be empty.
func skillToolInput(env *Envelope) (skill, args string, ok bool) {
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
