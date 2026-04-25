// Package prompts composes the fixed prompts Block B's features
// (summarize, reflect, propose) send to the LLM. Each Build* function
// returns:
//
//   - the llm.Request the caller will hand to a llm.Client (with a
//     forced-tool call so the reply arrives as validated JSON)
//   - a deterministic prompt hash the caller can use as the
//     llm_outputs cache key
//   - the union of redact.Outbound pattern names that fired while
//     assembling the prompt, so callers can log what got scrubbed
//
// Structured output via tool use: each feature declares exactly one
// tool (record_summary / record_reflection / record_proposal) and
// forces the model to call it. The model's JSON arguments arrive on
// Response.ToolUses[0].Input, ready to Unmarshal into the matching
// *Result type in this package.
//
// Egress redaction is enforced here, not in the llm transport.
// Every user-content string passes through redact.Outbound before it
// joins the prompt. If you need "abort on any finding" semantics,
// switch to redact.MustClean at the call site and check the returned
// pattern list.
package prompts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/redact"
)

// Built is the output of every Build* function. Returning it as a
// struct (not positional returns) keeps call sites readable when new
// metadata gets added — e.g. a token-budget estimate in the future.
type Built struct {
	Request  llm.Request
	Hash     string
	Patterns []string // redact patterns fired during assembly
}

// maxTokens defaults. Callers can override by setting Request.MaxTokens
// themselves after the build step if they need more headroom.
const (
	summaryMaxTokens = 1024
	reflectMaxTokens = 2048
	proposeMaxTokens = 2048
)

// Tool names. Keeping them as consts so callers (CLI wrappers, tests)
// can assert against a single source of truth rather than duplicating
// the string literal.
const (
	ToolNameSummary    = "record_summary"
	ToolNameReflection = "record_reflection"
	ToolNameProposal   = "record_proposal"
)

// --- result types ---

// SummaryResult is the schema-validated payload of a record_summary
// tool call. Fields are always populated (empty slices/strings on
// fields the model had nothing to say about).
type SummaryResult struct {
	Topic       string           `json:"topic"`
	WhatWasDone []string         `json:"what_was_done"`
	Unresolved  []string         `json:"unresolved"`
	KeyFiles    []string         `json:"key_files"`
	Links       []LinkAnnotation `json:"links"`
}

// LinkAnnotation pairs a URL the user actually referenced in a
// session with a one-line explanation of what it was used for. The
// URL must appear in the session's extractions table — the model is
// prompted to drop any link it can't confidently attribute rather
// than emit a filler `used_for`.
type LinkAnnotation struct {
	URL     string `json:"url"`
	UsedFor string `json:"used_for"`
}

// ReflectionResult is the schema-validated payload of a
// record_reflection tool call.
type ReflectionResult struct {
	TaskTypes      []Evidenced `json:"task_types"`
	Frictions      []Evidenced `json:"frictions"`
	WorkflowChange string      `json:"workflow_change"`
}

// Evidenced is a claim the model is making that must cite at least
// one session id as evidence.
type Evidenced struct {
	Label      string   `json:"label"`
	SessionIDs []string `json:"session_ids"`
}

// ProposalResult is the schema-validated payload of a
// record_proposal tool call. Each section capped at 5 items by
// schema so callers can render the output deterministically.
type ProposalResult struct {
	Skills          []ProposedSkill        `json:"skills"`
	ClaudeMdEntries []ProposedClaudeMdRule `json:"claude_md_entries"`
	Scripts         []ProposedScript       `json:"scripts"`
}

type ProposedSkill struct {
	Name       string   `json:"name"`
	WhenToUse  string   `json:"when_to_use"`
	Why        string   `json:"why"`
	SessionIDs []string `json:"session_ids"`
}

type ProposedClaudeMdRule struct {
	Rule       string   `json:"rule"`
	Why        string   `json:"why"`
	SessionIDs []string `json:"session_ids"`
}

type ProposedScript struct {
	Name       string   `json:"name"`
	Purpose    string   `json:"purpose"`
	SessionIDs []string `json:"session_ids"`
}

// --- summary ---

const summarySystem = `You summarize a single coding session between a human and an AI coding assistant. You MUST call the record_summary tool exactly once. Be factual and tight. Do not invent details. Do not invent URLs — only annotate links that were observed in the session. If a list section has no content, return an empty array.`

