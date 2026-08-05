package events

import (
	"encoding/json"
	"sort"

	"github.com/toabctl/aichronicles/internal/redact"
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
// redact.Scanner (the regex/detector catalog) to the
// envelope-shaped Redactor interface. NewScannerRedactor wraps
// any redact.Scanner; the project default is wired by callers
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
// env.Redaction is populated with every pattern that fired across
// all fields in THIS pass; env.Redaction.Applied is set to true
// unconditionally so downstream code can use it as a "scrubber ran"
// signal independent of whether anything actually matched.
//
// Patterns reflect THIS pass only, not a historical union. Scrub
// (the only re-scrubbing caller) gates on len(Patterns)==0 to skip
// the rewrite when this pass found nothing new, which preserves the
// original ingest-time pattern list in the persisted envelope_json
// untouched.
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
// scrubs every string leaf.
//
// The default arm must NOT pass unrecognised values through. A Source
// is free to put any Go value in Payload — the Gemini transcript
// reader stores json.RawMessage for message content and a *toolCall
// struct for tool events — and a pass-through default silently
// exempted those subtrees from scrubbing. The failure was invisible
// from the outside: the sibling ContentText scrubbed correctly and
// Redaction.Applied was still set to true, so the Sink's "scrubber
// ran" assertion passed and plaintext landed in raw_envelopes.
//
// Anything that is not already a JSON-generic value is therefore
// normalised through a marshal/unmarshal round-trip and walked in its
// generic form. Re-marshalling the generic form reproduces byte-identical
// JSON, so this changes what we scan, not what we store.
func walkAny(v any, scanner redact.Scanner, patterns map[string]struct{}) any {
	switch x := v.(type) {
	case nil:
		return nil
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
	case bool, float64, float32,
		int, int8, int16, int32, int64,
		uint, uint16, uint32, uint64,
		json.Number:
		// Genuine JSON scalars: no string leaf, nothing to scrub.
		// uint8 is deliberately absent — []uint8 is []byte, which
		// belongs on the normalising path below, and a bare byte
		// carries no secret either way.
		return x
	default:
		return normaliseAndWalk(v, scanner, patterns)
	}
}

// normaliseAndWalk converts a non-JSON-generic value into generic form
// and scrubs it. Used for json.RawMessage, []byte, and any concrete
// struct a Source placed in Payload.
//
// A value that cannot be marshalled is dropped rather than returned
// unscrubbed: we cannot inspect it, so we cannot claim it is
// secret-free, and "returning nothing" beats storing something wrong.
// In practice this is unreachable — Pipeline.Process marshals the
// whole envelope immediately afterwards, so such a value would fail
// the ingest anyway.
func normaliseAndWalk(v any, scanner redact.Scanner, patterns map[string]struct{}) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil
	}
	return walkAny(generic, scanner, patterns)
}
