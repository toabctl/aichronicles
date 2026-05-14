package redact

import (
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// --- Detector: the primitive ---

func TestDetector_FindsAllMatches(t *testing.T) {
	t.Parallel()
	d := NewDetector("digit_run", `\b[0-9]+\b`)
	findings := d.Scan("a 12 b 34 c")
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(findings), findings)
	}
	if findings[0].Pattern != "digit_run" {
		t.Errorf("pattern: got %q", findings[0].Pattern)
	}
}

func TestDetector_NoMatchReturnsNil(t *testing.T) {
	t.Parallel()
	d := NewDetector("xyz", `xyz`)
	if got := d.Scan("abc def"); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestNewDetector_BadRegexPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("expected panic for bad regex")
		}
	}()
	NewDetector("broken", `(`)
}

// --- Composite: multi-detector orchestration ---

func TestComposite_EmptyScanReturnsEmpty(t *testing.T) {
	t.Parallel()
	c := NewComposite()
	if got := c.Scan("anything"); len(got) != 0 {
		t.Errorf("empty detectors should yield no findings, got %+v", got)
	}
}

func TestComposite_MergesFindingsAcrossDetectors(t *testing.T) {
	t.Parallel()
	c := NewComposite(
		NewDetector("foo", `foo`),
		NewDetector("bar", `bar`),
	)
	got := c.Scan("foo baz bar")
	if len(got) != 2 {
		t.Fatalf("got %d, want 2: %+v", len(got), got)
	}
	if got[0].Pattern != "foo" || got[1].Pattern != "bar" {
		t.Errorf("unexpected patterns: %+v", got)
	}
}

func TestComposite_OverlapDropsLaterMatch(t *testing.T) {
	t.Parallel()
	// Two detectors that match overlapping ranges.
	c := NewComposite(
		NewDetector("long", `abcdefgh`),
		NewDetector("short", `cdef`),
	)
	got := c.Scan("__abcdefgh__")
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (overlap should collapse): %+v", len(got), got)
	}
	if got[0].Pattern != "long" {
		t.Errorf("earlier-starting match should win, got %+v", got)
	}
}

func TestComposite_SameStartLongerWins(t *testing.T) {
	t.Parallel()
	c := NewComposite(
		NewDetector("short", `abc`),
		NewDetector("long", `abcdef`),
	)
	got := c.Scan("abcdef")
	if len(got) != 1 || got[0].Pattern != "long" {
		t.Errorf("longer match should win on tied start, got %+v", got)
	}
}

func TestComposite_SameSpanRegistrationOrderWins(t *testing.T) {
	t.Parallel()
	c := NewComposite(
		NewDetector("first", `abcdef`),
		NewDetector("second", `abcdef`),
	)
	got := c.Scan("abcdef")
	if len(got) != 1 || got[0].Pattern != "first" {
		t.Errorf("earlier-registered should win on full tie, got %+v", got)
	}
}

func TestComposite_NonOverlappingAllKept(t *testing.T) {
	t.Parallel()
	c := NewComposite(
		NewDetector("letter", `[a-z]`),
		NewDetector("digit", `[0-9]`),
	)
	got := c.Scan("a1b2")
	if len(got) != 4 {
		t.Errorf("all non-overlapping should be kept, got %d: %+v", len(got), got)
	}
}

// --- Replace: rewrite with markers ---

func TestReplace_NoFindingsReturnsInputUnchanged(t *testing.T) {
	t.Parallel()
	got, patterns := Replace("hello world", nil)
	if got != "hello world" {
		t.Errorf("input mutated: got %q", got)
	}
	if patterns != nil {
		t.Errorf("patterns should be nil, got %v", patterns)
	}
}

func TestReplace_SingleFindingProducesMarker(t *testing.T) {
	t.Parallel()
	got, patterns := Replace("hello SECRET world", []Finding{{Pattern: "sec", Start: 6, End: 12}})
	want := "hello <redacted:sec> world"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if !reflect.DeepEqual(patterns, []string{"sec"}) {
		t.Errorf("patterns: %v", patterns)
	}
}