// summaryToolSchema is the JSON Schema for record_summary. Kept as a
// const so its bytes are stable; hashRequest includes these bytes
// when computing prompt_hash.
const summaryToolSchema = `{
  "type": "object",
  "required": ["topic","what_was_done","unresolved","key_files","links"],
  "additionalProperties": false,
  "properties": {
    "topic": {"type":"string","minLength":1},
    "what_was_done": {"type":"array","items":{"type":"string","minLength":1},"minItems":1,"maxItems":8},
    "unresolved": {"type":"array","items":{"type":"string","minLength":1}},
    "key_files": {"type":"array","items":{"type":"string","minLength":1}},
    "links": {
      "type":"array",
      "items":{
        "type":"object",
        "required":["url","used_for"],
        "additionalProperties": false,
        "properties":{
          "url":{"type":"string","minLength":1},
          "used_for":{"type":"string","minLength":1}
        }
      }
    }
  }
}`

const summaryTemplate = `Session: %s
Events: %d
%s
Transcript follows, oldest first:
---
%s
---
`

// BuildSummary returns the prompt for summarizing one session's
// events. links is the distinct URL list observed in the session
// (typically from store.LoadExtractionsForSession(kind='url')); the
// model is prompted to annotate each with a `used_for` via the
// record_summary tool, dropping any it cannot confidently attribute.
// Passing a nil/empty links slice is fine — the tool just receives
// an empty links array.
func BuildSummary(sessionID string, events []store.EventView, links []string) (Built, error) {
	if sessionID == "" {
		return Built{}, fmt.Errorf("BuildSummary: sessionID required")
	}
	if len(events) == 0 {
		return Built{}, fmt.Errorf("BuildSummary: no events for session %s", sessionID)
	}

	pats := patternSet{}
	transcript := renderEvents(events, pats)
	linksBlock := renderLinksBlock(links, pats)

	userMsg := fmt.Sprintf(summaryTemplate, sessionID, len(events), linksBlock, transcript)

	req := llm.Request{
		System:    summarySystem,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: userMsg}},
		MaxTokens: summaryMaxTokens,
		Tools: []llm.Tool{{
			Name:        ToolNameSummary,
			Description: "Record the structured summary of one coding session.",
			InputSchema: json.RawMessage(summaryToolSchema),
		}},
		ForceTool: ToolNameSummary,
	}
	return Built{Request: req, Hash: hashRequest(req), Patterns: pats.sortedSlice()}, nil
}

// renderLinksBlock formats the "Links observed" stanza that goes
// above the transcript, or returns "" when there are no links to
// annotate. The model is told to drop links it can't attribute so
// noise in extractions (e.g. spurious URLs in tool output) doesn't
// pollute the final summary.
func renderLinksBlock(links []string, pats patternSet) string {
	if len(links) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nLinks observed in this session — annotate each with a specific `used_for` in the record_summary `links` field. DROP any you cannot confidently attribute; do NOT invent new URLs:\n")
	for _, url := range links {
		clean, names := redact.Outbound(url)
		pats.addAll(names)
		b.WriteString("- ")
		b.WriteString(clean)
		b.WriteByte('\n')
	}
	return b.String()
}

// --- reflect / propose share an input shape ---

// SessionDigest is the per-session input to reflect/propose. Callers
// are expected to prefer an existing summary when available for
// token-efficient meta-analysis. If Summary is empty, FirstPrompt is
// the best the model gets to work with for that session.
type SessionDigest struct {
	ID          string
	StartedAtMs int64
	EndedAtMs   int64
	Cwd         string
	FirstPrompt string   // usually the first user_prompt in the session
	Summary     string   // optional: existing llm_outputs summary for this session
	Links       []string // optional: distinct URLs observed in the session
}

const reflectSystem = `You reflect on a developer's recent AI coding sessions to spot patterns. You MUST call the record_reflection tool exactly once. Be concrete and grounded in the material. Do not invent sessions, tools, or files. Brevity beats completeness. Every claim in task_types or frictions must cite at least one real session_id from the input.`

