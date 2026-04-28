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

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
	"github.com/toabctl/aichronicles/pkg/llm"
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
	ToolNameSummary        = "record_summary"
	ToolNameReflection     = "record_reflection"
	ToolNameProposal       = "record_proposal"
	ToolNameProposalVerify = "record_proposal_verification"
	ToolNameSkillRevision  = "record_skill_revision"
	ToolNameInduction      = "record_induction"
	ToolNameChallenge      = "record_challenge"
	ToolNameFacts          = "record_facts"
)

// --- result types ---

// SummaryResult is the schema-validated payload of a record_summary
// tool call. Fields are always populated (empty slices/strings on
// fields the model had nothing to say about).
type SummaryResult struct {
	Topic        string                  `json:"topic"`
	WhatWasDone  []string                `json:"what_was_done"`
	Unresolved   []string                `json:"unresolved"`
	KeyFiles     []string                `json:"key_files"`
	Links        []LinkAnnotation        `json:"links"`
	Subagents    []SubagentSummary       `json:"subagents"`
	SessionLinks []SessionLinkAnnotation `json:"session_links"`
}

// SessionLinkAnnotation is one model-emitted typed link to a prior
// session. The summarize prompt is shown a shortlist of recent
// same-cwd sessions ("candidate prior sessions") and asked to emit
// one link per session it can confidently relate to the current
// one. The to_session_id MUST come from the candidate list — same
// anti-fabrication rule as URL annotations: ground the connection
// in something the model was actually shown, don't invent.
//
// kind is one of the four closed values that match the
// session_links migration's CHECK clause. rationale is a short
// (≤160 chars) line explaining the connection — surfaced verbatim
// in the web UI.
type SessionLinkAnnotation struct {
	ToSessionID string `json:"to_session_id"`
	Kind        string `json:"kind"`
	Rationale   string `json:"rationale"`
}

// SubagentSummary describes one sub-agent thread that ran in the
// session. ID matches store.events.subagent_id; Type is the role
// label when the host emitted one; Description is a one-line
// model-attributed summary of what the thread did, drawn from the
// events labelled with that subagent in the prompt transcript.
type SubagentSummary struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Description string `json:"description"`
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

// WeeklyDigestEnvelope is what callers marshal into llm_outputs.body
// for kind=reflect_weekly. The reflect result lands inside Result so
// a reader can recover both the underlying analysis and the period
// the digest covers without having to parse dates out of the prompt.
// Stable JSON shape; the Period fields are RFC3339 strings so they
// round-trip cleanly through JSON.
//
// Lives in this package (rather than internal/cli, where the digest
// command does its work) so other layers — internal/web rendering
// the /digests page, future MCP tools — can decode the persisted
// body without dragging in the cli package and tripping import
// cycles.
type WeeklyDigestEnvelope struct {
	PeriodStart string            `json:"period_start"`
	PeriodEnd   string            `json:"period_end"`
	Result      *ReflectionResult `json:"result"`
}

// ReflectionResult is the schema-validated payload of a
// record_reflection tool call.
type ReflectionResult struct {
	TaskTypes      []ReflectionTaskType `json:"task_types"`
	Frictions      []ReflectionFriction `json:"frictions"`
	WorkflowChange string               `json:"workflow_change"`
}

// ReflectionTaskType is one cluster of recurring work the model
// observed across sessions. Evidence requires ≥2 distinct sessions
// (a one-off task isn't yet a pattern), each grounded in a verbatim
// quote so the user can grep the session and verify the claim.
type ReflectionTaskType struct {
	Label     string               `json:"label"`
	Evidence  []ReflectionEvidence `json:"evidence"`
	Frequency int                  `json:"frequency"`
}

// ReflectionFriction is one recurring pain point. Same evidence
// contract as ReflectionTaskType, plus a severity hint so the
// reviewer can sort by impact: "small" = mild annoyance,
// "medium" = noticeable cost per occurrence, "large" = blocked
// progress or required workaround.
type ReflectionFriction struct {
	Label     string               `json:"label"`
	Evidence  []ReflectionEvidence `json:"evidence"`
	Frequency int                  `json:"frequency"`
	Severity  string               `json:"severity"`
}

// ReflectionEvidence has the same shape as ProposalEvidence — quote
// is a VERBATIM excerpt (≤160 chars), what_happened is the ≤1-line
// context. Kept as its own type so the two surfaces can diverge if
// reflection ever needs a different field set.
type ReflectionEvidence struct {
	SessionID    string `json:"session_id"`
	Quote        string `json:"quote"`
	WhatHappened string `json:"what_happened"`
}

// ProposalResult is the schema-validated payload of a
// record_proposal tool call. Single-shape: every proposal is a
// skill. Trigger-conditional rules collapse into the skill's
// when_to_use; helper scripts collapse into the skill's
// scripts[]. Practice-level invariants without a trigger
// (CLAUDE.md territory) are explicitly out of scope.
//
// Capped at 5 items by schema so callers can render the output
// deterministically.
type ProposalResult struct {
	Skills []ProposedSkill `json:"skills"`
}

// ProposalEvidence grounds one proposal in actual session text.
// quote is a verbatim excerpt (≤160 chars) from the session, not a
// paraphrase — paraphrase loses the property that the user can grep
// the session and verify the claim. what_happened is the ≤1-line
// context for that quote.
type ProposalEvidence struct {
	SessionID    string `json:"session_id"`
	Quote        string `json:"quote"`
	WhatHappened string `json:"what_happened"`
}

// ProposedSkill is the only artefact propose generates. Scripts
// (when present) are skill-scoped: the apply command writes them
// under <skill-dir>/scripts/<name> and the SKILL.md picks up a
// reference under "Steps". Mirrors how Claude Code's skill
// directory layout (and hermes-agent's skill_manager_tool) treat
// scripts as supporting files inside a skill, not free-floating
// helpers on PATH.
type ProposedSkill struct {
	Name                 string                `json:"name"`
	WhenToUse            string                `json:"when_to_use"`
	Why                  string                `json:"why"`
	Scripts              []ProposedSkillScript `json:"scripts,omitempty"`
	Evidence             []ProposalEvidence    `json:"evidence"`
	Frequency            int                   `json:"frequency"`
	Effort               string                `json:"effort"`
	AlternativesRejected string                `json:"alternatives_rejected"`
}

// ProposedSkillScript is one helper script associated with a
// proposed skill. Body is intentionally optional: the LLM gives us
// the purpose and (when it can ground it from the evidence) a
// starter shell body. The apply command falls back to a TODO stub
// when Body is empty.
//
// Steps, when populated, are the AWM-style parameterised action
// template — a sequence of (cmd, purpose) pairs where cmd may
// contain {placeholder} tokens that vary per invocation. apply
// materialises Steps as a runnable script with placeholder hints
// listed at the top, so the user gets a real bash file rather
// than a prose recipe. Steps and Body are mutually exclusive at
// the schema level; either populates the apply scaffold.
type ProposedSkillScript struct {
	Name         string                      `json:"name"`    // bare filename, e.g. "build-test.sh"
	Purpose      string                      `json:"purpose"` // one-line description for the script header
	Body         string                      `json:"body,omitempty"`
	Steps        []ProposedScriptStep        `json:"steps,omitempty"`
	Placeholders []ProposedScriptPlaceholder `json:"placeholders,omitempty"`
}

// ProposedScriptStep is one line of a parameterised script. cmd
// is the shell line as it would appear in the file; placeholders
// are written in {brace-token} form. purpose is the inline
// comment apply prepends so a reader knows what the line is for.
type ProposedScriptStep struct {
	Cmd     string `json:"cmd"`
	Purpose string `json:"purpose"`
}

// ProposedScriptPlaceholder documents one {brace-token} that
// appears across the steps. example is a real value the LLM saw
// in the cited evidence sessions; description is one line on what
// the user should substitute when running. apply renders these as
// a leading comment block so the first thing the reader sees is
// "to run this you need to fill in X, Y, Z".
type ProposedScriptPlaceholder struct {
	Token       string `json:"token"`       // e.g. "branch-name" (the literal between the braces)
	Description string `json:"description"` // one-line explanation
	Example     string `json:"example,omitempty"`
}

// --- summary ---

const summarySystem = `You summarize a single coding session between a human and an AI coding assistant. You MUST call the record_summary tool exactly once. Be factual and tight. Do not invent details. Do not invent URLs — only annotate links that were observed in the session. Do not invent prior sessions — only emit session_links to ids that appear in the "Possibly-related prior sessions" stanza, and only when the connection is grounded in this session's transcript or the prior session's topic. If a list section has no content, return an empty array.`

// summaryToolSchema is the JSON Schema for record_summary. Kept as a
// const so its bytes are stable; hashRequest includes these bytes
// when computing prompt_hash.
const summaryToolSchema = `{
  "type": "object",
  "required": ["topic","what_was_done","unresolved","key_files","links","subagents","session_links"],
  "additionalProperties": false,
  "properties": {
    "topic": {"type":"string","minLength":1},
    "what_was_done": {"type":"array","items":{"type":"string","minLength":1},"minItems":1,"maxItems":8},
    "unresolved": {"type":"array","items":{"type":"string","minLength":1}},
    "key_files": {
      "type":"array",
      "description": "Absolute file paths the session worked on. Prefer entries from the 'Files observed' stanza when present. For files referenced only in prose, copy the path verbatim from the transcript — never shorten, infer, or reformat. Do not invent paths.",
      "items":{"type":"string","minLength":1}
    },
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
    },
    "subagents": {
      "type":"array",
      "description": "One entry per sub-agent thread that ran. The transcript labels each event with [sa:<id>:<type>] when it belongs to a thread; populate only ids that actually appear there. Empty array if no [sa:...] labels in the transcript.",
      "items":{
        "type":"object",
        "required":["id","type","description"],
        "additionalProperties": false,
        "properties":{
          "id":{"type":"string","minLength":1},
          "type":{"type":"string"},
          "description":{"type":"string","minLength":1}
        }
      }
    },
    "session_links": {
      "type":"array",
      "maxItems": 5,
      "description": "Typed links from THIS session to prior sessions. to_session_id MUST be a full id from the 'Possibly-related prior sessions' stanza — never invent ids, never use a short prefix. Emit at most one entry per kind per target. Empty array when no candidate connects, or when the candidate stanza is absent.",
      "items":{
        "type":"object",
        "required":["to_session_id","kind","rationale"],
        "additionalProperties": false,
        "properties":{
          "to_session_id":{"type":"string","minLength":1},
          "kind":{"type":"string","enum":["builds_on","repeats_failure_of","supersedes","related"]},
          "rationale":{"type":"string","minLength":1,"maxLength":160}
        }
      }
    }
  }
}`

const summaryTemplate = `Session: %s
Events: %d
%s%s%s
Transcript follows, oldest first:
---
%s
---
`

// CandidatePriorSession is one entry in the shortlist of recent
// same-cwd sessions BuildSummary shows the model so it can emit
// session_links. Must come from store.LoadCandidatePriorSessions
// (or equivalent) — never synthesized, since the model is told to
// only emit links to ids it sees here.
type CandidatePriorSession struct {
	ID          string
	StartedAtMs int64
	EndedAtMs   int64
	Topic       string // empty when the candidate hasn't been summarized
}

// SummaryInputs bundles the optional inputs to BuildSummary that
// have grown past comfortable positional-arg territory. Required
// fields stay positional on BuildSummary; this struct collects the
// "you can pass nil" extras.
type SummaryInputs struct {
	Links             []string                // observed URLs (kind=url extractions)
	Files             []string                // observed file paths (kind=file_path)
	CandidatePriorSes []CandidatePriorSession // candidate prior sessions for session_links
}