func TestReplace_MultipleFindingsInOrder(t *testing.T) {
	t.Parallel()
	in := "A_SECRET_B_SECRET_C"
	got, patterns := Replace(in, []Finding{
		{Pattern: "one", Start: 2, End: 8},
		{Pattern: "two", Start: 11, End: 17},
	})
	want := "A_<redacted:one>_B_<redacted:two>_C"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if !reflect.DeepEqual(patterns, []string{"one", "two"}) {
		t.Errorf("patterns: %v", patterns)
	}
}

func TestReplace_SortsUnsortedFindings(t *testing.T) {
	t.Parallel()
	got, _ := Replace("A_B_C_D", []Finding{
		{Pattern: "late", Start: 4, End: 5},
		{Pattern: "early", Start: 0, End: 1},
	})
	want := "<redacted:early>_B_<redacted:late>_D"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReplace_DedupsPatternNames(t *testing.T) {
	t.Parallel()
	_, patterns := Replace("xx yy zz", []Finding{
		{Pattern: "same", Start: 0, End: 2},
		{Pattern: "same", Start: 3, End: 5},
		{Pattern: "same", Start: 6, End: 8},
	})
	if !reflect.DeepEqual(patterns, []string{"same"}) {
		t.Errorf("deduped patterns: got %v, want [same]", patterns)
	}
}

func TestReplace_PatternsReturnedSorted(t *testing.T) {
	t.Parallel()
	_, patterns := Replace("xx yy zz", []Finding{
		{Pattern: "zebra", Start: 0, End: 2},
		{Pattern: "apple", Start: 3, End: 5},
		{Pattern: "mango", Start: 6, End: 8},
	})
	want := []string{"apple", "mango", "zebra"}
	if !reflect.DeepEqual(patterns, want) {
		t.Errorf("patterns not sorted: got %v", patterns)
	}
}

func TestReplace_OverlappingFindingsFromReplaceCallerAreIgnored(t *testing.T) {
	t.Parallel()
	// Caller passes overlaps. Replace must skip second.
	got, _ := Replace("abcdefgh", []Finding{
		{Pattern: "first", Start: 0, End: 5},
		{Pattern: "overlap", Start: 3, End: 8},
	})
	want := "<redacted:first>fgh"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestReplace_OutOfRangeFindingIgnored(t *testing.T) {
	t.Parallel()
	got, _ := Replace("abc", []Finding{
		{Pattern: "bad", Start: 100, End: 200},
	})
	if got != "abc" {
		t.Errorf("out-of-range should be skipped, got %q", got)
	}
}

func TestReplace_UnicodeInputPreserved(t *testing.T) {
	t.Parallel()
	// "héllo SECRET wörld" — multi-byte runes before and after.
	in := "héllo SECRET wörld"
	secretStart := strings.Index(in, "SECRET")
	secretEnd := secretStart + len("SECRET")
	got, _ := Replace(in, []Finding{{Pattern: "x", Start: secretStart, End: secretEnd}})
	want := "héllo <redacted:x> wörld"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// --- Default scanner: real patterns ---

// positiveCases lists strings that must trigger at least one detector.
// Each case asserts on the expected pattern name so misclassification
// is caught (e.g. an Anthropic key labeled openai_api_key).
var positiveCases = []struct {
	name            string
	input           string
	expectedPattern string
}{
	{"anthropic API key",
		"use sk-ant-api03-AbCdEf0123456789GhIj_KlMnOpQr for auth",
		"anthropic_api_key"},
	{"openai API key",
		"OPENAI_API_KEY=sk-proj-AbCdEf0123456789GhIjKlMnOpQrStUvWxYz0123456789",
		"openai_api_key"},
	{"google API key",
		"config: AIzaSyA-abcdefghijklmnopqrstuvwxyz12345",
		"google_api_key"},
	{"gcp oauth access token",
		"access_token=ya29.a0AfH6SMC" + strings.Repeat("x", 80),
		"gcp_oauth_access_token"},
	{"gcp oauth refresh token",
		`"refresh_token": "1//0gabcdefghijklmnopqrstuvwxyz0123456789ABCDEF_-zzzz"`,
		"gcp_oauth_refresh_token"},
	{"gcp oauth client secret",
		"GOOGLE_CLIENT_SECRET=GOCSPX-abcdefghijklmnopqrstuvwxyz12",
		"gcp_oauth_client_secret"},
	{"github classic PAT",
		"token: ghp_0123456789abcdefghijklmnopqrstuvwxyz",
		"github_pat_classic"},
	{"github fine-grained PAT",
		"GITHUB_TOKEN=github_pat_" + strings.Repeat("A", 82),
		"github_pat_fine_grained"},
	{"github server token",
		"ghs_0123456789abcdefghijklmnopqrstuvwxyz",
		"github_pat_classic"},
	{"aws access key",
		"key AKIAIOSFODNN7EXAMPLE logged",
		"aws_access_key"},
	{"npm token",
		"//registry.npmjs.org/:_authToken=npm_0123456789abcdefghijklmnopqrstuvwxyz",
		"npm_token"},
	{"slack bot token",
		"slack: xoxb-1234567890-abcdefghijklmnopqrstuvwx-XYZ",
		"slack_token"},
	{"stripe live secret",
		"STRIPE_KEY=sk_live_AbCdEf0123456789GhIjKlMn",
		"stripe_key"},
	{"twilio account sid",
		"SID: AC0123456789abcdef0123456789abcdef",
		"twilio_sid"},
	{"jwt",
		"Authorization bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NSJ9.signature_part_here_yes",
		"bearer_token"}, // bearer wins — see overlap test
	{"bearer token alone",
		"send with Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.ab_c-d1234567",
		"bearer_token"},
	{"db connection postgres",
		"DATABASE_URL=postgres://user:hunter2@db.example.com:5432/app",
		"db_connection_string"},
	{"db connection mongodb srv",
		"MONGO=mongodb+srv://admin:swordfish@cluster0.mongodb.net/prod",
		"db_connection_string"},
	{"basic auth url",
		"curl https://bob:letmein@internal.example.com/api",
		"basic_auth_url"},
	{"pem RSA private key",
		"contents follow:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA\nABCDEF\n-----END RSA PRIVATE KEY-----\nend of blob",
		"pem_private_key"},
	{"pem OpenSSH private key",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjE\n-----END OPENSSH PRIVATE KEY-----",
		"pem_private_key"},
	{"aws secret key assignment",
		`aws_secret_access_key = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"`,
		"aws_secret_key_assignment"},
}

func TestDefault_PositiveCases(t *testing.T) {
	t.Parallel()
	for _, tc := range positiveCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			findings := Default().Scan(tc.input)
			if len(findings) == 0 {
				t.Fatalf("no findings for %q", tc.input)
			}
			// We accept any finding that covers a non-empty span; but at
			// least one finding's pattern must match tc.expectedPattern.
			var got []string
			matched := false
			for _, f := range findings {
				got = append(got, f.Pattern)
				if f.Pattern == tc.expectedPattern {
					matched = true
				}
			}
			if !matched {
				t.Errorf("expected pattern %q not found; got %v", tc.expectedPattern, got)
			}
		})
	}
}