const reflectionToolSchema = `{
  "type": "object",
  "required": ["task_types","frictions","workflow_change"],
  "additionalProperties": false,
  "properties": {
    "task_types": {
      "type":"array",
      "minItems": 0,
      "maxItems": 3,
      "items": {
        "type":"object",
        "required":["label","session_ids"],
        "additionalProperties": false,
        "properties": {
          "label": {"type":"string","minLength":1},
          "session_ids": {"type":"array","minItems":1,"items":{"type":"string","minLength":1}}
        }
      }
    },
    "frictions": {
      "type":"array",
      "minItems": 0,
      "maxItems": 3,
      "items": {
        "type":"object",
        "required":["label","session_ids"],
        "additionalProperties": false,
        "properties": {
          "label": {"type":"string","minLength":1},
          "session_ids": {"type":"array","minItems":1,"items":{"type":"string","minLength":1}}
        }
      }
    },
    "workflow_change": {"type":"string"}
  }
}`

const reflectTemplate = `Below are %d sessions from %s to %s.

---
%s
---
`

// BuildReflect composes the meta-prompt for multi-session reflection.
// digests must be non-empty; the caller sets the window.
func BuildReflect(digests []SessionDigest, window time.Duration) (Built, error) {
	if len(digests) == 0 {
		return Built{}, fmt.Errorf("BuildReflect: no sessions")
	}
	pats := patternSet{}
	body := renderDigests(digests, pats)

	now := time.Now().UTC()
	userMsg := fmt.Sprintf(reflectTemplate,
		len(digests),
		now.Add(-window).Format(time.RFC3339),
		now.Format(time.RFC3339),
		body)

	req := llm.Request{
		System:    reflectSystem,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: userMsg}},
		MaxTokens: reflectMaxTokens,
		Tools: []llm.Tool{{
			Name:        ToolNameReflection,
			Description: "Record a structured reflection across multiple coding sessions.",
			InputSchema: json.RawMessage(reflectionToolSchema),
		}},
		ForceTool: ToolNameReflection,
	}
	return Built{Request: req, Hash: hashRequest(req), Patterns: pats.sortedSlice()}, nil
}

const proposeSystem = `You are a principal engineer reviewing a developer's AI coding sessions to propose reusable capabilities. You MUST call the record_proposal tool exactly once. Only suggest things that would have demonstrably saved time in the sessions shown. Reject generic advice. Every proposed item must cite at least one real session_id from the input as evidence.`

const proposalToolSchema = `{
  "type": "object",
  "required": ["skills","claude_md_entries","scripts"],
  "additionalProperties": false,
  "properties": {
    "skills": {
      "type":"array",
      "minItems": 0,
      "maxItems": 5,
      "items": {
        "type":"object",
        "required":["name","when_to_use","why","session_ids"],
        "additionalProperties": false,
        "properties": {
          "name": {"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
          "when_to_use": {"type":"string","minLength":1},
          "why": {"type":"string","minLength":1},
          "session_ids": {"type":"array","minItems":1,"items":{"type":"string","minLength":1}}
        }
      }
    },
    "claude_md_entries": {
      "type":"array",
      "minItems": 0,
      "maxItems": 5,
      "items": {
        "type":"object",
        "required":["rule","why","session_ids"],
        "additionalProperties": false,
        "properties": {
          "rule": {"type":"string","minLength":1},
          "why": {"type":"string","minLength":1},
          "session_ids": {"type":"array","minItems":1,"items":{"type":"string","minLength":1}}
        }
      }
    },
    "scripts": {
      "type":"array",
      "minItems": 0,
      "maxItems": 5,
      "items": {
        "type":"object",
        "required":["name","purpose","session_ids"],
        "additionalProperties": false,
        "properties": {
          "name": {"type":"string","minLength":1},
          "purpose": {"type":"string","minLength":1},
          "session_ids": {"type":"array","minItems":1,"items":{"type":"string","minLength":1}}
        }
      }
    }
  }
}`

const proposeTemplate = `Below are %d recent sessions.

---
%s
---
`

// BuildPropose composes the skills/CLAUDE.md/scripts proposal prompt.
func BuildPropose(digests []SessionDigest) (Built, error) {
	if len(digests) == 0 {
		return Built{}, fmt.Errorf("BuildPropose: no sessions")
	}
	pats := patternSet{}
	body := renderDigests(digests, pats)

	userMsg := fmt.Sprintf(proposeTemplate, len(digests), body)
	req := llm.Request{
		System:    proposeSystem,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: userMsg}},
		MaxTokens: proposeMaxTokens,
		Tools: []llm.Tool{{
			Name:        ToolNameProposal,
			Description: "Record a structured proposal of reusable capabilities derived from recent sessions.",
			InputSchema: json.RawMessage(proposalToolSchema),
		}},
		ForceTool: ToolNameProposal,
	}
	return Built{Request: req, Hash: hashRequest(req), Patterns: pats.sortedSlice()}, nil
}