// BuildSummary returns the prompt for summarizing one session's
// events.
//
// in.Links is the distinct URL list observed in the session
// (typically from store.LoadExtractionsForSession(kind='url')); the
// model is prompted to annotate each with a `used_for` via the
// record_summary tool, dropping any it cannot confidently attribute.
//
// in.Files is the distinct file_path list observed in the session
// (typically from store.LoadExtractionsForSession(kind='file_path'));
// it grounds the model's `key_files` output in actually-touched
// files rather than plausible-looking paths it might invent.
//
// in.CandidatePriorSes is the shortlist of recent same-cwd sessions
// (from store.LoadCandidatePriorSessions) the model is allowed to
// emit session_links to. Same anti-fabrication rule as URLs: the
// model is told to drop candidates it can't confidently relate to
// the current session rather than guess a kind.
//
// Passing nil/empty for any of the optional slices is fine — the
// corresponding stanza is omitted and the model receives an empty
// array (or whatever it surfaces from prose mentions).
func BuildSummary(sessionID string, events []store.EventView, in SummaryInputs) (Built, error) {
	if sessionID == "" {
		return Built{}, fmt.Errorf("BuildSummary: sessionID required")
	}
	if len(events) == 0 {
		return Built{}, fmt.Errorf("BuildSummary: no events for session %s", sessionID)
	}

	pats := patternSet{}
	transcript := renderEvents(events, pats)
	linksBlock := renderLinksBlock(in.Links, pats)
	filesBlock := renderFilesBlock(in.Files, pats)
	priorBlock := renderPriorSessionsBlock(in.CandidatePriorSes, pats)

	userMsg := fmt.Sprintf(summaryTemplate, sessionID, len(events), linksBlock, filesBlock, priorBlock, transcript)

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

// renderPriorSessionsBlock formats the "Possibly-related prior
// sessions" stanza, or returns "" when there are no candidates.
// The list is what BuildSummary's caller pulled from
// store.LoadCandidatePriorSessions — recent same-cwd sessions that
// ended before this one started. The model is told to drop
// candidates it can't relate, same anti-fabrication contract as
// the Links and Files stanzas.
//
// Each line carries: full id (the model MUST echo this verbatim
// in to_session_id), an absolute date in UTC for orientation, and
// the candidate's existing summary topic (or "(no summary)" so
// the model knows the topic field was absent rather than empty).
func renderPriorSessionsBlock(prior []CandidatePriorSession, pats patternSet) string {
	if len(prior) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nPossibly-related prior sessions (same cwd, ended before this one started). Emit `session_links` entries ONLY for ones that genuinely connect to this session — pick the kind that fits (builds_on / repeats_failure_of / supersedes / related). DROP any you can't ground in a specific connection; do NOT invent ids:\n")
	for _, p := range prior {
		topic := p.Topic
		if topic == "" {
			topic = "(no summary)"
		} else {
			clean, names := redact.Outbound(topic)
			pats.addAll(names)
			topic = clean
		}
		// Use ended_at when present, fall back to started_at.
		ts := p.EndedAtMs
		if ts == 0 {
			ts = p.StartedAtMs
		}
		when := time.UnixMilli(ts).UTC().Format("2006-01-02 15:04 UTC")
		b.WriteString("- ")
		b.WriteString(p.ID)
		b.WriteString("  ")
		b.WriteString(when)
		b.WriteString("  ")
		b.WriteString(topic)
		b.WriteByte('\n')
	}
	return b.String()
}

// renderFilesBlock formats the "Files observed" stanza that grounds
// key_files in actually-touched files. Returns "" when no files
// were observed via tool calls — in that case the model falls back
// to prose mentions in the transcript (per the schema description),
// which is the right behaviour for sessions where everything
// happened verbally.
//
// The list is the distinct file_path extractions for the session
// (already absolute thanks to FilePathExtractor's normalisation),
// passed verbatim. The schema description tells the model to draw
// key_files from this list when present and to use the path string
// from the transcript verbatim for any file referenced only in
// prose.
func renderFilesBlock(files []string, pats patternSet) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nFiles observed via tool calls in this session — prefer these as `key_files` entries. Use the path strings verbatim. If a file appears only in prose (user prompt or assistant text), copy that path verbatim too; do NOT shorten or reformat:\n")
	for _, p := range files {
		clean, names := redact.Outbound(p)
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
	// ShellCommands, when non-empty, lists the distinct shell
	// command lines extracted from this session (kind=shell_command
	// extractions). Surfaced to propose so the model has concrete
	// observed actions to ground action_template steps in — that's
	// the substrate for the AWM-style parameterised template
	// pattern (extract repeated subsequences, replace varying
	// values with placeholders).
	ShellCommands []string
	// Outcome, when non-nil, carries the per-session outcome cue
	// (success_likely / failure_likely / mixed / unknown) plus the
	// raw counters that derived it. Rendered as a one-line "Outcome:
	// <label> (n failures, n undos, n repeats)" cue in the digest
	// body — see renderDigests. The cue is a HEURISTIC over
	// observable signals (tool failures, git undos, consecutive
	// prompt repeats) and is not ground truth; downstream prompts
	// treat it as a prior, not a rule.
	Outcome *store.SessionOutcome
}

const reflectSystem = `You reflect on recent coding sessions to spot recurring patterns of work and recurring pain points. You MUST call the record_reflection tool exactly once.

Hard rules:

1. Every task_type and friction requires evidence from ≥2 DISTINCT sessions. One-off tasks/pains aren't patterns — drop them.
2. Each item carries 2–5 evidence entries. Each quote is a verbatim excerpt (≤160 chars) copied from a session's summary text. If a session has no summary, you may quote from its first_prompt ONLY when that first_prompt is itself substantive (≥30 chars and concrete). Short follow-ups like "do plan", "go ahead", "/loop", or "what's next?" don't ground anything — skip those sessions. Do NOT paraphrase.
3. frequency = the count of distinct session_ids in your evidence array.
4. severity (frictions only) — "small" = mild annoyance / extra step. "medium" = noticeable cost per occurrence (re-running a query, re-reading docs). "large" = blocked progress, required workaround, lost work.
5. Reject generic observations ("user wrote a lot of Go", "many sessions involved git"). Specific patterns only — name the tool, the artifact, the symptom.
6. Reject single-session insights, no matter how dramatic. A 30-hour outage that taught you something is interesting, but until it RECURS it's not a pattern worth a workflow_change.
7. workflow_change: ONE concrete sentence the user could act on this week, grounded in the same patterns you just listed. If no single change stands out, write "no single change recommended" — do NOT pad with vague advice ("communicate more", "take breaks", "iterate faster").

For a typical 25-session window, expect 2–4 task_types, 1–4 frictions, and a workflow_change (or the explicit "no single change" disclaimer). Lean toward proposing a clearly-grounded pattern even when its severity is moderate.`

const reflectionToolSchema = `{
  "type": "object",
  "required": ["task_types","frictions","workflow_change"],
  "additionalProperties": false,
  "properties": {
    "task_types": {
      "type":"array",
      "minItems": 0,
      "maxItems": 4,
      "items": {
        "type":"object",
        "required":["label","evidence","frequency"],
        "additionalProperties": false,
        "properties": {
          "label":     {"type":"string","minLength":1,"maxLength":120},
          "evidence":  {"$ref":"#/$defs/reflectEvidence"},
          "frequency": {"type":"integer","minimum":2}
        }
      }
    },
    "frictions": {
      "type":"array",
      "minItems": 0,
      "maxItems": 4,
      "items": {
        "type":"object",
        "required":["label","evidence","frequency","severity"],
        "additionalProperties": false,
        "properties": {
          "label":     {"type":"string","minLength":1,"maxLength":120},
          "evidence":  {"$ref":"#/$defs/reflectEvidence"},
          "frequency": {"type":"integer","minimum":2},
          "severity":  {"type":"string","enum":["small","medium","large"]}
        }
      }
    },
    "workflow_change": {"type":"string","minLength":1}
  },
  "$defs": {
    "reflectEvidence": {
      "type":"array",
      "minItems": 2,
      "maxItems": 5,
      "items": {
        "type":"object",
        "required":["session_id","quote","what_happened"],
        "additionalProperties": false,
        "properties": {
          "session_id":    {"type":"string","minLength":1},
          "quote":         {"type":"string","minLength":1,"maxLength":160},
          "what_happened": {"type":"string","minLength":1}
        }
      }
    }
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

const proposeSystem = `You triage recent coding sessions to propose high-leverage, repeatedly-useful skills. Every proposal is a SKILL — a Claude Code skill directory with a SKILL.md and (optionally) helper scripts. You MUST call the record_proposal tool exactly once.

Out of scope (do NOT propose):
- Standalone scripts not associated with a skill. If a recurring pattern needs a helper script, attach it to a skill via the skill's scripts[] field; the apply layer writes it under the skill's scripts/ subdir. There are no skill-less scripts.
- Practice-level invariants without a trigger condition ("always commit-message format X", "never push --force to main"). These are CLAUDE.md territory and live there already; don't repropose them as skills.
- Generic engineering advice ("write tests", "use small commits", "be careful with merges"). Same reason.
- Things that already exist as a Claude Code / CLI built-in. (You can mention them in alternatives_rejected.)

Hard rules:

1. A pattern requires evidence from ≥2 DISTINCT sessions. One-off tasks don't qualify, no matter how dramatic — drop them.
2. Each proposal carries 2–5 evidence entries. Each quote is a verbatim excerpt (≤160 chars) copied from a session's summary text. If a session has no summary, you may quote from its first_prompt ONLY when the first_prompt is itself substantive (≥30 chars and concrete — "compare libvirt against openSUSE Tumbleweed" qualifies; "do plan", "go ahead", "/loop", "what's next?" do NOT). Sessions whose only available text is a short prompt are not usable evidence — skip them. Do NOT paraphrase, but feel free to truncate inside a sentence.
3. Skill names: ≤4 words, kebab-case (matches a directory name under ~/.claude/skills/).
4. when_to_use is the trigger — lead with the condition that fires the skill ("When CI fails on a Go service…", "When deploying to staging…"). This is what would otherwise have been a separate CLAUDE.md rule; folding it in here is the canonical home.
5. scripts[] is optional. Use it when the recurring pattern includes specific commands the user types repeatedly. Each script gets a bare filename (e.g. "build-test.sh"), a one-line purpose, and optionally either a starter body OR a parameterised steps[] template. Don't list scripts[] entries that are just "run this one bash command" — those belong inline in the skill's steps. Reserve scripts[] for multi-step shell logic that benefits from being its own file.

5a. PARAMETERISED STEPS (preferred when the cited evidence sessions list "Shell commands observed"): instead of (or in addition to) a body, emit a steps[] array. Each step is a single shell line plus a one-line purpose. Replace concrete values that VARY across the cited sessions with {placeholder} tokens, kebab-case (e.g. "git checkout -b wt-{topic-slug}", "go build ./{package-path}"). Then list each token in the placeholders[] array with a description and an example value drawn from the actual sessions. This is the AWM (Agent Workflow Memory) pattern: the user can re-fill the template per task instead of editing prose. Skip steps[] entirely when the pattern is genuinely free-form (e.g. "open the file and edit it") — only use it when the underlying actions are observable shell commands you can extract from the cited sessions.
6. frequency = the count of distinct session_ids in your evidence array.
7. effort: "small" = an afternoon. "medium" = a few days, well-scoped. "large" = a project-shaped effort that probably wants its own design doc.

Skill-awareness rules (the "Skills installed" and "Skills invoked recently" sections at the top of the user message are CANONICAL — do not invent skill names):

8. If a recurring pattern overlaps an *installed* skill (same domain or trigger), do NOT propose a new skill with that name or near-duplicate name. Skip the pattern.
9. If a pattern is a clear *extension* of an installed skill (the skill exists but has a gap), you may propose the skill, but its name must be DIFFERENT and alternatives_rejected MUST cite the existing skill and explain the increment.
10. Treat "Skills invoked recently" as evidence the user already uses those skills successfully. Patterns whose work is plausibly served by an invoked skill are solved — don't repropose them. The frequency in that section reflects real usage, not aspiration.

11. Outcome cues (Outcome: success_likely / failure_likely / mixed / unknown, with optional counter tail) are HEURISTICS over observable signals — tool_failures, git_undos, consecutive prompt_repeats. Treat them as priors, not facts. A failure_likely session is NOT automatically uninteresting: a recurring failure shape across multiple sessions IS a pattern (the friction is the signal — propose a skill that prevents or short-circuits the failure). But avoid grounding a skill ONLY in failure_likely sessions when no success_likely session shows the same pattern — the user was probably stuck, not exhibiting reusable behaviour. Prefer mixed evidence (some success_likely, some failure_likely) over single-flavour evidence.

12. The "Prior proposals" stanza is the closed-loop signal: previous proposals from this system, with their applied / not-applied / used / failing state. Treat each entry as a STRONG prior:
    - APPLIED, in use, working — DO NOT repropose. If a current pattern overlaps, skip it (cite the existing skill in alternatives_rejected).
    - APPLIED but unused — the user kept the SKILL.md but never invoked it. The when_to_use trigger may be wrong. If you see new evidence for the same domain, propose a REVISION (different name, alternatives_rejected explains the increment) — do not propose the same shape again.
    - APPLIED but failing — the skill exists but trips tool_failures after load. If new evidence reveals the failure mode, you MAY propose a follow-up skill that addresses it; otherwise leave it alone.
    - NOT APPLIED — the user saw this proposal and did not act on it. Near-duplicate proposals are likely to be rejected the same way; skip the pattern.

13. The "Failure shapes observed" stanza is the contrastive half of the corpus: sessions where things went wrong (high tool_failure / git_undo / prompt_repeat counts). Treat these as CANDIDATES for prevention skills, not just things to ignore. A skill that catches a known failure mode early — "when test X fails with Y, before retrying do Z" — is as valuable as one that codifies a successful workflow. RULES:
    - Group failure-shaped sessions by dominant mode (tool_failures, git_undos, prompt_repeats). A failure mode that appears in ≥2 distinct failure-shaped sessions IS a recurring pattern worth a prevention skill.
    - Skill names should reference the failure ("recover-from-rebase-conflict", "diagnose-test-flake", "unblock-stuck-deploy") rather than be generic.
    - Evidence MUST cite the failure-shaped sessions (verbatim quotes from their summaries / first_prompts) — same anti-fabrication rule as positive evidence; do NOT invent failure modes the corpus doesn't show.
    - If a failure mode appears once, it's a one-off — do not propose a prevention skill on its strength alone.

For a typical 25-session window, expect 1–4 well-grounded skills total — not 0, not 5. Zero is acceptable only if every recurring pattern you see is already covered by an installed skill or is out of scope per the rules above. Lean toward proposing a clearly-grounded skill even when its time-saving estimate is moderate; the user wants concrete leads, not perfect ones.`

const proposalToolSchema = `{
  "type": "object",
  "required": ["skills"],
  "additionalProperties": false,
  "properties": {
    "skills": {
      "type":"array",
      "minItems": 0,
      "maxItems": 5,
      "items": { "$ref": "#/$defs/proposalSkill" }
    }
  },
  "$defs": {
    "evidence": {
      "type":"array",
      "minItems": 2,
      "maxItems": 5,
      "items": {
        "type":"object",
        "required":["session_id","quote","what_happened"],
        "additionalProperties": false,
        "properties": {
          "session_id":    {"type":"string","minLength":1},
          "quote":         {"type":"string","minLength":1,"maxLength":160},
          "what_happened": {"type":"string","minLength":1}
        }
      }
    },
    "frequency": {"type":"integer","minimum":2},
    "effort":    {"type":"string","enum":["small","medium","large"]},
    "alternatives_rejected": {"type":"string"},
    "proposalSkill": {
      "type":"object",
      "required":["name","when_to_use","why","evidence","frequency","effort","alternatives_rejected"],
      "additionalProperties": false,
      "properties": {
        "name":                  {"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
        "when_to_use":           {"type":"string","minLength":1},
        "why":                   {"type":"string","minLength":1},
        "scripts": {
          "type":"array",
          "minItems": 0,
          "maxItems": 5,
          "items": {
            "type":"object",
            "required":["name","purpose"],
            "additionalProperties": false,
            "properties": {
              "name":    {"type":"string","pattern":"^[A-Za-z0-9_.-]+$","maxLength":64},
              "purpose": {"type":"string","minLength":1,"maxLength":200},
              "body":    {"type":"string","maxLength":4000},
              "steps": {
                "type":"array",
                "description":"Parameterised action template (AWM-style). Each step is one shell line that may contain {placeholder} tokens; populate from observed shell_command extractions across the cited sessions.",
                "minItems":0,
                "maxItems":12,
                "items": {
                  "type":"object",
                  "required":["cmd","purpose"],
                  "additionalProperties": false,
                  "properties": {
                    "cmd":     {"type":"string","minLength":1,"maxLength":400},
                    "purpose": {"type":"string","minLength":1,"maxLength":160}
                  }
                }
              },
              "placeholders": {
                "type":"array",
                "description":"One entry per {brace-token} that appears in the steps. Populate the example field with a real value the user actually used in the cited sessions.",
                "minItems":0,
                "maxItems":10,
                "items": {
                  "type":"object",
                  "required":["token","description"],
                  "additionalProperties": false,
                  "properties": {
                    "token":       {"type":"string","pattern":"^[a-z][a-z0-9-]*$","maxLength":32},
                    "description": {"type":"string","minLength":1,"maxLength":160},
                    "example":     {"type":"string","maxLength":160}
                  }
                }
              }
            }
          }
        },
        "evidence":              {"$ref":"#/$defs/evidence"},
        "frequency":             {"$ref":"#/$defs/frequency"},
        "effort":                {"$ref":"#/$defs/effort"},
        "alternatives_rejected": {"$ref":"#/$defs/alternatives_rejected"}
      }
    }
  }
}`

// InstalledSkill is a SKILL.md the user already has on disk —
// global (~/.claude/skills/) or project-local
// (<project>/.claude/skills/). Source carries the origin so the
// LLM can resolve duplicate names sensibly when both layers
// define the same skill.
type InstalledSkill struct {
	Name        string
	Description string
	Source      string // "global", "project:<abs-path-to-project-root>", "plugin:<id>"
}

// InvokedSkill is one (skill_name, count) pair for skills the
// user actually loaded inside the propose window. Distinct from
// InstalledSkill: a skill can be installed but never invoked
// (stale candidate), invoked but not installed (plugin), or both.
//
// SuccessRate / FailedLoads / TotalLoads, when populated, carry
// the post-load tool_failure correlation from the same window.
// SuccessRate is in [0, 1] (1.0 = no failures fired). When the
// caller doesn't supply impact data (e.g. older callers), the
// fields stay zero-valued and the prompt template skips the
// per-skill success annotation rather than reporting a misleading
// "0% success" — that's the CLAUDE.md correctness rule applied
// to this layer.
type InvokedSkill struct {
	Name        string
	Count       int
	SuccessRate float64
	FailedLoads int
	TotalLoads  int
}

// PriorProposal is one entry in the propose-prompt stanza that
// surfaces the lifecycle of past proposals to the LLM. The LLM uses
// this to (a) avoid re-proposing skills the user already rejected
// (Applied=false), (b) avoid duplicating skills already on disk and
// in active use (Applied=true with high loads), and (c) reconsider
// the trigger conditions of skills that landed but went unused
// (Applied=true with zero post-apply loads).
//
// Closes the AWM (Agent Workflow Memory) loop: without this signal
// the propose prompt is open-loop — every run is a fresh shot,
// blind to which prior shots worked. With it, the system can
// exhibit the "self-improving" property: future proposals are
// shaped by the observed fate of past ones.
type PriorProposal struct {
	SkillName        string
	ProposedAtMs     int64
	Applied          bool
	AppliedAtMs      int64
	LoadsAfterApply  int
	FailedLoadsAfter int
	LastLoadedMs     int64
}

// FailureShapeDigest is one row of the negative-example corpus the
// propose prompt receives. Sessions surface here when their
// session_outcomes row is failure_likely. The LLM is instructed to
// consider skills that PREVENT or short-circuit the recurring
// failure modes, not just consolidate observed successful patterns —
// the contrastive half of ExpeL-style insight extraction (Zhao et
// al. 2024, arXiv:2308.10144).
type FailureShapeDigest struct {
	SessionID         string
	Cwd               string
	Title             string // summary_topic, fallback first_prompt
	ToolFailureCount  int
	GitUndoCount      int
	PromptRepeatCount int
	LastEventKind     string
}

// ProposeInputs bundles every input BuildPropose consumes. Adding
// a field here is non-breaking; callers that don't have it pass
// the zero value. The shape was introduced when propose became
// skill-aware so the signature wouldn't sprout positional args.
type ProposeInputs struct {
	Digests         []SessionDigest
	InstalledSkills []InstalledSkill
	InvokedSkills   []InvokedSkill
	// PriorProposals, when non-empty, surfaces the lifecycle of
	// every proposal the system has emitted (applied or not, with
	// post-apply usage stats for applied ones). Drives the "don't
	// repropose what we already tried" rule; renders as a stanza
	// before the per-session digest body.
	PriorProposals []PriorProposal
	// FailureShapes, when non-empty, surfaces sessions that went
	// wrong (high tool_failure / git_undo / prompt_repeat counts)
	// alongside the recurring-pattern digest corpus. The LLM is
	// instructed to consider skills that prevent these failure
	// shapes — see system prompt rule 13.
	FailureShapes []FailureShapeDigest
}

const proposeTemplate = `Below are %d recent sessions.%s%s%s%s

---
%s
---
`

// BuildPropose composes the skills/CLAUDE.md/scripts proposal prompt.
func BuildPropose(in ProposeInputs) (Built, error) {
	if len(in.Digests) == 0 {
		return Built{}, fmt.Errorf("BuildPropose: no sessions")
	}
	pats := patternSet{}
	body := renderDigests(in.Digests, pats)

	userMsg := fmt.Sprintf(proposeTemplate,
		len(in.Digests),
		renderInstalledSkills(in.InstalledSkills),
		renderInvokedSkills(in.InvokedSkills),
		renderPriorProposals(in.PriorProposals),
		renderFailureShapes(in.FailureShapes),
		body,
	)
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

// --- single-session induction (unified skill + workflow extraction) ---

// InductionResult is the schema-validated payload of a
// record_induction tool call. ONE LLM call extracts BOTH possible
// reusable artefacts from a settled session:
//
//   - Skill (Voyager-style): a high-confidence specific reusable
//     capability the user could materialise as a SKILL.md on disk
//     via `propose apply`. when_to_use names a concrete trigger
//     condition; the model is biased to NOT emit one.
//
//   - Workflow (AWM-style; Wang et al. 2024, arXiv:2409.07429): an
//     ABSTRACT procedural recipe — drop concrete URLs/IDs/file
//     paths, keep the procedure shape — that lives in the database
//     for retrieval at task-planning time, not on disk.
//
// Both fields are optional. The LLM emits zero, one, or both
// depending on what the session reveals. Most sessions emit
// neither (one-off bug fixes don't ground reusable artefacts);
// some emit just a workflow (loose recipe); rare sessions emit
// just a skill (tight specific capability); occasionally both
// (specific tactical capability AND general procedure).
//
// Replaces the previous (Skill, NoSkillFound) shape and the
// separate kind=workflow LLM call: one LLM call, two optional
// outputs, half the per-session induction cost.
type InductionResult struct {
	// Skill, if non-nil, is a Voyager-style concrete reusable
	// capability. Same shape ProposalResult.Skills carries — so
	// `propose apply --output-id=<llm_outputs.id>` consumes it
	// without translation.
	Skill *ProposedSkill `json:"skill,omitempty"`

	// Workflow, if non-nil, is an AWM-style abstract procedural
	// recipe. Lives only in the LLM-output body — there is no
	// "apply workflow to disk" path; the value is retrieval at
	// task-planning time via list_workflows / get_project_context.
	Workflow *InducedWorkflow `json:"workflow,omitempty"`

	// Rationale is a short (≤200 char) line explaining the
	// emission decision: which artefacts the model chose to emit
	// (or none), and why. e.g. "no reusable artefact — one-off
	// debug session"; "extracted both: deploy-staging skill from
	// the specific commands, plus an abstract deploy-service
	// workflow generalising over services".
	Rationale string `json:"rationale"`
}

// InducedWorkflow is the workflow-shaped artefact embedded in an
// InductionResult. Carries the AWM properties (abstract task_shape,
// {placeholder}-bearing procedure, observable preconditions and
// success_checks). Evidence is bound to the inducing session.
type InducedWorkflow struct {
	// TaskShape is the abstracted task description: ≤120 chars,
	// no concrete URLs / IDs / file paths. The test: would a
	// different user with a different project recognise their
	// task in this string?
	TaskShape string `json:"task_shape"`

	// Procedure is the ordered sequence of abstract NL actions —
	// not shell lines. {placeholder} tokens for varying values,
	// each documented in the action's placeholders[].
	Procedure []WorkflowStep `json:"procedure"`

	// Preconditions: state assumptions the procedure relies on
	// before step 1 (e.g. "git working tree clean", "failing
	// run id is known"). ≤5 entries.
	Preconditions []string `json:"preconditions"`

	// SuccessChecks: observable signals confirming the procedure
	// worked (e.g. "kubectl rollout status returns complete").
	// ≤5 entries.
	SuccessChecks []string `json:"success_checks"`

	// Evidence is the verbatim quote(s) from the inducing
	// session that ground the workflow. Same shape as
	// WorkflowEvidence retained for compatibility.
	Evidence []WorkflowEvidence `json:"evidence"`
}

// InduceFromSessionInputs carries everything BuildInduce needs:
// the single session digest (the seed of the induction) and the
// user's installed-skill set (so the model doesn't propose a
// near-duplicate of a skill that already exists).
type InduceFromSessionInputs struct {
	Digest          SessionDigest
	InstalledSkills []InstalledSkill
}

const inductionMaxTokens = 4096

const inductionSystem = `You inspect ONE coding session that just ended and decide whether it contained any reusable artefact worth saving. You MUST call the record_induction tool exactly once.

This is online induction — the trigger fires the moment a session ends, so the bar is HIGH. Most sessions yield nothing reusable. A casual "fixed a typo" session emits no skill, no workflow. The default is to emit BOTH skill AND workflow as null with rationale="no reusable artefact". Only emit a non-null artefact when you can specifically defend it against the rules below.

There are TWO kinds of artefact you can emit:

  * skill — a Voyager-style HIGH-CONFIDENCE SPECIFIC reusable capability that should land as a SKILL.md on disk. The user invokes it explicitly via the Skill tool. Has a concrete trigger condition (when_to_use), a tight scope, and ideally a parameterised steps[] template with {placeholder} tokens.

  * workflow — an AWM-style ABSTRACT PROCEDURAL recipe. Drop concrete URLs / IDs / file paths; keep the procedure shape. Lives only in the database; retrieved as guidance at task-planning time, never installed to disk. Looser confidence than a skill — the right artefact when the procedure is real but doesn't yet warrant becoming an installed capability.

Either, both, or neither may be emitted from one session.

Hard rules:

1. DEFAULT to emitting nothing. Saving an artefact that won't ever fire again is worse than saving nothing — false positives clutter the corpus and erode trust. Only emit when you can name what makes the artefact reusable BEYOND this single session.

2. Evidence comes from this ONE session. The multi-session minimum the offline propose path enforces does NOT apply here. But every emitted artefact MUST be grounded in a verbatim quote (≥30 chars for a skill, ≥1 char for a workflow's evidence entries) drawn from the session's summary or first_prompt. Filler ("go ahead", "/loop", "ok", "next") never grounds anything; emit nothing.

3. SKILL CRITERIA — emit a skill ONLY when:
   a. The session shows a concrete, repeatable trigger condition the user would name themselves ("when CI fails on a Go service", "when deploying to staging").
   b. The actions are tight enough to fit in a SKILL.md scaffold; ≤4-word kebab-case name; when_to_use leads with the trigger.
   c. No installed skill already covers the same condition (the "Skills installed" stanza is canonical; near-duplicate names OR triggers both disqualify).
   d. PREFER parameterised steps[] + placeholders[] when the underlying actions are observable shell commands.
   e. frequency=1, effort ∈ {small,medium,large}.

4. WORKFLOW CRITERIA — emit a workflow ONLY when:
   a. The session ran a recognisable PROCEDURE (sequence of high-level actions the same user, or a different user with a similar goal, would benefit from following next time).
   b. task_shape is ABSTRACTED: "deploy a Go service to staging" (good), NOT "deploy aichronicles to staging" (names the project — bad). The test: would a different user with a different project recognise their task in your task_shape?
   c. Procedure steps are NL actions, NOT shell lines. Replace varying values with {kebab-case} tokens; document each in the action's placeholders[].
   d. Preconditions and success_checks are OBSERVABLE (testable, not vibes).

5. EMIT BOTH when the session reveals a specific tactical capability (skill) AND a generalisable procedure (workflow) that aren't simply two views of the same thing. Most sessions don't qualify for both. If both fields end up describing the same kebab-case action sequence, just emit one — the more specific (skill) if the trigger is tight, otherwise the workflow.

6. Outcome cue (Outcome: success_likely / failure_likely / mixed / unknown) is a HEURISTIC. A failure_likely session biases TOWARD emitting nothing UNLESS the session reveals a clear reusable debugging trigger. A success_likely session is more likely to ground a real artefact but does not automatically warrant one. An unknown session almost always emits nothing.

Rationale (≤200 chars) explains the emission decision. Examples:
  * "no reusable artefact — session was a one-off bug fix"
  * "skill only: deploy-staging trigger is concrete; the deploy-service workflow form would be too generic against the existing skill"
  * "workflow only: recurring debug procedure, but the trigger varies too much per session to qualify as a skill"
  * "both: deploy-staging skill (specific) + an abstract deploy-service workflow (generalises over services in this project's pattern)"`

const inductionToolSchema = `{
  "type": "object",
  "required": ["rationale"],
  "additionalProperties": false,
  "properties": {
    "skill": {
      "type":"object",
      "required":["name","when_to_use","why","evidence","frequency","effort","alternatives_rejected"],
      "additionalProperties": false,
      "properties": {
        "name":                  {"type":"string","pattern":"^[a-z][a-z0-9-]*$"},
        "when_to_use":           {"type":"string","minLength":1},
        "why":                   {"type":"string","minLength":1},
        "scripts": {
          "type":"array",
          "minItems": 0,
          "maxItems": 3,
          "items": {
            "type":"object",
            "required":["name","purpose"],
            "additionalProperties": false,
            "properties": {
              "name":    {"type":"string","pattern":"^[A-Za-z0-9_.-]+$","maxLength":64},
              "purpose": {"type":"string","minLength":1,"maxLength":200},
              "body":    {"type":"string","maxLength":4000},
              "steps": {
                "type":"array",
                "minItems":0,
                "maxItems":12,
                "items": {
                  "type":"object",
                  "required":["cmd","purpose"],
                  "additionalProperties": false,
                  "properties": {
                    "cmd":     {"type":"string","minLength":1,"maxLength":400},
                    "purpose": {"type":"string","minLength":1,"maxLength":160}
                  }
                }
              },
              "placeholders": {
                "type":"array",
                "minItems":0,
                "maxItems":10,
                "items": {
                  "type":"object",
                  "required":["token","description"],
                  "additionalProperties": false,
                  "properties": {
                    "token":       {"type":"string","pattern":"^[a-z][a-z0-9-]*$","maxLength":32},
                    "description": {"type":"string","minLength":1,"maxLength":160},
                    "example":     {"type":"string","maxLength":160}
                  }
                }
              }
            }
          }
        },
        "evidence": {
          "type":"array",
          "minItems": 1,
          "maxItems": 3,
          "items": {
            "type":"object",
            "required":["session_id","quote","what_happened"],
            "additionalProperties": false,
            "properties": {
              "session_id":    {"type":"string","minLength":1},
              "quote":         {"type":"string","minLength":30,"maxLength":160},
              "what_happened": {"type":"string","minLength":1}
            }
          }
        },
        "frequency":             {"type":"integer","minimum":1,"maximum":1},
        "effort":                {"type":"string","enum":["small","medium","large"]},
        "alternatives_rejected": {"type":"string"}
      }
    },
    "workflow": {
      "type":"object",
      "required":["task_shape","procedure","preconditions","success_checks","evidence"],
      "additionalProperties": false,
      "properties": {
        "task_shape": {"type":"string","minLength":1,"maxLength":120},
        "procedure": {
          "type":"array",
          "minItems":1,
          "maxItems":12,
          "items": {
            "type":"object",
            "required":["action"],
            "additionalProperties": false,
            "properties": {
              "action": {"type":"string","minLength":1,"maxLength":240},
              "placeholders": {
                "type":"array",
                "minItems":0,
                "maxItems":6,
                "items": {
                  "type":"object",
                  "required":["token","description"],
                  "additionalProperties": false,
                  "properties": {
                    "token":       {"type":"string","pattern":"^[a-z][a-z0-9-]*$","maxLength":32},
                    "description": {"type":"string","minLength":1,"maxLength":160},
                    "example":     {"type":"string","maxLength":160}
                  }
                }
              }
            }
          }
        },
        "preconditions":  {"type":"array","minItems":0,"maxItems":5,"items":{"type":"string","minLength":1,"maxLength":200}},
        "success_checks": {"type":"array","minItems":0,"maxItems":5,"items":{"type":"string","minLength":1,"maxLength":200}},
        "evidence": {
          "type":"array",
          "minItems":1,
          "maxItems":1,
          "items": {
            "type":"object",
            "required":["session_id","quote","what_happened"],
            "additionalProperties": false,
            "properties": {
              "session_id":    {"type":"string","minLength":1},
              "quote":         {"type":"string","minLength":1,"maxLength":160},
              "what_happened": {"type":"string","minLength":1,"maxLength":240}
            }
          }
        }
      }
    },
    "rationale":      {"type":"string","minLength":1,"maxLength":200}
  }
}`

const inductionTemplate = `One session that just ended. Decide whether it contained a reusable workflow.

%sSession follows.

---
%s
---
`

// BuildInduce composes the single-session induction prompt. The
// digest is the just-ended session — typically the output of
// store.LoadRecentSessionDigests with limit=1 filtered to one id,
// already enriched with summary and shell_command extractions
// (the substrate the steps[]/placeholders[] template fields draw
// from).
func BuildInduce(in InduceFromSessionInputs) (Built, error) {
	if in.Digest.ID == "" {
		return Built{}, fmt.Errorf("BuildInduce: digest.ID required")
	}
	pats := patternSet{}
	body := renderDigests([]SessionDigest{in.Digest}, pats)
	userMsg := fmt.Sprintf(inductionTemplate,
		renderInstalledSkills(in.InstalledSkills),
		body,
	)
	req := llm.Request{
		System:    inductionSystem,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: userMsg}},
		MaxTokens: inductionMaxTokens,
		Tools: []llm.Tool{{
			Name:        ToolNameInduction,
			Description: "Record the result of online induction over one just-ended session.",
			InputSchema: json.RawMessage(inductionToolSchema),
		}},
		ForceTool: ToolNameInduction,
	}
	return Built{Request: req, Hash: hashRequest(req), Patterns: pats.sortedSlice()}, nil
}

// --- workflow shapes (used by InducedWorkflow inside InductionResult) ---

// WorkflowStep is one entry in InducedWorkflow.Procedure. NL action,
// not a shell line — the abstraction is the AWM property.
type WorkflowStep struct {
	// Action is the abstract step description. Reads as a
	// natural-language imperative ("Tag the release commit with
	// {version}", "Run the staging integration suite").
	Action string `json:"action"`

	// Placeholders documents the {brace-tokens} that appear in
	// Action — one-line description plus an example value drawn
	// from the cited session.
	Placeholders []WorkflowPlaceholder `json:"placeholders,omitempty"`
}

// WorkflowPlaceholder documents one {brace-token} from the action
// text. Same kebab-case convention as ProposedScriptPlaceholder for
// consistency across the two surfaces.
type WorkflowPlaceholder struct {
	Token       string `json:"token"`
	Description string `json:"description"`
	Example     string `json:"example,omitempty"`
}

// WorkflowEvidence pairs a session_id with a verbatim quote
// supporting the workflow's existence.
type WorkflowEvidence struct {
	SessionID    string `json:"session_id"`
	Quote        string `json:"quote"`
	WhatHappened string `json:"what_happened"`
}

// --- single-session SEMANTIC fact induction ---

// FactsResult is the schema-validated payload of a record_facts tool
// call. The LLM extracts (subject, predicate, object) triples from
// one session's content — typed project-level facts like "uses Go
// 1.26", "runs tests via go test ./...". The caller persists each
// fact into the semantic_facts table for typed retrieval.
//
// Distinct from procedural memory (skills, workflows, propose):
// facts answer "what is true?" rather than "how do I do X?". The
// retrieval surface is keyed by subject (typically a cwd) so the
// next session that opens in the same project can ground itself
// without re-discovering the build/test/deploy contract from raw
// events.
type FactsResult struct {
	// Found is the explicit "this session yielded at least one
	// project-level fact worth saving" signal. False means the
	// session was conversational / one-off and produced no
	// reusable facts.
	Found bool `json:"found"`

	// Facts are the (subject, predicate, object) triples to
	// persist. Each MUST be grounded in a verbatim quote from
	// the session — same anti-fabrication contract as workflow
	// evidence.
	Facts []InducedFact `json:"facts"`

	// Rationale is a short (≤200 char) explanation of the verdict.
	// On found=false: "session was a Q&A about Go generics — no
	// project-level facts asserted". On found=true: "extracted 4
	// build/test/deploy facts from the session".
	Rationale string `json:"rationale"`
}

// InducedFact is one (subject, predicate, object) triple emitted by
// BuildFacts. Confidence reflects the LLM's certainty after reading
// the session — high when the fact is asserted directly ("the
// project uses Go 1.26"), lower when it's inferred from a sequence
// of commands.
type InducedFact struct {
	Subject      string  `json:"subject"`
	Predicate    string  `json:"predicate"`
	Object       string  `json:"object"`
	Confidence   float64 `json:"confidence"`
	Quote        string  `json:"quote"`
	WhatHappened string  `json:"what_happened"`
}

// FactsFromSessionInputs carries everything BuildFacts needs.
// Mirrors WorkflowFromSessionInputs / InduceFromSessionInputs.
type FactsFromSessionInputs struct {
	Digest SessionDigest
}

const factsMaxTokens = 4096

const factsSystem = `You inspect ONE coding session and extract any TYPED PROJECT-LEVEL FACTS the session reveals — the kind of fact a future agent opening the same project would benefit from knowing without re-discovering it. You MUST call the record_facts tool exactly once.

A fact is a (subject, predicate, object) triple. For v1 the subject is usually the session's CWD (the project path). The predicate names the relation. The object is the value.

The recommended predicate vocabulary (use these when applicable; the schema accepts free-form predicates but stable retrieval depends on stable names):

  - uses_language_version       e.g. object="Go 1.26"
  - runs_tests_via              e.g. object="go test ./..."
  - runs_build_via              e.g. object="go build ./..."
  - runs_lint_via               e.g. object="golangci-lint run ./..."
  - deploys_to                  e.g. object="staging via systemd timer"
  - uses_dependency             e.g. object="modernc.org/sqlite"
  - key_directory               e.g. object="internal/store"
  - git_branch_convention       e.g. object="feature branches off main"
  - commit_convention           e.g. object="conventional commits"
  - documentation_at            e.g. object="docs/explanation/threat-model.md"
  - requires_setup_step         e.g. object="run aichronicles setup claude-code first"
  - requires_environment        e.g. object="ANTHROPIC_API_KEY"
  - primary_language            e.g. object="Go"
  - build_artefact_location     e.g. object="./bin/"
  - runs_via_command            (catchall when no specific predicate fits)

Hard rules:

1. Default to found=false. Sessions that don't establish anything reusable about a project — Q&A about generic programming, debugging-sympathy chats, single-shot bug fixes that don't reveal contract — produce zero facts. Empty facts[] array with found=false is correct.

2. Every fact MUST be grounded in a verbatim quote from the session. The quote (≤160 chars) is the substrate the user can grep to verify. Don't paraphrase. Don't synthesise from inference alone — if the session never names the test command, do NOT invent it from a build_command observation.

3. Use the documented predicate vocabulary when applicable. If your fact doesn't fit a documented predicate, use a kebab-case-with-underscores name following the same shape; flag the choice via what_happened so the maintainer can decide whether to add it to the canonical list.

4. Subject is the session's cwd (the path where the work happened) for project-level facts. If the session has no cwd, omit the fact rather than guess — facts without an anchor don't retrieve.

5. Confidence in [0, 1]: 1.0 when the session text directly asserts the fact ("we use Go 1.26"); 0.7-0.9 when the fact is observed indirectly (a go.mod line shown in tool output); below 0.7 the fact is too speculative — drop it.

6. ONE fact per (subject, predicate, object) triple. If the session mentions the same triple twice, emit one entry with the strongest evidence quote.

7. Rationale ≤200 chars. On found=false explain why ("session was a generics Q&A; no project-level facts established"). On found=true name the broad shape ("extracted 4 build/test/deploy facts").

The point of this layer: a future agent opening the same cwd can retrieve "what do I know about this project?" without scanning raw events. Optimise for facts that survive across sessions — contracts and conventions, not session-specific events.`

const factsToolSchema = `{
  "type": "object",
  "required": ["found", "facts", "rationale"],
  "additionalProperties": false,
  "properties": {
    "found": {"type": "boolean"},
    "facts": {
      "type": "array",
      "minItems": 0,
      "maxItems": 12,
      "items": {
        "type": "object",
        "required": ["subject", "predicate", "object", "confidence", "quote", "what_happened"],
        "additionalProperties": false,
        "properties": {
          "subject":       {"type": "string", "minLength": 1, "maxLength": 400},
          "predicate":     {"type": "string", "pattern": "^[a-z][a-z0-9_]*$", "maxLength": 64},
          "object":        {"type": "string", "minLength": 1, "maxLength": 400},
          "confidence":    {"type": "number", "minimum": 0.7, "maximum": 1.0},
          "quote":         {"type": "string", "minLength": 1, "maxLength": 160},
          "what_happened": {"type": "string", "minLength": 1, "maxLength": 240}
        }
      }
    },
    "rationale": {"type": "string", "minLength": 1, "maxLength": 200}
  }
}`

const factsTemplate = `One session. Extract any typed project-level facts it reveals.

Session follows.

---
%s
---
`

// BuildFacts composes the single-session semantic-facts induction
// prompt. Same single-session shape as BuildWorkflow; the system
// prompt and tool schema target (subject, predicate, object) facts
// rather than abstract workflows.
func BuildFacts(in FactsFromSessionInputs) (Built, error) {
	if in.Digest.ID == "" {
		return Built{}, fmt.Errorf("BuildFacts: digest.ID required")
	}
	pats := patternSet{}
	body := renderDigests([]SessionDigest{in.Digest}, pats)
	userMsg := fmt.Sprintf(factsTemplate, body)
	req := llm.Request{
		System:    factsSystem,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: userMsg}},
		MaxTokens: factsMaxTokens,
		Tools: []llm.Tool{{
			Name:        ToolNameFacts,
			Description: "Record typed project-level facts induced from one session.",
			InputSchema: json.RawMessage(factsToolSchema),
		}},
		ForceTool: ToolNameFacts,
	}
	return Built{Request: req, Hash: hashRequest(req), Patterns: pats.sortedSlice()}, nil
}

// --- self-generated curriculum (Voyager-style challenge mode) ---

// ChallengeResult is the schema-validated payload of a
// record_challenge tool call. Forward-looking counterpart to
// ProposalResult: where propose looks back ("what patterns do
// these sessions reveal?"), challenge looks forward ("what would
// be a worthwhile next problem given the user's current state?").
//
// Voyager (Wang et al., 2023) earns its capability gains via an
// "automatic curriculum" that picks the agent's next task based
// on what's been mastered and what's unexplored. aichronicles'
// analog: given the user's installed skills, recent successes,
// and open threads, propose the next problem to tackle.
//
// Capped at 3 challenges per call so the output stays scannable
// — one strong suggestion is more useful than five lukewarm ones.
type ChallengeResult struct {
	Challenges []Challenge `json:"challenges"`
}

// Challenge is one proposed next problem. Distinct from
// ProposedSkill — a skill is something the user has DONE that
// could be reused; a challenge is something the user HASN'T DONE
// that would be worth learning. The output shape reflects that:
// no `evidence` from past sessions (the point is novelty), but a
// `grounded_in` hook that ties the challenge to observed gaps
// or open threads so the prompt can't drift into pure invention.
type Challenge struct {
	// Title is a kebab-case label (≤4 words). Same convention as
	// skill names so a challenge that graduates into an attempted
	// skill keeps its identity across the propose/challenge
	// boundary.
	Title string `json:"title"`

	// Problem is a 1–3 sentence concrete description of what
	// the user would do. Specific enough that the user can read
	// it and immediately know whether to act on it — not "improve
	// observability" but "wire structured logs through the daemon
	// so /v1/ingest emits one slog line per accepted envelope".
	Problem string `json:"problem"`

	// Why is the rationale: why is this worth tackling NOW given
	// the user's current state? Grounds the suggestion in either
	// (a) an observed gap (no installed skill covers a pattern
	// that keeps recurring) or (b) an open thread (an unresolved
	// item that would benefit from a focused follow-up).
	Why string `json:"why"`

	// GroundedIn is a list of session ids and/or installed-skill
	// names the challenge is anchored to. Anti-fabrication:
	// the model is instructed to drop a challenge it can't
	// ground in the canonical lists shown in the prompt, rather
	// than fabricate a connection.
	GroundedIn []string `json:"grounded_in"`

	// Effort: "small" = an afternoon. "medium" = a few days,
	// well-scoped. "large" = a project-shaped effort that probably
	// wants its own design doc. Same scale as ProposedSkill so the
	// reader's calibration carries.
	Effort string `json:"effort"`

	// SuccessLooksLike is the observable outcome that would mark
	// this challenge as "done". Not a checklist; one short line
	// that gives the user a clear stopping criterion ("the
	// daemon's /v1/ingest log line lands in journalctl with the
	// envelope's content_hash").
	SuccessLooksLike string `json:"success_looks_like"`
}

// ChallengeInputs carries the same recent-session / installed-
// skills / invoked-skills bundle as ProposeInputs, plus the
// distinct-cwd unresolved-items list (when supplied) so the
// model can ground a "follow up on X" challenge in actual open
// threads rather than confabulate.
type ChallengeInputs struct {
	Digests         []SessionDigest
	InstalledSkills []InstalledSkill
	InvokedSkills   []InvokedSkill
	// Unresolved, when non-empty, lists open items from prior
	// sessions in the user's recent cwd. Surfaced to the model
	// as a "Open threads" stanza — the model is told to prefer
	// challenges that build on these vs. ones invented from
	// scratch.
	Unresolved []UnresolvedItemForChallenge
}

// UnresolvedItemForChallenge is the prompt-side shape of an
// open-thread reference. Mirrors store.UnresolvedItem one-to-one
// (kept local so the prompts package doesn't import store —
// layering stays one-way: cli + web import prompts and store;
// prompts imports neither).
type UnresolvedItemForChallenge struct {
	SessionID    string
	SessionShort string
	Topic        string
	Item         string
}

const challengeMaxTokens = 4096

const challengeSystem = `You propose the user's NEXT worthwhile problem. The user has recent sessions, an installed skill set, and a list of open threads. Pick 1–3 problems they should tackle next that would meaningfully expand what they can do, and call the record_challenge tool exactly once.

This is forward-looking: NOT "what patterns do I see in past sessions" (that's record_proposal). NOT "what's broken" (that's record_reflection). It's "given the current state, what's the next worthwhile thing".

Hard rules:

1. Every challenge MUST be grounded in either (a) a specific open thread from the "Open threads" stanza, OR (b) an observed capability gap — a recurring pattern in the digests that no installed skill covers. Drop challenges you can't ground; do NOT invent a problem to fill the slot.

2. grounded_in[] MUST cite the canonical anchor: a session_id from the digests OR an installed-skill name OR a session_id from the open threads. Empty grounded_in = the challenge is fabricated; the schema rejects it (minItems:1).

3. Avoid generic engineering advice ("write more tests", "improve performance", "add monitoring"). Specific challenges only — name the artefact, the change, and the observable outcome. "Wire structured logs through internal/daemon" qualifies; "improve observability" doesn't.

4. Skip challenges already covered by an installed skill. The "Skills installed" stanza is canonical.

5. success_looks_like is ONE short line — a specific observable outcome the user can check against. "The daemon's /v1/ingest log line lands in journalctl with the envelope's content_hash" qualifies; "logging works" doesn't.

6. Effort scale: "small" = an afternoon. "medium" = a few days, well-scoped. "large" = a project, probably wants its own design doc. Lean small/medium — a stack of three small challenges is more useful than one large one.

7. Title is ≤4 words, kebab-case (same convention as skill names — a challenge that gets done turns into a skill candidate, the names should travel).

For a typical input, expect 1–3 challenges. Zero is acceptable when nothing in the input grounds a worthwhile problem — explicit empty array is better than padded fluff.`

const challengeToolSchema = `{
  "type": "object",
  "required": ["challenges"],
  "additionalProperties": false,
  "properties": {
    "challenges": {
      "type":"array",
      "minItems": 0,
      "maxItems": 3,
      "items": {
        "type":"object",
        "required":["title","problem","why","grounded_in","effort","success_looks_like"],
        "additionalProperties": false,
        "properties": {
          "title":              {"type":"string","pattern":"^[a-z][a-z0-9-]*$","maxLength":48},
          "problem":            {"type":"string","minLength":20,"maxLength":600},
          "why":                {"type":"string","minLength":20,"maxLength":400},
          "grounded_in":        {"type":"array","minItems":1,"maxItems":5,"items":{"type":"string","minLength":1}},
          "effort":             {"type":"string","enum":["small","medium","large"]},
          "success_looks_like": {"type":"string","minLength":10,"maxLength":200}
        }
      }
    }
  }
}`

const challengeTemplate = `Recent sessions: %d.%s%s%s

---
%s
---
`

// BuildChallenge composes the curriculum / next-problem prompt.
// Mirrors BuildPropose's input shape but adds the unresolved-items
// stanza and uses the challenge-specific system prompt + schema.
func BuildChallenge(in ChallengeInputs) (Built, error) {
	if len(in.Digests) == 0 {
		return Built{}, fmt.Errorf("BuildChallenge: no sessions")
	}
	pats := patternSet{}
	body := renderDigests(in.Digests, pats)

	userMsg := fmt.Sprintf(challengeTemplate,
		len(in.Digests),
		renderInstalledSkills(in.InstalledSkills),
		renderInvokedSkills(in.InvokedSkills),
		renderUnresolvedStanza(in.Unresolved, pats),
		body,
	)
	req := llm.Request{
		System:    challengeSystem,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: userMsg}},
		MaxTokens: challengeMaxTokens,
		Tools: []llm.Tool{{
			Name:        ToolNameChallenge,
			Description: "Record 1–3 forward-looking problems the user should tackle next, grounded in their current state.",
			InputSchema: json.RawMessage(challengeToolSchema),
		}},
		ForceTool: ToolNameChallenge,
	}
	return Built{Request: req, Hash: hashRequest(req), Patterns: pats.sortedSlice()}, nil
}

// renderUnresolvedStanza formats the "Open threads" stanza for the
// challenge prompt, or returns "" when there are no items.
// Anti-fabrication contract mirrors the Links / Files stanzas in
// BuildSummary: model is told to ground a "follow up on X"
// challenge in entries from this stanza rather than invent one.
func renderUnresolvedStanza(items []UnresolvedItemForChallenge, pats patternSet) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nOpen threads observed in recent sessions — prefer challenges that BUILD ON these (cite the session_id in grounded_in[]) over challenges invented from scratch:\n")
	for _, it := range items {
		clean, names := redact.Outbound(it.Item)
		pats.addAll(names)
		short := it.SessionShort
		if short == "" && len(it.SessionID) >= 8 {
			short = it.SessionID[:8]
		}
		topic := it.Topic
		if topic == "" {
			topic = "(no summary topic)"
		}
		b.WriteString("- [")
		b.WriteString(short)
		b.WriteString("] ")
		b.WriteString(topic)
		b.WriteString(" — ")
		b.WriteString(clean)
		b.WriteByte('\n')
	}
	return b.String()
}

// --- propose verification (Voyager-style critic gate) ---

// ProposalVerification is the schema-validated payload of a
// record_proposal_verification tool call. The critic decides
// whether `propose apply` should proceed; on go_ahead=false the
// CLI refuses to write and surfaces concern + recommendation.
//
// "Severity" gives the user a single axis to sort concerns by
// when the critic returns multiple in one run (currently we only
// verify one skill at a time, but keeping severity per-decision
// makes batch-verification trivial later).
type ProposalVerification struct {
	GoAhead        bool   `json:"go_ahead"`
	Concern        string `json:"concern"`
	Severity       string `json:"severity"`
	Recommendation string `json:"recommendation"`
}

// VerifyProposalInputs carries everything the critic needs to
// decide. Keep it tight: the critic is a small focused call, not
// a re-run of propose. We hand it ONE skill and the cited
// evidence + the installed skills, and ask one yes/no.
type VerifyProposalInputs struct {
	Skill           ProposedSkill
	InstalledSkills []InstalledSkill
	EvidenceDigests []SessionDigest // sessions cited as evidence
}

const verifyProposalMaxTokens = 1024

const verifyProposalSystem = `You are a strict critic deciding whether a proposed Claude Code skill should be installed to ~/.claude/skills/. You MUST call the record_proposal_verification tool exactly once.

Refuse (go_ahead=false) when ANY of:

1. Near-duplicate of an already-installed skill — same trigger condition, same purpose. Different name doesn't matter; if the user is already covered, refuse.
2. Evidence is too thin to ground the trigger condition — fewer than 2 distinct sessions of clear, on-topic evidence; or evidence quotes that are filler ("go ahead", "/loop", "what's next?") rather than concrete task descriptions.
3. The when_to_use is generic enough that the skill would fire on every session ("when working on code", "when debugging") — Claude Code skills only earn their cost when they fire SELECTIVELY.
4. The proposed steps would actively mislead — e.g. a "use git rebase -i to fix the commits" steps section when the cited sessions never actually used rebase.

Approve (go_ahead=true) when:

- Trigger condition is concrete and observable (not "when X is hard" but "when the user runs aichronicles propose and the output is too verbose to scan").
- Cited evidence shows the same problem in 2+ distinct sessions, with concrete quotes.
- No installed skill already covers it.
- The proposed steps are grounded in what actually happened in the sessions, not invented.

Severity scale (when refusing):

- "low" — proposal is fine but borderline; would benefit from another evidence session or tighter when_to_use.
- "medium" — meaningful problem (duplicate of installed, weak evidence) — fix before applying.
- "high" — actively wrong (would mislead the agent, fabricated steps) — do not apply.

Recommendation is one short sentence the user can act on: "tighten the when_to_use to 'X'", "merge with installed skill 'Y'", "drop — only one session of evidence", etc. Empty when go_ahead=true.`

const verifyProposalToolSchema = `{
  "type": "object",
  "required": ["go_ahead", "concern", "severity", "recommendation"],
  "additionalProperties": false,
  "properties": {
    "go_ahead": {
      "type": "boolean",
      "description": "true to allow propose apply to write the SKILL.md; false to refuse."
    },
    "concern": {
      "type": "string",
      "description": "When go_ahead=false: one short paragraph explaining the issue (which rule was triggered + why this proposal is the offender). Empty when go_ahead=true."
    },
    "severity": {
      "type": "string",
      "enum": ["low", "medium", "high", "none"],
      "description": "How blocking the concern is. 'none' when go_ahead=true."
    },
    "recommendation": {
      "type": "string",
      "description": "When go_ahead=false: ONE concrete sentence the user can act on. Empty when go_ahead=true."
    }
  }
}`

const verifyProposalTemplate = `Decide whether to apply this proposed skill.

PROPOSED SKILL:
name: %s
when_to_use: %s
why: %s
frequency: %d
effort: %s

EVIDENCE (sessions cited by the proposal):
%s

INSTALLED SKILLS (already on disk; near-duplicates trigger refusal):
%s

Call record_proposal_verification with your decision.`

// BuildVerifyProposal composes the critic prompt that gates
// `propose apply`. Returns a Built — caller threads through
// runCachedLLM the same way summarize / reflect / propose do, so
// repeated runs against the same proposal hit the cache for free.
func BuildVerifyProposal(in VerifyProposalInputs) (Built, error) {
	if in.Skill.Name == "" {
		return Built{}, fmt.Errorf("BuildVerifyProposal: skill name is required")
	}
	pats := patternSet{}

	// Render evidence as a compact list — one bullet per cited
	// session, with the verbatim quote + what_happened so the
	// critic can verify "is this real evidence or filler".
	var evidence strings.Builder
	for _, ev := range in.Skill.Evidence {
		short := ev.SessionID
		if len(short) > 8 {
			short = short[:8]
		}
		quote, qpats := redact.Outbound(ev.Quote)
		pats.addAll(qpats)
		ctxText, cpats := redact.Outbound(ev.WhatHappened)
		pats.addAll(cpats)
		fmt.Fprintf(&evidence, "  - [%s] %q (%s)\n", short, quote, ctxText)
	}
	if evidence.Len() == 0 {
		evidence.WriteString("  (no evidence — refuse if you take rule 2 seriously)\n")
	}

	whenClean, wpats := redact.Outbound(in.Skill.WhenToUse)
	pats.addAll(wpats)
	whyClean, wypats := redact.Outbound(in.Skill.Why)
	pats.addAll(wypats)

	userMsg := fmt.Sprintf(verifyProposalTemplate,
		in.Skill.Name, whenClean, whyClean,
		in.Skill.Frequency, in.Skill.Effort,
		strings.TrimRight(evidence.String(), "\n"),
		renderInstalledSkills(in.InstalledSkills),
	)

	req := llm.Request{
		System:    verifyProposalSystem,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: userMsg}},
		MaxTokens: verifyProposalMaxTokens,
		Tools: []llm.Tool{{
			Name:        ToolNameProposalVerify,
			Description: "Record the critic's decision on whether to install a proposed Claude Code skill.",
			InputSchema: json.RawMessage(verifyProposalToolSchema),
		}},
		ForceTool: ToolNameProposalVerify,
	}
	return Built{Request: req, Hash: hashRequest(req), Patterns: pats.sortedSlice()}, nil
}

// --- skill revision (TDS-critique gap: act on stale flags) ---

// SkillRevision is the schema-validated payload of a
// record_skill_revision tool call. revised_body is the new SKILL.md
// content (frontmatter PRESERVED — the LLM rewrites everything
// after the closing ---). rationale is one short paragraph the user
// can read to understand WHY the revision differs from the original;
// no_change_needed=true means the model couldn't find a clear
// improvement and the failures look unrelated to the SKILL itself.
type SkillRevision struct {
	RevisedBody    string `json:"revised_body"`
	Rationale      string `json:"rationale"`
	NoChangeNeeded bool   `json:"no_change_needed"`
}

// SkillFailureExample is one cited failure for the evolve prompt.
// SessionID + ts let the model attribute. ContextSnippet is a tight
// excerpt around the failure (a few hundred chars on each side) so
// the revision is grounded in what actually went wrong, not just
// "this skill correlates with failures."
type SkillFailureExample struct {
	SessionID      string
	TsMs           int64
	ContextSnippet string
}

// EvolveSkillInputs bundles the SKILL.md being revised + the
// failure evidence the staleness detector flagged.
type EvolveSkillInputs struct {
	SkillName       string
	CurrentSkillMd  string
	FailureExamples []SkillFailureExample
}

const evolveSkillMaxTokens = 4096

const evolveSkillSystem = `You revise an existing Claude Code skill (SKILL.md) so its instructions stop correlating with tool failures. You MUST call the record_skill_revision tool exactly once.

Input shape:

- The CURRENT SKILL.md the user has installed.
- A list of FAILURE EXAMPLES — sessions where loading this skill was followed by a tool_failure event within ~10 minutes. Each example carries the verbatim tool failure text + nearby context.

Your job is ONE of two outcomes:

1. revised_body=<new SKILL.md>, no_change_needed=false. Revise the SKILL by:
   - Tightening the when_to_use to exclude the failing case (the skill was firing too broadly).
   - Adding a Pitfalls / Gotchas section listing the specific failure modes you see in the evidence.
   - Fixing concrete instruction errors (a wrong flag, an outdated path, a step that no longer works).
   - PRESERVING the YAML frontmatter VERBATIM — copy lines from --- to --- exactly. Do NOT rename the skill, do NOT change its description without a strong reason. The SKILL's identity stays.
   - rationale = one short paragraph naming the specific failures you addressed.

2. no_change_needed=true, revised_body="". Use this when:
   - Failures look UNRELATED to the SKILL (e.g. a generic ENOENT that any skill would hit).
   - Evidence is too thin (<2 distinct sessions) to ground a revision.
   - The SKILL is already specific and the failures look like genuine user error.
   - rationale = one short paragraph explaining why no revision is warranted.

Hard rules:
- Do NOT reinvent the skill from scratch. Edits should be surgical: "tighten this trigger, add a Pitfalls bullet, fix this step." Big rewrites usually mean the skill's CONCEPT is wrong, which is propose's territory, not evolve's.
- Do NOT add fictional steps. Every claim about "do X to fix Y" must trace back to evidence in the failure examples.
- Keep the revised SKILL.md under 4000 characters total — Claude Code's skill loader has practical limits and longer skills are less likely to be loaded by the host.`

const evolveSkillToolSchema = `{
  "type": "object",
  "required": ["revised_body", "rationale", "no_change_needed"],
  "additionalProperties": false,
  "properties": {
    "revised_body": {
      "type": "string",
      "description": "The full new SKILL.md text including the unchanged YAML frontmatter, OR an empty string when no_change_needed=true.",
      "maxLength": 4000
    },
    "rationale": {
      "type": "string",
      "description": "One short paragraph (≤400 chars) explaining what changed and why, grounded in the failure examples.",
      "minLength": 1,
      "maxLength": 400
    },
    "no_change_needed": {
      "type": "boolean",
      "description": "true when the failures don't ground a revision (unrelated, evidence too thin, etc.). When true, revised_body must be empty."
    }
  }
}`

const evolveSkillTemplate = `Revise this Claude Code skill so its instructions stop correlating with tool failures.

SKILL NAME: %s

CURRENT SKILL.md (preserve the YAML frontmatter VERBATIM):
---
%s
---

FAILURE EXAMPLES (%d sessions, each with skill_load → tool_failure within ~10 minutes):
%s

Call record_skill_revision with your decision.`

// BuildEvolveSkill composes the revision prompt. Returns Built so
// the caller threads through runCachedLLM the same way every other
// LLM-output kind does — a re-run on identical inputs hits the
// cache.
func BuildEvolveSkill(in EvolveSkillInputs) (Built, error) {
	if in.SkillName == "" {
		return Built{}, fmt.Errorf("BuildEvolveSkill: skill name required")
	}
	if strings.TrimSpace(in.CurrentSkillMd) == "" {
		return Built{}, fmt.Errorf("BuildEvolveSkill: current SKILL.md required")
	}
	pats := patternSet{}
	skillCleaned, snames := redact.Outbound(in.CurrentSkillMd)
	pats.addAll(snames)

	var examples strings.Builder
	for i, ex := range in.FailureExamples {
		short := ex.SessionID
		if len(short) > 8 {
			short = short[:8]
		}
		ctxClean, cnames := redact.Outbound(ex.ContextSnippet)
		pats.addAll(cnames)
		_, _ = fmt.Fprintf(&examples, "\n[%d] session %s, %s:\n%s\n",
			i+1, short,
			time.UnixMilli(ex.TsMs).UTC().Format("2006-01-02 15:04 UTC"),
			ctxClean)
	}
	if examples.Len() == 0 {
		examples.WriteString("(no failure examples — recommend no_change_needed=true)")
	}

	userMsg := fmt.Sprintf(evolveSkillTemplate,
		in.SkillName, skillCleaned, len(in.FailureExamples), examples.String(),
	)

	req := llm.Request{
		System:    evolveSkillSystem,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: userMsg}},
		MaxTokens: evolveSkillMaxTokens,
		Tools: []llm.Tool{{
			Name:        ToolNameSkillRevision,
			Description: "Record a revised SKILL.md that addresses the cited failures, OR record that no revision is warranted.",
			InputSchema: json.RawMessage(evolveSkillToolSchema),
		}},
		ForceTool: ToolNameSkillRevision,
	}
	return Built{Request: req, Hash: hashRequest(req), Patterns: pats.sortedSlice()}, nil
}

// SearchHit is one row supplied to BuildSearchSummary. Every field
// is grounding context the LLM is allowed to cite verbatim. We
// pass session_id and ts_source_ms so the model can attribute each
// claim back to a concrete row the user could click through to.
type SearchHit struct {
	SessionID  string
	Kind       string
	Cwd        string
	TsSourceMs int64
	Snippet    string
}

const searchSummarySystem = `You answer the user's search query using ONLY the provided hits as ground truth. Anti-fabrication rules:

1. Every statement you make must be supported by at least one hit. If the hits don't answer the query, say so plainly — do NOT pad with general knowledge.
2. Cite session_ids inline using the form [session=<short-id>] after each claim. Use the FIRST 8 characters of the session_id; the user can expand to the full id from there.
3. Quote sparingly. Synthesise the answer in your own words; reach for a verbatim quote only when paraphrasing would lose precision.
4. Keep it tight: 2–5 sentences. The user is using this to recall what happened across recent work, not to read an essay.
5. If multiple hits agree, cite multiple sessions: [session=abc12345, session=def67890]. If they disagree, say so explicitly and cite both sides.

Respond as plain prose — no headers, no bullet lists, no markdown.`

// BuildSearchSummary composes the prompt for `aichronicles search
// --summarize`. Each hit is rendered as a labelled block carrying
// session_id, timestamp, kind, cwd, and snippet — enough grounding
// for the LLM to attribute claims without seeing the full
// transcript.
func BuildSearchSummary(query string, hits []SearchHit, maxTokens int) (Built, error) {
	if query == "" {
		return Built{}, fmt.Errorf("BuildSearchSummary: empty query")
	}
	if len(hits) == 0 {
		return Built{}, fmt.Errorf("BuildSearchSummary: no hits")
	}
	if maxTokens <= 0 {
		maxTokens = 512
	}

	pats := patternSet{}

	// Scrub query just like content — secrets the user might have
	// accidentally pasted into a search box must not echo into the
	// LLM call.
	cleanQuery, names := redact.Outbound(query)
	pats.addAll(names)

	var b strings.Builder
	fmt.Fprintf(&b, "Query: %s\n\nHits (%d):\n", cleanQuery, len(hits))
	for _, h := range hits {
		shortSess := h.SessionID
		if len(shortSess) > 8 {
			shortSess = shortSess[:8]
		}
		when := time.UnixMilli(h.TsSourceMs).UTC().Format(time.RFC3339)
		cleanSnip, ns := redact.Outbound(h.Snippet)
		pats.addAll(ns)
		cleanCwd, ns2 := redact.Outbound(h.Cwd)
		pats.addAll(ns2)
		fmt.Fprintf(&b, "\n[session=%s] [kind=%s] [when=%s] [cwd=%s]\n%s\n",
			shortSess, h.Kind, when, cleanCwd, cleanSnip)
	}

	req := llm.Request{
		System:    searchSummarySystem,
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: b.String()}},
		MaxTokens: maxTokens,
	}
	return Built{Request: req, Hash: hashRequest(req), Patterns: pats.sortedSlice()}, nil
}

