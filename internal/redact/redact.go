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

// Detector is the simplest Scanner: a single named regex with an
// optional fast-path literal prefilter. Construct with NewDetector
// so pattern compilation is bounded to package init.
type Detector struct {
	Name string
	RE   *regexp.Regexp

	// Prefilter is a set of literal substrings (any-of). When
	// non-empty, Scan does an O(n) substring screen before
	// running the regex — if NONE of the literals appear in the
	// input, the regex cannot match and is skipped entirely.
	// Empty means "run the regex unconditionally" (the right
	// choice for case-insensitive patterns we don't want to
	// pay an O(n) lowercase copy for).
	//
	// Correctness invariant: every string the regex would match
	// MUST contain at least one of the prefilter literals.
	// Otherwise the prefilter introduces false negatives.
	Prefilter []string
}

// NewDetector compiles pattern and returns a Detector. Panics on a
// bad regex — pattern lists are static, so a bad regex is a build
// error, not a runtime surprise.
func NewDetector(name, pattern string) *Detector {
	return &Detector{Name: name, RE: regexp.MustCompile(pattern)}
}

// WithPrefilter attaches one or more literal substrings as a
// fast-path screen. At least one MUST appear verbatim in any
// string the regex could match (see Detector.Prefilter doc).
// Returns the detector so it can be chained inside the
// builtinDetectors literal:
//
//	NewDetector("foo", `\bfoo[A-Z]+`).WithPrefilter("foo")
func (d *Detector) WithPrefilter(literals ...string) *Detector {
	d.Prefilter = literals
	return d
}

