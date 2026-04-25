// Package redact detects credentials in arbitrary text and rewrites
// them as opaque "<redacted:kind>" markers. It is the first line of
// defense against leaking API keys, tokens, private keys, and similar
// high-value secrets into aichronicles' store and (later) into LLM
// calls.
//
// Reuse: this package is provider-neutral and aichronicles-agnostic;
// any Go program needing pattern-based credential redaction can
// import it directly. aichronicles is a work in progress.
//
// Scope is deliberate: credential patterns only. User PII (names,
// emails, addresses) is NOT in scope — that data is already in
// ~/.claude/projects/ unredacted, scrubbing it here adds friction
// with marginal incremental risk reduction. See ROADMAP.md Block A
// for the full threat model.
//
// Design properties worth holding on to:
//   - Patterns are RE2-compatible (Go's regexp package). No lookaround.
//   - Detectors compile their regex once at package init and never
//     inside a scan. Safe for concurrent use.
//   - Replacement is byte-accurate: markers are inserted at exact
//     offsets the detectors return; no surrounding text is mutated.
//   - The marker format `<redacted:kind>` is deterministic and doesn't
//     embed any bytes from the original secret.
//   - Overlapping findings from different detectors are resolved by:
//     first by start offset (earliest wins), then by length (longest
//     wins), then by registration order (earlier-listed wins). This
//     makes e.g. the Anthropic key detector label `sk-ant-…` strings
//     as anthropic_api_key rather than openai_api_key even though the
//     looser OpenAI pattern also matches.
package redact

import (
	"regexp"
	"sort"
	"strings"
)

// Finding locates one detected secret in a scanned string. Start is
// inclusive, End exclusive, both byte offsets. Pattern names a
// detector; Replace uses it to build the marker.
type Finding struct {
	Pattern string
	Start   int
	End     int
}

// Scanner runs credential detection over a string. Implementations
// are expected to be pure, deterministic, and safe for concurrent use.
type Scanner interface {
	Scan(s string) []Finding
}

// Detector is the simplest Scanner: a single named regex. Construct
// with NewDetector so pattern compilation is bounded to package init.
type Detector struct {
	Name string
	RE   *regexp.Regexp
}

// NewDetector compiles pattern and returns a Detector. Panics on a
// bad regex — pattern lists are static, so a bad regex is a build
// error, not a runtime surprise.
func NewDetector(name, pattern string) *Detector {
	return &Detector{Name: name, RE: regexp.MustCompile(pattern)}
}

// Scan finds every non-overlapping match of the detector's regex.
func (d *Detector) Scan(s string) []Finding {
	matches := d.RE.FindAllStringIndex(s, -1)
	if matches == nil {
		return nil
	}
	out := make([]Finding, len(matches))
	for i, m := range matches {
		out[i] = Finding{Pattern: d.Name, Start: m[0], End: m[1]}
	}
	return out
}

// Composite chains multiple detectors and merges their findings into
// a non-overlapping, start-sorted slice. See the package comment for
// the overlap-resolution rules.
type Composite struct {
	Detectors []Scanner
}

// NewComposite wraps a detector list.
func NewComposite(detectors ...Scanner) *Composite {
	return &Composite{Detectors: detectors}
}

// Scan runs every detector and deduplicates overlapping findings.
// Returns findings sorted by Start ascending, guaranteed non-overlapping.
func (c *Composite) Scan(s string) []Finding {
	type tagged struct {
		f   Finding
		idx int
	}
	var all []tagged
	for i, d := range c.Detectors {
		for _, f := range d.Scan(s) {
			all = append(all, tagged{f: f, idx: i})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].f.Start != all[j].f.Start {
			return all[i].f.Start < all[j].f.Start
		}
		if all[i].f.End != all[j].f.End {
			return all[i].f.End > all[j].f.End
		}
		return all[i].idx < all[j].idx
	})

	var out []Finding
	prevEnd := 0
	for _, t := range all {
		if len(out) > 0 && t.f.Start < prevEnd {
			continue
		}
		out = append(out, t.f)
		prevEnd = t.f.End
	}
	return out
}