// renderInstalledSkills produces the "Skills installed:" block
// inserted before the sessions in the propose user message. Empty
// list → empty string (no header), so a fresh user with no
// installed skills doesn't get a misleading section.
func renderInstalledSkills(skills []InstalledSkill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nSkills installed (the user has these — do not propose new skills with overlapping names):\n")
	for _, s := range skills {
		_, _ = fmt.Fprintf(&b, "- %s [%s]: %s\n", s.Name, s.Source, s.Description)
	}
	return b.String()
}

// renderInvokedSkills produces the "Skills invoked recently:" block.
// Counts reflect actual skill_load extractions in the propose
// window — patterns whose work the user is already serving via
// these skills are solved and should not be re-proposed.
//
// When a row carries impact data (TotalLoads > 0), the line also
// shows a success-rate annotation so the model sees which skills
// are actually working vs. which are correlated with tool_failure.
// A low-success skill is a different signal than a high-success
// one: it might want REVISION (the skill exists but its
// instructions are stale) rather than displacement by a brand-new
// proposal. Rows without impact data render the bare count, same
// as before.
func renderInvokedSkills(skills []InvokedSkill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nSkills invoked recently (count = times loaded in the window — these are working for the user; a low success rate suggests the existing skill needs a revision rather than displacement by a brand-new proposal):\n")
	for _, s := range skills {
		if s.TotalLoads > 0 {
			pct := int(s.SuccessRate * 100)
			_, _ = fmt.Fprintf(&b, "- %s × %d  (success: %d%%, %d/%d loads followed by tool_failure)\n",
				s.Name, s.Count, pct, s.FailedLoads, s.TotalLoads)
		} else {
			_, _ = fmt.Fprintf(&b, "- %s × %d\n", s.Name, s.Count)
		}
	}
	return b.String()
}

