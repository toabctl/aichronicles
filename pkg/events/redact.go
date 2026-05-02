package events

import (
	"sort"

	"github.com/toabctl/aichronicles/pkg/redact"
)

// Redactor is the interface the Pipeline uses to scrub secrets
// from an envelope before extractor dispatch and Sink.Write. Apply
// mutates env: text-bearing fields are scrubbed in place and
// env.Redaction.Applied is set to true.
//
// The Pipeline owns redaction end-to-end (Sources are pure
// translators; the Pipeline is the single point of enforcement).
// The Pipeline holds the Redactor at construction and calls Apply
// once per Process invocation; the SQLite Sink also enforces
// "Applied=true" as a defense-in-depth assertion against a
// programmer error that bypasses the Pipeline.
type Redactor interface {
	Apply(env *Envelope)
}

// ScannerRedactor is the production Redactor: it adapts a
// pkg/redact.Scanner (the regex/detector catalog) to the
// envelope-shaped Redactor interface. NewScannerRedactor wraps
// any pkg/redact.Scanner; the project default is wired by callers
// as ScannerRedactor{Scanner: redact.Default()}.
type ScannerRedactor struct {
	Scanner redact.Scanner
}

// NewScannerRedactor returns a Redactor that applies the given
// detector catalog. nil scanner produces a Redactor whose Apply
// only sets env.Redaction.Applied=true with no pattern matches —
// useful for tests that want to satisfy the redaction-applied
// gate without scanning.
func NewScannerRedactor(s redact.Scanner) *ScannerRedactor {
	return &ScannerRedactor{Scanner: s}
}

// Apply implements Redactor by deferring to the package-level
// ApplyRedaction free function, which keeps backward compatibility
// for callers that already pass a redact.Scanner directly.
func (r *ScannerRedactor) Apply(env *Envelope) {
	if env == nil {
		return
	}
	if r.Scanner == nil {
		// No catalog: no scrubbing, but the "scrubber ran" signal
		// still gets set so the Sink's Applied=true assertion
		// passes. Useful in tests with synthetic envelopes that
		// don't carry credentials anyway.
		env.Redaction = &Redaction{Applied: true}
		return
	}
	ApplyRedaction(env, r.Scanner)
}

// ApplyRedaction scans every free-text field on env with scanner and
// rewrites detected secrets to <redacted:kind> markers in place.
// env.Redaction is populated with the union of every pattern that
// fired across all fields; env.Redaction.Applied is set to true
// unconditionally so downstream code can use it as a "scrubber ran"
// signal independent of whether anything actually matched.
//
// Fields covered:
//   - ContentText (free-form string)
//   - Cwd (paths can legitimately contain secrets — usernames equal to
//     access-key-shaped strings, temp dirs holding tokens)
//   - Payload (recursive walk of every string leaf in the map/array tree)
//
// Slug-shaped fields (source_agent, kind, role, tool names, UUIDs) are
// not scanned: by construction they can't carry credentials, and
// running regex over them would only add noise to any audit output.
func ApplyRedaction(env *Envelope, scanner redact.Scanner) {
	patterns := map[string]struct{}{}

	if env.ContentText != "" {
		env.ContentText = scrubString(env.ContentText, scanner, patterns)
	}
	if env.Cwd != "" {
		env.Cwd = scrubString(env.Cwd, scanner, patterns)
	}
	if env.Payload != nil {
		if m, ok := walkAny(env.Payload, scanner, patterns).(map[string]any); ok {
			env.Payload = m
		}
	}

	names := make([]string, 0, len(patterns))
	for p := range patterns {
		names = append(names, p)
	}
	sort.Strings(names)
	env.Redaction = &Redaction{Applied: true, Patterns: names}
}

func scrubString(s string, scanner redact.Scanner, patterns map[string]struct{}) string {
	findings := scanner.Scan(s)
	out, names := redact.Replace(s, findings)
	for _, n := range names {
		patterns[n] = struct{}{}
	}
	return out
}

// walkAny recurses into JSON-shaped values (maps, arrays, strings) and
// scrubs every string leaf. Non-string scalars (numbers, bools, null)
// are returned unchanged — they cannot carry a matching secret.
func walkAny(v any, scanner redact.Scanner, patterns map[string]struct{}) any {
	switch x := v.(type) {
	case string:
		return scrubString(x, scanner, patterns)
	case map[string]any:
		for k, vv := range x {
			x[k] = walkAny(vv, scanner, patterns)
		}
		return x
	case []any:
		for i, vv := range x {
			x[i] = walkAny(vv, scanner, patterns)
		}
		return x
	default:
		return v
	}
}