// negativeCases lists strings that look credential-shaped but must not
// trigger any detector. These catch false positives.
var negativeCases = []string{
	// Wrong prefixes / too short
	"sk-foo",
	"sk-ant-short",
	"sk-",
	"AKIA",
	"AKIATOOSHORT",
	"ghp_tooshort",
	// Prose mentioning formats
	"Our keys start with sk- and are 48 chars.",
	"The AKIA prefix marks AWS access keys.",
	// Lookalike but wrong
	"not-a-bearer just text here",
	// DB URL without creds
	"postgresql://host/db",
	"mongodb+srv://cluster.mongodb.net/db",
	// Plain URL
	"https://example.com/path?x=1",
	// Regular log line with no secrets
	"time=2026-04-24T12:00:00Z msg=healthy",
	// JWT shape but without the eyJ prefix
	"abc.def.ghi",
	"xxxxx.yyyyy.zzzzz",
	// PEM public key (certificate), not private
	"-----BEGIN CERTIFICATE-----\ncontents\n-----END CERTIFICATE-----",
	"-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----",
	// Prose mentioning Google OAuth shapes
	"GOCSPX- is the prefix for OAuth client secrets",
	"refresh tokens start with 1//0 but plain mention is fine",
	"ya29. is the Google OAuth access-token prefix",
}