// --- rendering helpers ---

// renderEvents turns an event stream into a human-ish transcript.
// Every non-empty content_text and tool_name passes through
// redact.Outbound; patterns accumulate into pats.
//
// Events with a non-NULL subagent_id pick up an [sa:<id>:<type>]
// prefix on their label so the model can attribute work to a
// specific thread when it fills in SummaryResult.Subagents. The
// ID is the load-bearing identifier; type may be empty for hosts
// that don't emit one and is rendered as "?" in that case so the
// label shape stays uniform.
// Per-kind transcript caps. Tool_result bodies are the bulk-size
// offender (file dumps from Read, grep output, full command stdout)
// AND the least useful for a *summary* — the assistant's next turn
// already digested them into its decision. Head-truncate hard for
// those; spend the budget on the high-signal kinds (user prompts,
// assistant reasoning, errors).
//
// All values are in runes; 1 rune ≈ 0.5 tokens for English-ish
// text, ~1 token for code-dense content. Multiply by ~0.7 for a
// rough token estimate.
const (
	maxRunesUserPrompt       = 4000 // intent — high signal, keep almost full
	maxRunesAssistantMessage = 4000 // decisions / reasoning — keep almost full
	maxRunesToolFailure      = 2000 // error context — middle ground
	maxRunesToolUse          = 1500 // tool input args — usually fits naturally
	maxRunesToolResult       = 800  // file dumps and stdout — head-truncate hard
	maxRunesDefault          = 2000 // any other / future kind — conservative
)

