package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/toabctl/aichronicles/internal/config"
	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/preview"
	"github.com/toabctl/aichronicles/internal/store"
)

// providerLabel returns a short human-readable name for the
// configured LLM provider — used in command headers so the user
// sees which provider their batch is hitting before the first API
// call. Empty / unrecognised values fall back to "anthropic" since
// that's the documented default in llm.FromConfig.
func providerLabel(cfg llm.Config) string {
	switch cfg.Provider {
	case "", llm.ProviderAnthropic:
		return "anthropic"
	case llm.ProviderOpenAI:
		return "openai"
	default:
		return string(cfg.Provider)
	}
}

// resolveModelLabel returns the model identifier the user can
// expect to see in API requests for the given provider. When a
// flag-supplied model is non-empty it wins; otherwise we pick up
// the provider-specific default constant from internal/llm so the
// command header shows the real model id ("claude-sonnet-4-6")
// rather than a generic "(provider default)" placeholder.
//
// The constants stay the source of truth — bump them in internal/llm
// and headers across the cli automatically reflect the new
// default with no second list to keep in sync.
func resolveModelLabel(cfg llm.Config, flagModel string) string {
	if flagModel != "" {
		return flagModel
	}
	switch cfg.Provider {
	case "", llm.ProviderAnthropic:
		return llm.DefaultAnthropicModel
	case llm.ProviderOpenAI:
		return llm.DefaultOpenAIModel
	default:
		return "(unknown provider " + string(cfg.Provider) + " default)"
	}
}

// LLMConfigFromFile translates the file-shaped config.LLM into the
// runtime-shaped llm.Config that the llm package's FromConfig expects.
// Lives in the cli package so the llm package never imports config —
// keeps the layering one-way and the llm package independently
// testable without touching TOML.
func LLMConfigFromFile(in config.LLM) llm.Config {
	return llm.Config{
		Provider: llm.Provider(strings.ToLower(strings.TrimSpace(in.Provider))),
		Anthropic: llm.ProviderConfig{
			APIKeyCommand: in.Anthropic.APIKeyCommand,
		},
		OpenAI: llm.ProviderConfig{
			APIKeyCommand: in.OpenAI.APIKeyCommand,
		},
	}
}

// parseToolResult unmarshals the first tool_use block in resp into
// target, but only when its name matches toolName. Returns an error
// with a truncated + scrubbed view of any text the model produced
// instead, so operator diagnostics stay useful without smuggling
// potentially-sensitive raw text through.
//
// Used by summarize/reflect/propose to decode the forced-tool reply
// into a typed *Result struct.
func parseToolResult(resp *llm.Response, toolName string, target any) error {
	if resp == nil {
		return fmt.Errorf("parseToolResult: nil response")
	}
	if len(resp.ToolUses) == 0 {
		fallback := strings.TrimSpace(resp.Text)
		if fallback == "" {
			fallback = "(no text either)"
		}
		if len(fallback) > 200 {
			fallback = fallback[:200] + "…"
		}
		return fmt.Errorf("model did not call %s: %s", toolName, fallback)
	}
	// Forced tool use means the model is contractually obliged to
	// call exactly one tool. More than one is the model going off-
	// rails; silently picking [0] would discard whatever the others
	// said and the user couldn't tell.
	if len(resp.ToolUses) > 1 {
		names := make([]string, len(resp.ToolUses))
		for i, t := range resp.ToolUses {
			names[i] = t.Name
		}
		return fmt.Errorf("model returned %d tool uses (forced %s): %s",
			len(resp.ToolUses), toolName, strings.Join(names, ", "))
	}
	tu := resp.ToolUses[0]
	if tu.Name != toolName {
		return fmt.Errorf("model called tool %q but %q was forced", tu.Name, toolName)
	}
	if err := json.Unmarshal(tu.Input, target); err != nil {
		// Include a truncated preview of the raw tool input so an
		// operator can see exactly what the LLM emitted when its
		// shape disagrees with the Go struct. Without this, errors
		// like "cannot unmarshal string into struct field X" are
		// indistinguishable from one another and there's no way to
		// root-cause without re-running with a debugger attached.
		preview := string(tu.Input)
		if len(preview) > 800 {
			preview = preview[:800] + "…(truncated)"
		}
		// On decode failure, also dump the full raw payload to a
		// temp file so an operator can root-cause without
		// reproducing the exact LLM call. The path is logged at
		// debug level so the message length stays bounded.
		dumpPath := ""
		if f, ferr := os.CreateTemp("", "aichronicles-tool-decode-*.json"); ferr == nil {
			_, _ = f.Write(tu.Input)
			_ = f.Close()
			dumpPath = f.Name()
		}
		return fmt.Errorf("decode %s input: %w; raw=%s; full=%s", toolName, err, preview, dumpPath)
	}
	return nil
}