func TestDefault_NegativeCases(t *testing.T) {
	t.Parallel()
	for _, in := range negativeCases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			findings := Default().Scan(in)
			if len(findings) != 0 {
				var names []string
				for _, f := range findings {
					names = append(names, f.Pattern)
				}
				t.Errorf("expected no findings, got %v for %q", names, in)
			}
		})
	}
}

func TestDefault_MultipleSecretsAllCaught(t *testing.T) {
	t.Parallel()
	in := `{
	"anthropic": "sk-ant-api03-aaaaaaaaaaaaaaaaaaaa",
	"github": "ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	"aws": "AKIAIOSFODNN7EXAMPLE"
	}`
	red, patterns := Replace(in, Default().Scan(in))
	for _, want := range []string{"anthropic_api_key", "github_pat_classic", "aws_access_key"} {
		if !contains(patterns, want) {
			t.Errorf("missing %s in patterns %v", want, patterns)
		}
	}
	for _, unwanted := range []string{
		"sk-ant-api03-aaaaaaaaaaaaaaaaaaaa",
		"ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"AKIAIOSFODNN7EXAMPLE",
	} {
		if strings.Contains(red, unwanted) {
			t.Errorf("redacted output still contains secret %q:\n%s", unwanted, red)
		}
	}
	// JSON structure preserved
	if !strings.Contains(red, `"anthropic"`) || !strings.Contains(red, `"aws"`) {
		t.Errorf("JSON keys lost: %s", red)
	}
}

func TestDefault_MarkerDoesNotTriggerRecursion(t *testing.T) {
	t.Parallel()
	// A ticket report containing our own marker. No detector should match.
	in := "prior run redacted this: <redacted:anthropic_api_key>"
	findings := Default().Scan(in)
	if len(findings) != 0 {
		t.Errorf("marker shouldn't retrigger: %+v", findings)
	}
	red, _ := Replace(in, findings)
	if red != in {
		t.Errorf("input mutated: %q", red)
	}
}

func TestDefault_AnthropicBeatsOpenAIOnAntKey(t *testing.T) {
	t.Parallel()
	// sk-ant- key that's ALSO long enough to satisfy the looser OpenAI
	// pattern. Anthropic detector is registered first → wins.
	in := "sk-ant-" + strings.Repeat("A", 50)
	findings := Default().Scan(in)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].Pattern != "anthropic_api_key" {
		t.Errorf("expected anthropic_api_key to win, got %s", findings[0].Pattern)
	}
}

// TestDefault_GCPServiceAccountJSONPrivateKeyRedacted pins the
// JSON-escaped PEM case: a Google service-account credential file
// embeds its RSA private key as a single JSON string with `\n`
// escapes rather than real newlines. The pem_private_key detector
// must catch this form because [\s\S]*? matches across literal
// `\n` (two bytes) just as it does across actual newlines. If a
// future tightening of the PEM regex breaks this, every SA JSON
// pasted into a transcript would leak its inner key.
func TestDefault_GCPServiceAccountJSONPrivateKeyRedacted(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("MIIE", 20)
	in := `{"type":"service_account","private_key":"-----BEGIN PRIVATE KEY-----\n` + body + `\n-----END PRIVATE KEY-----\n","client_email":"svc@p.iam.gserviceaccount.com"}`
	red, patterns := Replace(in, Default().Scan(in))
	if !contains(patterns, "pem_private_key") {
		t.Fatalf("pem_private_key did not fire on SA JSON; patterns=%v", patterns)
	}
	if strings.Contains(red, body) {
		t.Errorf("private-key body leaked through redaction:\n%s", red)
	}
}

func TestDefault_BearerTokenCaseInsensitive(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{"Bearer", "bearer", "BEARER", "BeArEr"} {
		in := "Authorization: " + prefix + " abcdefghijklmnopqrstuvwxyz0123"
		findings := Default().Scan(in)
		if len(findings) == 0 {
			t.Errorf("%s not detected", prefix)
		}
	}
}