// capForKind returns the per-event rune cap for the named kind.
// Centralises the table above so renderEvents stays simple and a
// future kind can be added with a one-liner here.
func capForKind(kind string) int {
	switch kind {
	case ingest.KindUserPrompt:
		return maxRunesUserPrompt
	case ingest.KindAssistantMessage:
		return maxRunesAssistantMessage
	case ingest.KindToolFailure:
		return maxRunesToolFailure
	case ingest.KindToolUse:
		return maxRunesToolUse
	case ingest.KindToolResult:
		return maxRunesToolResult
	default:
		return maxRunesDefault
	}
}

// renderEvents flattens the per-event timeline for the summariser.
// Each event's body is capped to capForKind(e.Kind) runes — that
// surgical truncation handles the common case of a single huge
// tool_result blowing up the prompt without losing any chronology.
//
// We deliberately do NOT enforce a total transcript cap. If a
// session is so large that even per-kind-capped events sum past
// the API context window, the API will reject the request with a
// "prompt is too long" 400 — and that's the right outcome for now:
// summarising via silent middle-elision risks dropping decisions
// the user cared about. Surfacing the failure makes the size
// problem visible so we can decide whether to chunk, sample, or
// just skip the outlier.
func renderEvents(events []store.EventView, pats patternSet) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString(renderOneEvent(e, pats))
	}
	return b.String()
}

