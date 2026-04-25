package ingest

import (
	"sort"

	"github.com/toabctl/aichronicles/pkg/redact"
)

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
