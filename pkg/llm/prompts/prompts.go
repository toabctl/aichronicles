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
)

// --- result types ---

// SummaryResult is the schema-validated payload of a record_summary
// tool call. Fields are always populated (empty slices/strings on
// fields the model had nothing to say about).
type SummaryResult struct {
	Topic       string            `json:"topic"`
	WhatWasDone []string          `json:"what_was_done"`
	Unresolved  []string          `json:"unresolved"`
	KeyFiles    []string          `json:"key_files"`
	Links       []LinkAnnotation  `json:"links"`
	Subagents   []SubagentSummary `json:"subagents"`
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
type ProposedSkillScript struct {
	Name    string `json:"name"`           // bare filename, e.g. "build-test.sh"
	Purpose string `json:"purpose"`        // one-line description for the script header
	Body    string `json:"body,omitempty"` // optional starter body the LLM can ground from evidence
}

// --- summary ---

const summarySystem = `You summarize a single coding session between a human and an AI coding assistant. You MUST call the record_summary tool exactly once. Be factual and tight. Do not invent details. Do not invent URLs — only annotate links that were observed in the session. If a list section has no content, return an empty array.`

// summaryToolSchema is the JSON Schema for record_summary. Kept as a
// const so its bytes are stable; hashRequest includes these bytes
// when computing prompt_hash.
const summaryToolSchema = `{
  "type": "object",
  "required": ["topic","what_was_done","unresolved","key_files","links","subagents"],
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
    }
  }
}`

const summaryTemplate = `Session: %s
Events: %d
%s%s
Transcript follows, oldest first:
---
%s
---
`

// BuildSummary returns the prompt for summarizing one session's
// events.
//
// links is the distinct URL list observed in the session (typically
// from store.LoadExtractionsForSession(kind='url')); the model is
// prompted to annotate each with a `used_for` via the record_summary
// tool, dropping any it cannot confidently attribute.
//
// files is the distinct file_path list observed in the session
// (typically from store.LoadExtractionsForSession(kind='file_path'));
// it grounds the model's `key_files` output in actually-touched
// files rather than plausible-looking paths it might invent.
//
// Passing nil/empty for either slice is fine — the corresponding
// stanza is omitted and the model receives an empty array (or
// whatever it surfaces from prose mentions in the transcript).
func BuildSummary(sessionID string, events []store.EventView, links, files []string) (Built, error) {
	if sessionID == "" {
		return Built{}, fmt.Errorf("BuildSummary: sessionID required")
	}
	if len(events) == 0 {
		return Built{}, fmt.Errorf("BuildSummary: no events for session %s", sessionID)
	}

	pats := patternSet{}
	transcript := renderEvents(events, pats)
	linksBlock := renderLinksBlock(links, pats)
	filesBlock := renderFilesBlock(files, pats)

	userMsg := fmt.Sprintf(summaryTemplate, sessionID, len(events), linksBlock, filesBlock, transcript)

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
5. scripts[] is optional. Use it when the recurring pattern includes specific commands the user types repeatedly. Each script gets a bare filename (e.g. "build-test.sh"), a one-line purpose, and optionally a starter body. Don't list scripts[] entries that are just "run this one bash command" — those belong inline in the skill's steps. Reserve scripts[] for multi-step shell logic that benefits from being its own file.
6. frequency = the count of distinct session_ids in your evidence array.
7. effort: "small" = an afternoon. "medium" = a few days, well-scoped. "large" = a project-shaped effort that probably wants its own design doc.

Skill-awareness rules (the "Skills installed" and "Skills invoked recently" sections at the top of the user message are CANONICAL — do not invent skill names):

8. If a recurring pattern overlaps an *installed* skill (same domain or trigger), do NOT propose a new skill with that name or near-duplicate name. Skip the pattern.
9. If a pattern is a clear *extension* of an installed skill (the skill exists but has a gap), you may propose the skill, but its name must be DIFFERENT and alternatives_rejected MUST cite the existing skill and explain the increment.
10. Treat "Skills invoked recently" as evidence the user already uses those skills successfully. Patterns whose work is plausibly served by an invoked skill are solved — don't repropose them. The frequency in that section reflects real usage, not aspiration.

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
              "body":    {"type":"string","maxLength":4000}
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
type InvokedSkill struct {
	Name  string
	Count int
}

// ProposeInputs bundles every input BuildPropose consumes. Adding
// a field here is non-breaking; callers that don't have it pass
// the zero value. The shape was introduced when propose became
// skill-aware so the signature wouldn't sprout positional args.
type ProposeInputs struct {
	Digests         []SessionDigest
	InstalledSkills []InstalledSkill
	InvokedSkills   []InvokedSkill
}

const proposeTemplate = `Below are %d recent sessions.%s%s

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
func renderInvokedSkills(skills []InvokedSkill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nSkills invoked recently (count = times loaded in the window — these are working for the user):\n")
	for _, s := range skills {
		_, _ = fmt.Fprintf(&b, "- %s × %d\n", s.Name, s.Count)
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
	case "user_prompt":
		return maxRunesUserPrompt
	case "assistant_message":
		return maxRunesAssistantMessage
	case "tool_failure":
		return maxRunesToolFailure
	case "tool_use":
		return maxRunesToolUse
	case "tool_result":
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