// --- rendering helpers ---

// renderEvents turns an event stream into a human-ish transcript.
// Every non-empty content_text and tool_name passes through
// redact.Outbound; patterns accumulate into pats.
func renderEvents(events []store.EventView, pats patternSet) string {
	var b strings.Builder
	for _, e := range events {
		label := e.Kind
		if e.Role.Valid && e.Role.String != "" {
			label = e.Role.String + "/" + e.Kind
		}
		if e.ToolName.Valid && e.ToolName.String != "" {
			clean, names := redact.Outbound(e.ToolName.String)
			pats.addAll(names)
			label += " (" + clean + ")"
		}
		_, _ = fmt.Fprintf(&b, "[%s]\n", label)

		if e.ContentText.Valid && e.ContentText.String != "" {
			clean, names := redact.Outbound(e.ContentText.String)
			pats.addAll(names)
			b.WriteString(clean)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// renderDigests flattens session digests for reflect/propose.
// Per-session Links (when present) are inline so the model's
// annotation tool output can cite them by session_id.
func renderDigests(digests []SessionDigest, pats patternSet) string {
	var b strings.Builder
	for i, d := range digests {
		_, _ = fmt.Fprintf(&b, "## Session %d — %s\n", i+1, d.ID)
		if d.StartedAtMs > 0 && d.EndedAtMs > 0 {
			_, _ = fmt.Fprintf(&b, "Window: %s → %s\n",
				time.UnixMilli(d.StartedAtMs).UTC().Format(time.RFC3339),
				time.UnixMilli(d.EndedAtMs).UTC().Format(time.RFC3339))
		}
		if d.Cwd != "" {
			clean, names := redact.Outbound(d.Cwd)
			pats.addAll(names)
			_, _ = fmt.Fprintf(&b, "Cwd: %s\n", clean)
		}
		if d.FirstPrompt != "" {
			clean, names := redact.Outbound(d.FirstPrompt)
			pats.addAll(names)
			_, _ = fmt.Fprintf(&b, "First prompt: %s\n", clean)
		}
		if d.Summary != "" {
			clean, names := redact.Outbound(d.Summary)
			pats.addAll(names)
			_, _ = fmt.Fprintf(&b, "Prior summary:\n%s\n", clean)
		}
		if len(d.Links) > 0 {
			b.WriteString("Links observed:\n")
			for _, url := range d.Links {
				clean, names := redact.Outbound(url)
				pats.addAll(names)
				b.WriteString("- ")
				b.WriteString(clean)
				b.WriteByte('\n')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// hashRequest produces the stable cache key for Request. Includes
// system, all messages, max_tokens, and (when set) the tools +
// tool_choice. Model is NOT included — we want swapping models to
// still hit the cache when the input text is identical; callers can
// force a refresh via --force.
//
// Non-tool requests produce byte-identical hashes to the pre-tools
// version so any legacy caller that did not declare tools still hits
// the same cache rows it used to.
func hashRequest(req llm.Request) string {
	h := sha256.New()
	h.Write([]byte(req.System))
	h.Write([]byte{0})
	for _, m := range req.Messages {
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		h.Write([]byte(m.Content))
		h.Write([]byte{0})
	}
	_, _ = fmt.Fprintf(h, "%d", req.MaxTokens)
	// Tools section — only hashed when present, so the no-tools hash
	// stays identical to what we produced before tool use existed.
	for _, t := range req.Tools {
		h.Write([]byte{0xFE}) // sentinel separator
		h.Write([]byte(t.Name))
		h.Write([]byte{0})
		h.Write([]byte(t.Description))
		h.Write([]byte{0})
		h.Write(t.InputSchema)
	}
	if req.ForceTool != "" {
		h.Write([]byte{0xFD})
		h.Write([]byte(req.ForceTool))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// patternSet is a tiny sorted-deduped string collector used while
// rendering. Keeping it unexported avoids callers mutating it.
type patternSet map[string]struct{}

func (p patternSet) addAll(names []string) {
	for _, n := range names {
		p[n] = struct{}{}
	}
}

func (p patternSet) sortedSlice() []string {
	if len(p) == 0 {
		return nil
	}
	out := make([]string, 0, len(p))
	for k := range p {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