// Replace rewrites s with each finding replaced by "<redacted:pattern>".
// Findings need not be pre-sorted — Replace sorts and skips overlaps.
// Returns the redacted string and the unique, sorted list of pattern
// names that fired, suitable for populating envelope.Redaction.Patterns.
func Replace(s string, findings []Finding) (string, []string) {
	if len(findings) == 0 {
		return s, nil
	}
	local := append([]Finding(nil), findings...)
	sort.Slice(local, func(i, j int) bool { return local[i].Start < local[j].Start })

	var b strings.Builder
	b.Grow(len(s))
	patterns := map[string]struct{}{}

	prev := 0
	for _, f := range local {
		if f.Start < prev {
			// Overlap with previous; skip. Should not happen when
			// Composite produced the findings, but Replace is public
			// so defend the invariant.
			continue
		}
		if f.Start < 0 || f.End > len(s) || f.End < f.Start {
			// Out-of-range finding — defend against caller bugs.
			continue
		}
		b.WriteString(s[prev:f.Start])
		b.WriteString("<redacted:")
		b.WriteString(f.Pattern)
		b.WriteByte('>')
		patterns[f.Pattern] = struct{}{}
		prev = f.End
	}
	b.WriteString(s[prev:])

	names := make([]string, 0, len(patterns))
	for p := range patterns {
		names = append(names, p)
	}
	sort.Strings(names)
	return b.String(), names
}

// defaultScanner is the package-wide instance returned by Default.
// Built once at init; safe for concurrent use thereafter.
var defaultScanner = NewComposite(builtinDetectors()...)

// Default returns the production credential scanner with every
// baked-in detector enabled. The returned Scanner is a singleton —
// do not mutate it; create a fresh Composite if you need a custom set.
func Default() Scanner { return defaultScanner }

// BuiltinDetectors returns the production detector list in the same
// registration order Default uses internally. Exported for docgen
// tooling that enumerates the patterns without depending on
// test-only internals — every Scanner is a *Detector, so callers
// can type-assert to read .Name and .RE.String().
func BuiltinDetectors() []Scanner {
	return builtinDetectors()
}

// builtinDetectors is the ordered list of production detectors.
// Order matters for overlap resolution: earlier = wins on ties. Most
// specific / longest-prefix patterns go first.
func builtinDetectors() []Scanner {
	return []Scanner{
		// Specific API keys with distinctive prefixes.
		NewDetector("anthropic_api_key", `\bsk-ant-[A-Za-z0-9_-]{20,}\b`),
		NewDetector("openai_api_key", `\bsk-(?:proj-)?[A-Za-z0-9_-]{40,}\b`),
		NewDetector("google_api_key", `\bAIza[0-9A-Za-z_-]{35}\b`),
		NewDetector("github_pat_fine_grained", `\bgithub_pat_[A-Za-z0-9_]{82}\b`),
		NewDetector("github_pat_classic", `\bgh[pousr]_[A-Za-z0-9]{36}\b`),
		NewDetector("aws_access_key", `\bAKIA[0-9A-Z]{16}\b`),
		NewDetector("npm_token", `\bnpm_[A-Za-z0-9]{36}\b`),
		NewDetector("slack_token", `\bxox[abprs]-[0-9]{10,}-[0-9a-zA-Z-]{24,}\b`),
		NewDetector("stripe_key", `\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{24,}\b`),
		NewDetector("twilio_sid", `\b(?:AC|SK)[a-f0-9]{32}\b`),

		// Structural patterns (no short distinctive prefix).
		NewDetector("pem_private_key",
			`-----BEGIN (?:RSA |EC |OPENSSH |PGP |DSA |ENCRYPTED )?PRIVATE KEY-----[\s\S]*?-----END [^-]*PRIVATE KEY-----`),
		NewDetector("jwt",
			`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`),
		NewDetector("bearer_token",
			`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{20,}\b`),
		NewDetector("db_connection_string",
			`(?i)\b(?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis(?:s)?|amqp)://[^:@\s]+:[^@\s]+@[^\s]+`),
		NewDetector("basic_auth_url",
			`\bhttps?://[^:@\s/]+:[^@\s/]+@[^\s]+`),

		// Context-aware: AWS secret follows an assignment-style key.
		// Matches the whole key=value so the assignment syntax is
		// redacted alongside the 40-char secret.
		NewDetector("aws_secret_key_assignment",
			`(?i)\baws_?secret(?:_access)?_?key\s*[:=]\s*["']?[A-Za-z0-9/+=]{40}["']?`),
	}
}