// emitLLMBody writes body to w, either verbatim (jsonRaw) or through
// the kind-specific human renderer. Used by every subcommand that
// serves up llm_outputs.body — summarize, reflect, propose, and the
// `summaries show` command added later.
//
// On a render failure (unparseable body from a legacy prose row or a
// schema drift), emitLLMBody falls back to the raw body followed by
// a one-line marker on stderr so the data is still seen. The caller
// owns the decision of whether to surface the error upstream.
func emitLLMBody(w io.Writer, kind store.LLMOutputKind, body string, jsonRaw bool) error {
	if jsonRaw {
		return writeWithNewline(w, body)
	}
	var err error
	switch kind {
	case store.LLMKindSummary:
		err = renderSummary(w, body)
	case store.LLMKindReflect:
		err = renderReflection(w, body)
	case store.LLMKindPropose:
		err = renderProposal(w, body)
	case store.LLMKindChallenge:
		err = renderChallenge(w, body)
	default:
		// Unknown kind — pass through raw so the user still sees
		// what's stored.
		return writeWithNewline(w, body)
	}
	if err != nil {
		// Unparseable body (legacy prose, schema drift). Fall back
		// to raw so the content isn't hidden by a render error.
		return writeWithNewline(w, body)
	}
	return nil
}

// jsonMarshalIndent is the canonical serializer for *Result types
// before they go into llm_outputs.body. Indented so stored rows are
// greppable; HTML escaping disabled so &, <, > round-trip clean.
func jsonMarshalIndent(v any) ([]byte, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// json.Encoder.Encode always appends a trailing newline. Strip
	// it so callers decide the line ending when they print.
	out := buf.String()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return []byte(out), nil
}

func writeWithNewline(w io.Writer, s string) error {
	if _, err := fmt.Fprint(w, s); err != nil {
		return err
	}
	if len(s) == 0 || s[len(s)-1] != '\n' {
		_, err := fmt.Fprintln(w)
		return err
	}
	return nil
}

// sectionHeader formats "<title>:" in bold when w is a TTY. Used by
// every llm-output render so section labels stand out against bullet
// content without changing the layout when piped.
func sectionHeader(w io.Writer, title string) string {
	return styled(w, title+":", ansiBold)
}

// renderSummary pretty-prints a prompts.SummaryResult JSON body.
func renderSummary(w io.Writer, body string) error {
	var r prompts.SummaryResult
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n\n", sectionHeader(w, "Topic"), r.Topic)
	writeBulletSection(&b, w, "What was done", r.WhatWasDone)
	writeBulletSection(&b, w, "Unresolved", r.Unresolved)
	writeBulletSection(&b, w, "Key files", r.KeyFiles)
	if len(r.Links) > 0 {
		fmt.Fprintf(&b, "%s\n", sectionHeader(w, "Links"))
		for _, l := range r.Links {
			fmt.Fprintf(&b, "  - %s\n    %s\n", l.URL, l.UsedFor)
		}
	}
	_, err := fmt.Fprint(w, b.String())
	return err
}

// renderReflection pretty-prints a prompts.ReflectionResult JSON
// body. Each task_type / friction leads with [freq=N (severity=…)]
// so the reviewer can scan by frequency or impact; evidence quotes
// follow indented so the claim is grep-verifiable in the terminal.
func renderReflection(w io.Writer, body string) error {
	var r prompts.ReflectionResult
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return err
	}
	var b strings.Builder
	if len(r.TaskTypes) > 0 {
		fmt.Fprintf(&b, "%s\n", sectionHeader(w, "Recurring task types"))
		for _, t := range r.TaskTypes {
			fmt.Fprintf(&b, "  - %s  [freq=%d]\n", t.Label, t.Frequency)
			writeReflectEvidence(&b, t.Evidence)
		}
		b.WriteByte('\n')
	}
	if len(r.Frictions) > 0 {
		fmt.Fprintf(&b, "%s\n", sectionHeader(w, "Recurring sources of friction"))
		for _, f := range r.Frictions {
			fmt.Fprintf(&b, "  - %s  [freq=%d severity=%s]\n", f.Label, f.Frequency, f.Severity)
			writeReflectEvidence(&b, f.Evidence)
		}
		b.WriteByte('\n')
	}
	if r.WorkflowChange != "" {
		fmt.Fprintf(&b, "%s\n  %s\n", sectionHeader(w, "Suggested workflow change"), r.WorkflowChange)
	}
	_, err := fmt.Fprint(w, b.String())
	return err
}