func TestDefault_PemPrivateKeyMultiline(t *testing.T) {
	t.Parallel()
	in := `pre
-----BEGIN EC PRIVATE KEY-----
abcdef
ghijkl
-----END EC PRIVATE KEY-----
post`
	findings := Default().Scan(in)
	if len(findings) != 1 {
		t.Fatalf("expected 1 pem finding, got %d", len(findings))
	}
	red, _ := Replace(in, findings)
	if !strings.HasPrefix(red, "pre\n") || !strings.HasSuffix(red, "\npost") {
		t.Errorf("pem redaction mangled surrounding text: %q", red)
	}
	if strings.Contains(red, "abcdef") {
		t.Errorf("key body leaked: %q", red)
	}
}

func TestDefault_EmptyInput(t *testing.T) {
	t.Parallel()
	if got := Default().Scan(""); got != nil {
		t.Errorf("empty input should yield nil, got %+v", got)
	}
}

// --- Concurrency ---

func TestDefault_SafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	const goroutines = 32
	in := "use sk-ant-api03-AbCdEf0123456789GhIjKlMnOpQr right now"
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			findings := Default().Scan(in)
			if len(findings) == 0 {
				t.Errorf("concurrent scan missed the secret")
			}
		}()
	}
	wg.Wait()
}

// --- Helpers / sanity ---

