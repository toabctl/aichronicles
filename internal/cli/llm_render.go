package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
	"github.com/toabctl/aichronicles/internal/store"
)

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
	tu := resp.ToolUses[0]
	if tu.Name != toolName {
		return fmt.Errorf("model called tool %q but %q was forced", tu.Name, toolName)
	}
	if err := json.Unmarshal(tu.Input, target); err != nil {
		return fmt.Errorf("decode %s input: %w", toolName, err)
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

// renderSummary pretty-prints a prompts.SummaryResult JSON body.
func renderSummary(w io.Writer, body string) error {
	var r prompts.SummaryResult
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Topic: %s\n\n", r.Topic)
	writeBulletSection(&b, "What was done", r.WhatWasDone)
	writeBulletSection(&b, "Unresolved", r.Unresolved)
	writeBulletSection(&b, "Key files", r.KeyFiles)
	if len(r.Links) > 0 {
		b.WriteString("Links:\n")
		for _, l := range r.Links {
			fmt.Fprintf(&b, "  - %s\n    %s\n", l.URL, l.UsedFor)
		}
	}
	_, err := fmt.Fprint(w, b.String())
	return err
}

// renderReflection pretty-prints a prompts.ReflectionResult JSON body.
func renderReflection(w io.Writer, body string) error {
	var r prompts.ReflectionResult
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return err
	}
	var b strings.Builder
	writeEvidencedSection(&b, "Recurring task types", r.TaskTypes)
	writeEvidencedSection(&b, "Recurring sources of friction", r.Frictions)
	if r.WorkflowChange != "" {
		fmt.Fprintf(&b, "Suggested workflow change:\n  %s\n", r.WorkflowChange)
	}
	_, err := fmt.Fprint(w, b.String())
	return err
}

// renderProposal pretty-prints a prompts.ProposalResult JSON body.
func renderProposal(w io.Writer, body string) error {
	var r prompts.ProposalResult
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return err
	}
	var b strings.Builder
	if len(r.Skills) > 0 {
		b.WriteString("Skills / slash-command ideas:\n")
		for _, s := range r.Skills {
			fmt.Fprintf(&b, "  - %s\n    when: %s\n    why:  %s\n    evidence: %s\n",
				s.Name, s.WhenToUse, s.Why, strings.Join(s.SessionIDs, ", "))
		}
		b.WriteByte('\n')
	}
	if len(r.ClaudeMdEntries) > 0 {
		b.WriteString("CLAUDE.md entries worth adding:\n")
		for _, e := range r.ClaudeMdEntries {
			fmt.Fprintf(&b, "  - %s\n    why:      %s\n    evidence: %s\n",
				e.Rule, e.Why, strings.Join(e.SessionIDs, ", "))
		}
		b.WriteByte('\n')
	}
	if len(r.Scripts) > 0 {
		b.WriteString("Pre-built scripts worth keeping:\n")
		for _, s := range r.Scripts {
			fmt.Fprintf(&b, "  - %s — %s\n    evidence: %s\n",
				s.Name, s.Purpose, strings.Join(s.SessionIDs, ", "))
		}
	}
	_, err := fmt.Fprint(w, b.String())
	return err
}

func writeBulletSection(b *strings.Builder, header string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "%s:\n", header)
	for _, s := range items {
		fmt.Fprintf(b, "  - %s\n", s)
	}
	b.WriteByte('\n')
}

func writeEvidencedSection(b *strings.Builder, header string, items []prompts.Evidenced) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "%s:\n", header)
	for _, it := range items {
		fmt.Fprintf(b, "  - %s\n    evidence: %s\n", it.Label, strings.Join(it.SessionIDs, ", "))
	}
	b.WriteByte('\n')
}
