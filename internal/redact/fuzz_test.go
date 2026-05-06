package redact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzDefaultScanReplace exercises the production detector chain
// against arbitrary input. The properties below capture the
// load-bearing invariants of a credential scrubber — anything that
// breaks them is a real bug, not a fuzz curiosity.
//
// The seed corpus mixes prose, redaction-marker lookalikes, and
// representative tokens for every pattern in builtinDetectors so
// the fuzzer starts from coverage of the prefilter fast path AND
// the full regex evaluator.
func FuzzDefaultScanReplace(f *testing.F) {
	seeds := []string{
		"",
		"plain prose with no secrets at all",
		"sk-ant-" + strings.Repeat("a", 30),
		"sk-proj-" + strings.Repeat("b", 50),
		"AIza" + strings.Repeat("c", 35),
		"github_pat_" + strings.Repeat("d", 82),
		"ghp_" + strings.Repeat("e", 36),
		"AKIA" + strings.Repeat("F", 16),
		"npm_" + strings.Repeat("g", 36),
		"xoxb-1234567890-" + strings.Repeat("h", 30),
		"sk_live_" + strings.Repeat("i", 30),
		"AC" + strings.Repeat("0", 32),
		"-----BEGIN PRIVATE KEY-----\nABC\n-----END PRIVATE KEY-----",
		"eyJabcdefghij.eyJabcdefghij.signaturesignature",
		"Authorization: Bearer " + strings.Repeat("j", 40),
		"postgres://user:pass@host/db",
		"https://user:pass@host/path",
		`AWS_SECRET_ACCESS_KEY="` + strings.Repeat("k", 40) + `"`,

		// Marker-shaped inputs to test idempotence.
		"<redacted:openai_api_key>",
		"<redacted:anthropic_api_key> sk-ant-" + strings.Repeat("a", 30),

		// Two adjacent secrets (overlap-resolution path).
		"sk-ant-" + strings.Repeat("a", 30) + " sk-proj-" + strings.Repeat("b", 50),

		// Multi-byte UTF-8 around a secret (offset arithmetic).
		"héllo sk-ant-" + strings.Repeat("a", 30) + " 日本語",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	scanner := Default()

	f.Fuzz(func(t *testing.T, in string) {
		findings := scanner.Scan(in)

		// Property 1: every finding is in-range and non-degenerate.
		for _, fnd := range findings {
			if fnd.Start < 0 || fnd.End > len(in) || fnd.End <= fnd.Start {
				t.Fatalf("finding out of range: %+v len=%d", fnd, len(in))
			}
			if fnd.Pattern == "" {
				t.Fatalf("finding with empty pattern name: %+v", fnd)
			}
		}

		// Property 2: Composite returns findings sorted by Start
		// and non-overlapping. (Documented invariant of Composite.Scan.)
		for i := 1; i < len(findings); i++ {
			if findings[i].Start < findings[i-1].End {
				t.Fatalf("findings overlap: %+v then %+v",
					findings[i-1], findings[i])
			}
		}

		out, names := Replace(in, findings)

		// Property 3: pattern-names slice is sorted, deduped, and
		// matches the set of patterns in findings.
		for i := 1; i < len(names); i++ {
			if names[i] <= names[i-1] {
				t.Fatalf("names not strictly sorted: %v", names)
			}
		}
		want := map[string]struct{}{}
		for _, fnd := range findings {
			want[fnd.Pattern] = struct{}{}
		}
		if len(names) != len(want) {
			t.Fatalf("names %v vs unique patterns in findings %v", names, want)
		}
		for _, n := range names {
			if _, ok := want[n]; !ok {
				t.Fatalf("name %q not in finding patterns %v", n, want)
			}
		}

		// Property 4: Replace produces valid UTF-8 when input is
		// valid UTF-8. (Markers are ASCII; we never split a rune.)
		if utf8.ValidString(in) && !utf8.ValidString(out) {
			t.Fatalf("Replace produced invalid UTF-8 from valid input")
		}

		// Property 5: every redacted byte range is gone from the
		// output. The exact bytes between Start..End in the input
		// must not appear in `out` — otherwise the redaction did
		// nothing. We allow false positives (a 1-char secret
		// happens to match a substring of a marker) by skipping
		// secrets shorter than 4 bytes.
		for _, fnd := range findings {
			secret := in[fnd.Start:fnd.End]
			if len(secret) >= 4 && strings.Contains(out, secret) {
				t.Fatalf("secret bytes survived redaction: pattern=%s secret=%q out=%q",
					fnd.Pattern, secret, out)
			}
		}

		// Property 6: idempotence. Re-scanning the redacted output
		// must not find more secrets than the marker count itself
		// would explain. Strong form: a second pass should produce
		// the same output as the first.
		findings2 := scanner.Scan(out)
		out2, _ := Replace(out, findings2)
		if out2 != out {
			t.Fatalf("redaction not idempotent:\nfirst:  %q\nsecond: %q", out, out2)
		}
	})
}