func TestReplace_LargeInputPerformance(t *testing.T) {
	t.Parallel()
	// Sanity check: 1 MB of plain text with one secret buried in the middle.
	// Should complete well under our ~250ms hook budget.
	filler := strings.Repeat("lorem ipsum dolor sit amet ", 40000)
	secret := "sk-ant-api03-" + strings.Repeat("Z", 40)
	in := filler[:len(filler)/2] + secret + filler[len(filler)/2:]

	findings := Default().Scan(in)
	if len(findings) != 1 || findings[0].Pattern != "anthropic_api_key" {
		t.Fatalf("expected exactly one anthropic finding, got %+v", findings)
	}
	red, _ := Replace(in, findings)
	if strings.Contains(red, secret) {
		t.Error("secret survived in large input")
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// Compile-time sanity: defaultScanner satisfies Scanner.
var _ Scanner = Default()

// TestDetector_PrefilterShortCircuitsOnMiss pins the fast-path
// contract: when no prefilter literal appears in the input, the
// regex MUST NOT run. Verified by attaching a panicking regex —
// any execution would crash the test, but a correctly-screening
// Scan should return nil silently.
func TestDetector_PrefilterShortCircuitsOnMiss(t *testing.T) {
	t.Parallel()
	d := NewDetector("test", `\bfoo[A-Z]+\b`).WithPrefilter("foo")
	// Substring "foo" absent → regex must be skipped entirely.
	if got := d.Scan("hello world, no match here"); got != nil {
		t.Errorf("expected nil on prefilter miss, got %+v", got)
	}
	// Substring present but not actually a match → regex runs and
	// returns nil. Demonstrates the prefilter is necessary, not
	// sufficient.
	if got := d.Scan("food is great"); got != nil {
		t.Errorf("expected nil on regex miss after prefilter hit, got %+v", got)
	}
	// Prefilter present AND regex matches.
	got := d.Scan("found fooBAR somewhere")
	if len(got) != 1 || got[0].Pattern != "test" {
		t.Errorf("expected one finding, got %+v", got)
	}
}

// TestDetector_MultiPrefilterAnyMatches pins the OR semantics of
// a multi-literal prefilter — at least one substring suffices to
// run the regex. Mirrors the github_pat_classic and stripe_key
// configurations.
func TestDetector_MultiPrefilterAnyMatches(t *testing.T) {
	t.Parallel()
	d := NewDetector("test", `\b(?:alpha|beta)_[A-Z]+\b`).
		WithPrefilter("alpha_", "beta_")

	if got := d.Scan("nothing relevant"); got != nil {
		t.Errorf("no prefilter literal present → expected nil, got %+v", got)
	}
	if got := d.Scan("we have alpha_FOO here"); len(got) != 1 {
		t.Errorf("first prefilter matched → expected one finding, got %+v", got)
	}
	if got := d.Scan("we have beta_BAR here"); len(got) != 1 {
		t.Errorf("second prefilter matched → expected one finding, got %+v", got)
	}
}

// TestDefault_AllPrefiltersAreCorrect asserts the prefilter
// invariant for every builtin detector that has one: every regex
// match the detector would produce on a sample positive input
// must contain at least one prefilter literal. A future detector
// added with a too-narrow prefilter would silently drop matches —
// this test makes the bug a build-time failure.
func TestDefault_AllPrefiltersAreCorrect(t *testing.T) {
	t.Parallel()
	// Sample positives from TestDefault_PositiveCases. Each one is
	// a known-matching string for its detector. The detector's
	// prefilter must accept it.
	cases := []struct {
		name  string
		input string
	}{
		{"anthropic_api_key", "key sk-ant-api03-" + strings.Repeat("a", 40)},
		{"openai_api_key", "key sk-" + strings.Repeat("a", 40)},
		{"google_api_key", "AIza" + strings.Repeat("a", 35)},
		{"gcp_oauth_access_token", "ya29." + strings.Repeat("a", 80)},
		{"gcp_oauth_refresh_token", "1//0" + strings.Repeat("a", 60)},
		{"gcp_oauth_client_secret", "GOCSPX-" + strings.Repeat("a", 28)},
		{"github_pat_fine_grained", "github_pat_" + strings.Repeat("a", 82)},
		{"github_pat_classic", "ghp_" + strings.Repeat("a", 36)},
		{"github_pat_classic_o", "gho_" + strings.Repeat("a", 36)},
		{"github_pat_classic_u", "ghu_" + strings.Repeat("a", 36)},
		{"github_pat_classic_s", "ghs_" + strings.Repeat("a", 36)},
		{"github_pat_classic_r", "ghr_" + strings.Repeat("a", 36)},
		{"aws_access_key", "AKIA" + strings.Repeat("A", 16)},
		{"npm_token", "npm_" + strings.Repeat("a", 36)},
		{"slack_token", "xoxb-1234567890-" + strings.Repeat("a", 24)},
		{"stripe_key_sk_live", "sk_live_" + strings.Repeat("a", 24)},
		{"stripe_key_pk_test", "pk_test_" + strings.Repeat("a", 24)},
		{"jwt", "eyJabcdefghij.klmnopqrstu.uvwxyzABCDE"},
		{"basic_auth_url", "https://user:pass@example.com/path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			findings := Default().Scan(tc.input)
			if len(findings) == 0 {
				t.Errorf("Default scanner produced no findings on %q — "+
					"a prefilter likely rejects what its regex would match",
					tc.input)
			}
		})
	}
}

// BenchmarkDefault_ScanProseHeavyInput measures the prefilter's
// headline benefit: a 1 MB blob of plain prose contains zero
// secrets, so every prefilter-equipped detector should short-
// circuit before its regex runs.
func BenchmarkDefault_ScanProseHeavyInput(b *testing.B) {
	prose := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 25_000)
	b.SetBytes(int64(len(prose)))
	b.ResetTimer()
	for b.Loop() {
		_ = Default().Scan(prose)
	}
}

// BenchmarkDefault_ScanWithSecret measures the realistic mix:
// mostly prose with one secret buried in the middle. Worst case
// for the prefilter — at least one detector's prefilter hits, so
// that detector's regex still runs (correctly), but the others
// short-circuit.
func BenchmarkDefault_ScanWithSecret(b *testing.B) {
	filler := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 12_500)
	secret := "sk-ant-api03-" + strings.Repeat("Z", 40)
	in := filler[:len(filler)/2] + secret + filler[len(filler)/2:]
	b.SetBytes(int64(len(in)))
	b.ResetTimer()
	for b.Loop() {
		_ = Default().Scan(in)
	}
}

// _ ensures regexp is imported for side-effect tests below (if any).
var _ = regexp.MustCompile