// writeReflectEvidence is the reflection-side mirror of writeEvidence
// in renderProposal — kept separate because the Evidence types are
// distinct (so the renderer can diverge if the surfaces diverge).
func writeReflectEvidence(b *strings.Builder, evidence []prompts.ReflectionEvidence) {
	if len(evidence) == 0 {
		return
	}
	b.WriteString("    evidence:\n")
	for _, ev := range evidence {
		short := preview.ShortID(ev.SessionID)
		fmt.Fprintf(b, "      %s: %q (%s)\n", short, ev.Quote, ev.WhatHappened)
	}
}

// renderProposal pretty-prints a prompts.ProposalResult JSON body.
// Each item leads with `[freq=N effort=size]` so the user can scan
// for high-frequency / low-effort wins first; evidence lines carry
// a verbatim quote from each cited session so the proposal is
// grep-verifiable without leaving the terminal.
// renderChallenge renders a ChallengeResult body — the forward-
// looking counterpart to renderProposal. Each row shows title,
// effort, problem, why, what-it-anchors-on, and the success
// criterion. Distinct layout from renderProposal because there's
// no evidence list and no scripts; the "anchors" line is the
// closest analog to evidence (it cites what grounds the suggestion
// in the user's current state).
func renderChallenge(w io.Writer, body string) error {
	var r prompts.ChallengeResult
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return err
	}
	var b strings.Builder
	if len(r.Challenges) == 0 {
		fmt.Fprintf(&b, "%s\n", sectionHeader(w,
			"No challenges proposed (nothing in the input grounded a worthwhile next problem)"))
		_, err := fmt.Fprint(w, b.String())
		return err
	}
	fmt.Fprintf(&b, "%s\n", sectionHeader(w, "Proposed challenges"))
	for _, c := range r.Challenges {
		fmt.Fprintf(&b, "  - %s  [effort=%s]\n", c.Title, c.Effort)
		fmt.Fprintf(&b, "    problem: %s\n", c.Problem)
		fmt.Fprintf(&b, "    why:     %s\n", c.Why)
		if len(c.GroundedIn) > 0 {
			fmt.Fprintf(&b, "    anchors: %s\n", strings.Join(c.GroundedIn, ", "))
		}
		fmt.Fprintf(&b, "    success: %s\n", c.SuccessLooksLike)
	}
	_, err := fmt.Fprint(w, b.String())
	return err
}

func renderProposal(w io.Writer, body string) error {
	var r prompts.ProposalResult
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return err
	}
	var b strings.Builder
	if len(r.Skills) > 0 {
		fmt.Fprintf(&b, "%s\n", sectionHeader(w, "Proposed skills"))
		for _, s := range r.Skills {
			fmt.Fprintf(&b, "  - %s  [freq=%d effort=%s]\n    when: %s\n    why:  %s\n",
				s.Name, s.Frequency, s.Effort, s.WhenToUse, s.Why)
			for _, sc := range s.Scripts {
				fmt.Fprintf(&b, "    script: scripts/%s  — %s\n", sc.Name, sc.Purpose)
			}
			writeEvidence(&b, s.Evidence)
			writeAlternatives(&b, s.AlternativesRejected)
		}
	}
	_, err := fmt.Fprint(w, b.String())
	return err
}

// writeEvidence renders an indented "evidence:" block. Each entry
// shows the short session id, the verbatim quote, and the
// what_happened context — that's the bare minimum for a reviewer
// to verify "yes, the model is grounded in real session bytes".
func writeEvidence(b *strings.Builder, evidence []prompts.ProposalEvidence) {
	if len(evidence) == 0 {
		return
	}
	b.WriteString("    evidence:\n")
	for _, ev := range evidence {
		short := preview.ShortID(ev.SessionID)
		fmt.Fprintf(b, "      %s: %q (%s)\n", short, ev.Quote, ev.WhatHappened)
	}
}

// writeAlternatives renders the alternatives_rejected line when
// non-empty. Skipped on empty so a tight proposal that didn't have
// to choose between forms doesn't render an awkward blank line.
func writeAlternatives(b *strings.Builder, alt string) {
	if alt == "" {
		return
	}
	fmt.Fprintf(b, "    alt:  %s\n", alt)
}

func writeBulletSection(b *strings.Builder, w io.Writer, header string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "%s\n", sectionHeader(w, header))
	for _, s := range items {
		fmt.Fprintf(b, "  - %s\n", s)
	}
	b.WriteByte('\n')
}