// Scan finds every non-overlapping match of the detector's regex.
// When Prefilter is set, it short-circuits to nil for inputs that
// can't possibly match — turning the typical "scan a paragraph of
// prose" call from O(input × regex-state-machine) to O(input)
// memcmp. On a real-world Claude transcript import this dropped
// the regex share of CPU time from ~63% to a small fraction.
func (d *Detector) Scan(s string) []Finding {
	if len(d.Prefilter) > 0 && !containsAny(s, d.Prefilter) {
		return nil
	}
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

// containsAny reports whether s contains any literal from
// substrings. Linear in len(s) × len(substrings) worst case, but
// each strings.Contains is a SIMD-accelerated memmem-shape scan
// that's orders of magnitude faster than running the corresponding
// regex on miss.
func containsAny(s string, substrings []string) bool {
	for _, sub := range substrings {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
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
// Order matters for overlap resolution: earlier = wins on ties.
// Most specific / longest-prefix patterns go first.
//
// Each detector that can be screened by a literal prefix uses
// WithPrefilter so prose-heavy input (the common case for our
// import path) skips the regex engine entirely on miss.
// Case-insensitive detectors and ones whose required literal is
// too generic to filter usefully run their regex unconditionally.
func builtinDetectors() []Scanner {
	return []Scanner{
		// Specific API keys with distinctive prefixes.
		NewDetector("anthropic_api_key", `\bsk-ant-[A-Za-z0-9_-]{20,}\b`).
			WithPrefilter("sk-ant-"),
		NewDetector("openai_api_key", `\bsk-(?:proj-)?[A-Za-z0-9_-]{40,}\b`).
			WithPrefilter("sk-"),
		NewDetector("google_api_key", `\bAIza[0-9A-Za-z_-]{35}\b`).
			WithPrefilter("AIza"),
		// Google OAuth access token. Format documented in
		// google.golang.org/api/oauth2/v2 and the Identity Platform
		// spec: `ya29.` prefix then a long opaque body. Real tokens
		// are 100+ chars but the prefix is the load-bearing literal.
		NewDetector("gcp_oauth_access_token", `\bya29\.[A-Za-z0-9._-]{20,}`).
			WithPrefilter("ya29."),
		// Google OAuth refresh token. Stored in
		// ~/.config/gcloud/application_default_credentials.json under
		// "refresh_token". `1//0` prefix is the documented format for
		// Google's OAuth refresh tokens; bodies are ~60 base64url chars.
		NewDetector("gcp_oauth_refresh_token", `\b1//0[A-Za-z0-9_-]{40,}`).
			WithPrefilter("1//0"),
		// Google OAuth 2.0 client secret. Distinctive `GOCSPX-`
		// prefix introduced in 2022; bodies are 28 chars of
		// [A-Za-z0-9_-]. Used by OAuth desktop / web clients.
		NewDetector("gcp_oauth_client_secret", `\bGOCSPX-[A-Za-z0-9_-]{20,}\b`).
			WithPrefilter("GOCSPX-"),
		NewDetector("github_pat_fine_grained", `\bgithub_pat_[A-Za-z0-9_]{82}\b`).
			WithPrefilter("github_pat_"),
		// gh[pousr]_ has no single literal prefix, but the union of
		// the five concrete 4-byte prefixes covers every match.
		NewDetector("github_pat_classic", `\bgh[pousr]_[A-Za-z0-9]{36}\b`).
			WithPrefilter("ghp_", "gho_", "ghu_", "ghs_", "ghr_"),
		NewDetector("aws_access_key", `\bAKIA[0-9A-Z]{16}\b`).
			WithPrefilter("AKIA"),
		NewDetector("npm_token", `\bnpm_[A-Za-z0-9]{36}\b`).
			WithPrefilter("npm_"),
		NewDetector("slack_token", `\bxox[abprs]-[0-9]{10,}-[0-9a-zA-Z-]{24,}\b`).
			WithPrefilter("xox"),
		// (?:sk|pk|rk)_(?:live|test)_ — six concrete combinations
		// cover every possible match.
		NewDetector("stripe_key", `\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{24,}\b`).
			WithPrefilter(
				"sk_live_", "sk_test_",
				"pk_live_", "pk_test_",
				"rk_live_", "rk_test_",
			),
		// twilio_sid: AC and SK alone are too noisy in prose (any
		// capitalised word can contain "AC"); the regex's own RE2
		// literal-prefix screen handles the miss case adequately.
		NewDetector("twilio_sid", `\b(?:AC|SK)[a-f0-9]{32}\b`),

		// Structural patterns (no short distinctive prefix in the
		// "uppercase-prefix" sense, but most still have a strong
		// literal anchor we can screen on).
		NewDetector("pem_private_key",
			`-----BEGIN (?:RSA |EC |OPENSSH |PGP |DSA |ENCRYPTED )?PRIVATE KEY-----[\s\S]*?-----END [^-]*PRIVATE KEY-----`).
			WithPrefilter("-----BEGIN"),
		NewDetector("jwt",
			`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`).
			WithPrefilter("eyJ"),
		// bearer_token / db_connection_string / aws_secret_key_assignment
		// are all (?i)-flagged. A case-insensitive prefilter would
		// mean a per-Scan lowercase copy of the input — for typical
		// inputs the cost would dwarf the benefit. Run the regex
		// directly; RE2 handles the miss case in microseconds.
		NewDetector("bearer_token",
			`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{20,}\b`),
		NewDetector("db_connection_string",
			`(?i)\b(?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis(?:s)?|amqp)://[^:@\s]+:[^@\s]+@[^\s]+`),
		// basic_auth_url is case-sensitive and "://" is a required
		// substring — a serviceable prefilter for prose input.
		NewDetector("basic_auth_url",
			`\bhttps?://[^:@\s/]+:[^@\s/]+@[^\s]+`).
			WithPrefilter("://"),

		// Context-aware: AWS secret follows an assignment-style key.
		// Matches the whole key=value so the assignment syntax is
		// redacted alongside the 40-char secret. Case-insensitive
		// → no prefilter (same reasoning as bearer_token).
		NewDetector("aws_secret_key_assignment",
			`(?i)\baws_?secret(?:_access)?_?key\s*[:=]\s*["']?[A-Za-z0-9/+=]{40}["']?`),
	}
}
