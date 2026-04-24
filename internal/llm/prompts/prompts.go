// Package prompts composes the fixed prompts Block B's features
// (summarize, reflect, propose) send to the LLM. Each Build* function
// returns:
//
//   - the llm.Request the caller will hand to a llm.Client
//   - a deterministic prompt hash the caller can use as the
//     llm_outputs cache key
//   - the union of redact.Outbound pattern names that fired while
//     assembling the prompt, so callers can log what got scrubbed
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
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/redact"
	"github.com/toabctl/aichronicles/internal/store"
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

// --- summary ---

const summarySystem = `You summarize a single coding session between a human and an AI coding assistant. Be factual and tight. Do not invent details. If a section has no content, say "none".`

const summaryTemplate = `Session: %s
Events: %d

Produce, in this exact order:

Topic: one line.
What was done:
- bullet
- bullet
- bullet (3-5 total)
Unresolved issues:
- bullet or "none"
Key files touched:
- path or "none"

Transcript follows, oldest first:
---
%s
---
`

// BuildSummary returns the prompt for summarizing one session's
// events. The caller is expected to have already filtered out empty
// events if they wanted to; this function includes every event.
func BuildSummary(sessionID string, events []store.EventView) (Built, error) {
	if sessionID == "" {
		return Built{}, fmt.Errorf("BuildSummary: sessionID required")
	}
	if len(events) == 0 {
		return Built{}, fmt.Errorf("BuildSummary: no events for session %s", sessionID)
	}

	pats := patternSet{}
	transcript := renderEvents(events, pats)

	userMsg := fmt.Sprintf(summaryTemplate, sessionID, len(events), transcript)

	req := llm.Request{
		System:    summarySystem,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: userMsg}},
		MaxTokens: summaryMaxTokens,
	}
	return Built{Request: req, Hash: hashRequest(req), Patterns: pats.sortedSlice()}, nil
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
	FirstPrompt string // usually the first user_prompt in the session
	Summary     string // optional: existing llm_outputs summary for this session
}

const reflectSystem = `You reflect on a developer's recent AI coding sessions to spot patterns. Be concrete and grounded in the material. Do not invent sessions, tools, or files. Brevity beats completeness.`

const reflectTemplate = `Below are %d sessions from %s to %s.

Output, in this exact order:

Top 3 recurring task types (with session ids as evidence):
1. ...
2. ...
3. ...

Top 3 recurring sources of friction (with session ids as evidence):
1. ...
2. ...
3. ...

One workflow change that would most likely help:
- ...

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
	}
	return Built{Request: req, Hash: hashRequest(req), Patterns: pats.sortedSlice()}, nil
}

const proposeSystem = `You are a principal engineer reviewing a developer's AI coding sessions to propose reusable capabilities. Only suggest things that would have demonstrably saved time in the sessions shown. Reject generic advice.`

const proposeTemplate = `Below are %d recent sessions.

Propose, in this exact order:

Skills / slash-command ideas (max 5):
- name: <kebab-case>
  when to use: 1 line
  why: 1 line citing at least one session id

CLAUDE.md entries worth adding (max 5):
- the rule: 1 line
  why: 1 line citing at least one session id

Pre-built scripts worth keeping in scripts/bin/ (max 5):
- script name + 1-line purpose + citing session id

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
		b.WriteByte('\n')
	}
	return b.String()
}

// hashRequest produces the stable cache key for Request. Includes
// system, all messages, and max_tokens. Model is NOT included — we
// want swapping models to still hit the cache when the input text
// is identical; callers can force a refresh via --force.
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