// renderOneEvent is the per-event renderer extracted from the
// previous monolithic renderEvents loop. Adds a per-kind cap on
// content_text so a single huge tool_result can't blow the
// prompt budget on its own.
func renderOneEvent(e store.EventView, pats patternSet) string {
	label := e.Kind
	if e.Role.Valid && e.Role.String != "" {
		label = e.Role.String + "/" + e.Kind
	}
	if e.ToolName.Valid && e.ToolName.String != "" {
		clean, names := redact.Outbound(e.ToolName.String)
		pats.addAll(names)
		label += " (" + clean + ")"
	}
	if e.SubagentID.Valid && e.SubagentID.String != "" {
		t := "?"
		if e.SubagentType.Valid && e.SubagentType.String != "" {
			t = e.SubagentType.String
		}
		label = "sa:" + e.SubagentID.String + ":" + t + " " + label
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", label)
	if e.ContentText.Valid && e.ContentText.String != "" {
		clean, names := redact.Outbound(e.ContentText.String)
		pats.addAll(names)
		content, truncated := truncateTextRunes(clean, capForKind(e.Kind))
		b.WriteString(content)
		if truncated {
			fmt.Fprintf(&b, "\n(… %s body truncated)", e.Kind)
		}
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.String()
}

// truncateTextRunes returns s capped at n runes; the bool is true
// when truncation actually fired. Rune-aware so multibyte UTF-8
// doesn't get split mid-character.
func truncateTextRunes(s string, n int) (string, bool) {
	if n <= 0 {
		return "", true
	}
	r := []rune(s)
	if len(r) <= n {
		return s, false
	}
	return string(r[:n]), true
}

// renderDigests flattens session digests for reflect/propose.
// Per-session Links (when present) are inline so the model's
// annotation tool output can cite them by session_id.
func renderDigests(digests []SessionDigest, pats patternSet) string {
	var b strings.Builder
	for _, d := range digests {
		// Header carries ONLY the canonical UUID — no index — so
		// callers like propose can't accidentally cite the
		// human-readable index ("3") in fields that demand a
		// session_id. The downstream prompt is unambiguous: any
		// "session_id" in your tool call must be a full UUID
		// matching this header.
		_, _ = fmt.Fprintf(&b, "## session_id: %s\n", d.ID)
		if d.StartedAtMs > 0 && d.EndedAtMs > 0 {
			_, _ = fmt.Fprintf(&b, "Window: %s → %s\n",
				time.UnixMilli(d.StartedAtMs).UTC().Format(time.RFC3339),
				time.UnixMilli(d.EndedAtMs).UTC().Format(time.RFC3339))
		}
		if line := renderOutcomeCue(d.Outcome); line != "" {
			b.WriteString(line)
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
		if len(d.ShellCommands) > 0 {
			// Cap the rendered list per session so a session that
			// ran 500 tiny `git status` calls doesn't dominate the
			// prompt. The first ~20 distinct commands carry the
			// pattern signal; the rest are usually duplicates the
			// extractor's GROUP BY value already collapsed.
			const maxCmdsPerSession = 20
			b.WriteString("Shell commands observed (extracted from tool_use events):\n")
			n := len(d.ShellCommands)
			if n > maxCmdsPerSession {
				n = maxCmdsPerSession
			}
			for i := 0; i < n; i++ {
				clean, names := redact.Outbound(d.ShellCommands[i])
				pats.addAll(names)
				b.WriteString("- ")
				b.WriteString(clean)
				b.WriteByte('\n')
			}
			if n < len(d.ShellCommands) {
				_, _ = fmt.Fprintf(&b, "- (… %d more commands omitted)\n",
					len(d.ShellCommands)-n)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// renderFailureShapes formats the negative-example stanza for the
// propose prompt. Empty input → empty string so the template
// splices unconditionally. Each line is one failure-shaped session
// with its dominant failure mode summarised in a counter tail —
// the LLM groups by mode and decides whether a skill could
// prevent or short-circuit it. ExpeL's contrastive principle.
func renderFailureShapes(shapes []FailureShapeDigest) string {
	if len(shapes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nFailure shapes observed (sessions that went wrong — recurring failures here are CANDIDATES for prevention skills, not just things to ignore):\n")
	for _, fs := range shapes {
		title := fs.Title
		if title == "" {
			title = "(no topic)"
		}
		// Title can carry user-typed text — pass through outbound
		// redact so any lingering secrets don't enter the prompt.
		clean, _ := redact.Outbound(title)
		const maxTitleRunes = 100
		if r := []rune(clean); len(r) > maxTitleRunes {
			clean = string(r[:maxTitleRunes]) + "…"
		}
		// Counter tail — only the non-zero counters render, so the
		// dominant failure mode is visually obvious.
		var markers []string
		if fs.ToolFailureCount > 0 {
			markers = append(markers, fmt.Sprintf("%d tool_failures", fs.ToolFailureCount))
		}
		if fs.GitUndoCount > 0 {
			markers = append(markers, fmt.Sprintf("%d git_undos", fs.GitUndoCount))
		}
		if fs.PromptRepeatCount > 0 {
			markers = append(markers, fmt.Sprintf("%d prompt_repeats", fs.PromptRepeatCount))
		}
		short := fs.SessionID
		if len(short) > 8 {
			short = short[:8]
		}
		fmt.Fprintf(&b, "- [%s] %s  (%s)\n", short, clean, strings.Join(markers, ", "))
	}
	return b.String()
}

// renderPriorProposals formats the prior-proposals stanza for the
// propose prompt. Empty input → empty string so the template
// splices cleanly. Each line categorises the proposal so the LLM
// doesn't have to derive the lifecycle state from raw fields:
//
//   - APPLIED, in use     — applied + LoadsAfterApply > 0
//   - APPLIED, unused     — applied + LoadsAfterApply == 0 (the user
//     kept it on disk but never invoked it;
//     weakest "still relevant" signal)
//   - APPLIED, failing    — applied + FailedLoadsAfter > 0 with
//     non-trivial load count (skill exists
//     but trips tool failures)
//   - NOT APPLIED         — proposed but applied_at_ms is NULL (user
//     did not act on the suggestion; near-
//     duplicate proposals are likely to be
//     rejected too)
//
// Hard rule (rule 12) in the system prompt instructs the LLM to
// treat each category as guidance: don't repropose APPLIED skills,
// reconsider triggers for APPLIED-unused, address failures for
// APPLIED-failing, and avoid repeating NOT APPLIED suggestions.
func renderPriorProposals(props []PriorProposal) string {
	if len(props) == 0 {
		return ""
	}
	now := time.Now().UTC()
	var b strings.Builder
	b.WriteString("\nPrior proposals (the system has emitted these before — DO NOT repropose near-duplicates; reconsider when_to_use for ones that landed but went unused; address the failure for ones with post-apply tool_failures):\n")
	for _, p := range props {
		ageDays := daysSince(now, p.ProposedAtMs)
		switch {
		case !p.Applied:
			fmt.Fprintf(&b, "- %s — proposed %d days ago, NOT APPLIED (user did not act on this suggestion)\n",
				p.SkillName, ageDays)
		case p.LoadsAfterApply == 0:
			appliedDays := daysSince(now, p.AppliedAtMs)
			fmt.Fprintf(&b, "- %s — proposed %d days ago, APPLIED %d days ago, 0 loads since (skill on disk but unused — when_to_use may be wrong)\n",
				p.SkillName, ageDays, appliedDays)
		case p.FailedLoadsAfter > 0:
			appliedDays := daysSince(now, p.AppliedAtMs)
			fmt.Fprintf(&b, "- %s — proposed %d days ago, APPLIED %d days ago, %d loads with %d failures (skill exists but failing — propose an evolution if the failure pattern is grounded in evidence)\n",
				p.SkillName, ageDays, appliedDays, p.LoadsAfterApply, p.FailedLoadsAfter)
		default:
			appliedDays := daysSince(now, p.AppliedAtMs)
			fmt.Fprintf(&b, "- %s — proposed %d days ago, APPLIED %d days ago, %d loads, 0 failures (in use, working — DO NOT repropose)\n",
				p.SkillName, ageDays, appliedDays, p.LoadsAfterApply)
		}
	}
	return b.String()
}

// daysSince returns the integer-day delta between now and a unix-ms
// timestamp, clamped to ≥0. Used by renderPriorProposals for "N days
// ago" rendering. Zero/negative inputs collapse to 0 ("just now") so
// the rendered line stays sensible even when the timestamp is in the
// future or unset.
func daysSince(now time.Time, ms int64) int {
	if ms <= 0 {
		return 0
	}
	delta := now.Sub(time.UnixMilli(ms))
	if delta < 0 {
		return 0
	}
	return int(delta / (24 * time.Hour))
}

// renderOutcomeCue formats the per-session outcome heuristic as one
// line: "Outcome: <label> (n failures, n undos, n repeats)". Returns
// "" when Outcome is nil so callers can splice unconditionally
// without a guard. The detail tail is suppressed for success_likely
// (zero by definition there) and unknown (no signal worth showing)
// to keep the prompt token-lean — only failure_likely / mixed
// sessions get the detail tail.
//
// Outcome is a HEURISTIC. The label is computed by store.deriveOutcomeLabel
// from observable signals (tool_failure_count, git_undo_count,
// prompt_repeat_count, last_event_kind). Downstream prompts treat it
// as a prior, not ground truth.
func renderOutcomeCue(o *store.SessionOutcome) string {
	if o == nil {
		return ""
	}
	switch o.Outcome {
	case store.OutcomeSuccessLikely:
		return "Outcome: success_likely\n"
	case store.OutcomeUnknown:
		// No detail — by definition the session was too thin to
		// have a useful signal here. Still render the label so the
		// LLM doesn't conclude "outcome was withheld."
		return "Outcome: unknown\n"
	case store.OutcomeFailureLikely, store.OutcomeMixed:
		return fmt.Sprintf(
			"Outcome: %s (%d tool_failures, %d git_undos, %d prompt_repeats)\n",
			o.Outcome, o.ToolFailureCount, o.GitUndoCount, o.PromptRepeatCount,
		)
	default:
		// Defensive: a label outside the closed set means the
		// store package added a value we don't render here yet.
		// Fall through to a bare label rather than dropping the
		// signal entirely.
		return fmt.Sprintf("Outcome: %s\n", o.Outcome)
	}
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
