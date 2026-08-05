package events

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/internal/redact"
)

// freshEnv returns a minimal valid Envelope for tests to mutate.
func freshEnv() *Envelope {
	return &Envelope{
		V:               CurrentSchemaVersion,
		EventID:         uuid.Must(uuid.NewV7()).String(),
		SourceAgent:     "claude-code",
		SourceSessionID: "sess-1",
		Kind:            "user_prompt",
		Role:            "user",
		TsSource:        time.Now().UTC(),
		Payload:         map[string]any{},
	}
}

// geminiToolCallShape mirrors the concrete struct the Gemini
// transcript source places at Payload["tool_call"]. The exact fields
// don't matter; what matters is that it is a struct rather than a
// map[string]any, which is what the old walkAny default arm waved
// through unscrubbed.
type geminiToolCallShape struct {
	Name   string   `json:"name"`
	Result []string `json:"result"`
}

// TestApplyRedaction_ScrubsNonGenericPayloadValues is the regression
// gate for the Gemini plaintext leak. walkAny's default arm returned
// unrecognised values untouched, so json.RawMessage content and the
// tool-call struct kept their secrets while ContentText scrubbed
// cleanly and Applied was still set — the Sink's assertion passed and
// the plaintext reached raw_envelopes.
//
// Table covers every non-generic shape a Source can realistically
// store, so a new Source putting a fresh type in Payload is covered
// by the mechanism rather than needing its own case.
func TestApplyRedaction_ScrubsNonGenericPayloadValues(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("a", 36)
	cases := []struct {
		name  string
		value any
	}{
		{"json.RawMessage object", json.RawMessage(`{"text":"tok ` + secret + `"}`)},
		{"json.RawMessage array", json.RawMessage(`[{"text":"tok ` + secret + `"}]`)},
		{"json.RawMessage bare string", json.RawMessage(`"tok ` + secret + `"`)},
		{"concrete struct", geminiToolCallShape{Name: "run", Result: []string{"tok " + secret}}},
		{"pointer to struct", &geminiToolCallShape{Name: "run", Result: []string{"tok " + secret}}},
		{"slice of structs", []geminiToolCallShape{{Name: "run", Result: []string{"tok " + secret}}}},
		{"map with typed values", map[string]string{"out": "tok " + secret}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := freshEnv()
			env.Payload = map[string]any{"value": tc.value}

			ApplyRedaction(env, redact.Default())

			// Marshal the whole envelope: that is what actually gets
			// persisted, so it is the only honest place to assert.
			out, err := json.Marshal(env)
			if err != nil {
				t.Fatalf("marshal envelope: %v", err)
			}
			if strings.Contains(string(out), secret) {
				t.Errorf("secret survived redaction in persisted form:\n%s", out)
			}
			// Match without the angle brackets: encoding/json escapes
			// < and > to < / > in the persisted bytes.
			if !strings.Contains(string(out), "redacted:github_pat_classic") {
				t.Errorf("expected redaction marker in persisted form:\n%s", out)
			}
			if env.Redaction == nil || len(env.Redaction.Patterns) == 0 {
				t.Errorf("pattern list must record the hit, got %+v", env.Redaction)
			}
		})
	}
}

// TestApplyRedaction_PlainByteSliceStaysOpaque documents a deliberate
// limit rather than asserting a guarantee. encoding/json renders a
// plain []byte as base64, so a []byte holding JSON never presents a
// string leaf to scrub — before or after the normalisation change.
// json.RawMessage is the type that declares "these bytes are JSON",
// and that one IS walked (see the table above).
//
// No Source puts a plain []byte in Payload today; both real sources
// use json.RawMessage or a struct. Sniffing []byte for parseable JSON
// would be exactly the kind of heuristic that fails silently in a
// corner, so we don't. If a Source ever needs raw JSON bytes in
// Payload it must use json.RawMessage, and this test says why.
func TestApplyRedaction_PlainByteSliceStaysOpaque(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("a", 36)
	env := freshEnv()
	env.Payload = map[string]any{"value": []byte(`{"text":"tok ` + secret + `"}`)}

	ApplyRedaction(env, redact.Default())

	out, err := json.Marshal(env.Payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if strings.Contains(string(out), secret) {
		t.Errorf("plaintext secret must not appear even in the opaque form:\n%s", out)
	}
	if !strings.Contains(string(out), base64.StdEncoding.EncodeToString(
		[]byte(`{"text":"tok `+secret+`"}`))) {
		t.Errorf("expected the stdlib base64 rendering to be preserved:\n%s", out)
	}
}

// TestApplyRedaction_PreservesNonSecretPayloadShape pins the other
// half of the normalisation contract: round-tripping a value through
// JSON must not change what gets stored.
func TestApplyRedaction_PreservesNonSecretPayloadShape(t *testing.T) {
	t.Parallel()
	env := freshEnv()
	env.Payload = map[string]any{
		"raw":    json.RawMessage(`{"b":[1,2,{"c":"plain text"}],"a":true}`),
		"scalar": 42,
		"nilval": nil,
	}

	ApplyRedaction(env, redact.Default())

	got, err := json.Marshal(env.Payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	want := `{"nilval":null,"raw":{"a":true,"b":[1,2,{"c":"plain text"}]},"scalar":42}`
	if string(got) != want {
		t.Errorf("payload shape changed\n got: %s\nwant: %s", got, want)
	}
}

func TestApplyRedaction_ScrubsContentText(t *testing.T) {
	t.Parallel()
	env := freshEnv()
	env.ContentText = "oops my key is sk-ant-" + strings.Repeat("a", 40)

	ApplyRedaction(env, redact.Default())

	if strings.Contains(env.ContentText, "sk-ant-") {
		t.Errorf("content_text still contains secret: %q", env.ContentText)
	}
	if !strings.Contains(env.ContentText, "<redacted:anthropic_api_key>") {
		t.Errorf("expected marker in content_text: %q", env.ContentText)
	}
	if env.Redaction == nil || !env.Redaction.Applied {
		t.Fatalf("Redaction.Applied must be true")
	}
	if len(env.Redaction.Patterns) != 1 || env.Redaction.Patterns[0] != "anthropic_api_key" {
		t.Errorf("patterns: got %v", env.Redaction.Patterns)
	}
}

func TestApplyRedaction_ScrubsPayloadLeaves(t *testing.T) {
	t.Parallel()
	env := freshEnv()
	env.Payload = map[string]any{
		"tool_input": map[string]any{
			"command":   "curl -H 'Authorization: Bearer " + strings.Repeat("z", 30) + "' https://api",
			"env":       []any{"AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE"},
			"unrelated": 42,
			"nil":       nil,
			"boolean":   true,
		},
	}

	ApplyRedaction(env, redact.Default())

	top := env.Payload["tool_input"].(map[string]any)
	cmd := top["command"].(string)
	if strings.Contains(cmd, "Bearer zzzz") {
		t.Errorf("bearer not scrubbed: %q", cmd)
	}
	if !strings.Contains(cmd, "<redacted:bearer_token>") {
		t.Errorf("expected bearer marker: %q", cmd)
	}
	envList := top["env"].([]any)
	if !strings.Contains(envList[0].(string), "<redacted:aws_access_key>") {
		t.Errorf("expected aws marker in array element: %v", envList[0])
	}
	if top["unrelated"] != 42 || top["boolean"] != true || top["nil"] != nil {
		t.Errorf("non-string leaves mutated: %+v", top)
	}

	pats := env.Redaction.Patterns
	want := []string{"aws_access_key", "bearer_token"}
	if !reflect.DeepEqual(pats, want) {
		t.Errorf("patterns: got %v want %v", pats, want)
	}
}

func TestApplyRedaction_NoFindings_StillMarksApplied(t *testing.T) {
	t.Parallel()
	env := freshEnv()
	env.ContentText = "just a normal prompt with no secrets"
	env.Payload = map[string]any{"foo": "bar", "n": 1}

	ApplyRedaction(env, redact.Default())

	if env.Redaction == nil || !env.Redaction.Applied {
		t.Fatalf("Applied must be true even without findings")
	}
	if len(env.Redaction.Patterns) != 0 {
		t.Errorf("patterns should be empty: got %v", env.Redaction.Patterns)
	}
	if env.ContentText != "just a normal prompt with no secrets" {
		t.Errorf("content_text mutated when no secrets: %q", env.ContentText)
	}
}

func TestApplyRedaction_NilPayload_Safe(t *testing.T) {
	t.Parallel()
	env := freshEnv()
	env.Payload = nil
	env.ContentText = ""

	ApplyRedaction(env, redact.Default())

	if env.Redaction == nil || !env.Redaction.Applied {
		t.Fatalf("Applied must be true")
	}
	if env.Payload != nil {
		t.Errorf("nil payload should remain nil, got %v", env.Payload)
	}
}

func TestApplyRedaction_UnionsPatternsAcrossFields(t *testing.T) {
	t.Parallel()
	env := freshEnv()
	env.ContentText = "sk-ant-" + strings.Repeat("a", 40)
	env.Payload = map[string]any{
		"leaks": []any{
			"AIzaSyA-abcdefghijklmnopqrstuvwxyz12345",
			"AKIAIOSFODNN7EXAMPLE",
		},
	}

	ApplyRedaction(env, redact.Default())

	got := env.Redaction.Patterns
	want := []string{"anthropic_api_key", "aws_access_key", "google_api_key"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("union patterns: got %v want %v", got, want)
	}
}

func TestApplyRedaction_DeeplyNestedPayload(t *testing.T) {
	t.Parallel()
	env := freshEnv()
	env.Payload = map[string]any{
		"a": map[string]any{
			"b": []any{
				map[string]any{
					"c": "token AKIAIOSFODNN7EXAMPLE buried deep",
				},
			},
		},
	}

	ApplyRedaction(env, redact.Default())

	a := env.Payload["a"].(map[string]any)
	b := a["b"].([]any)
	c := b[0].(map[string]any)
	leaf := c["c"].(string)
	if !strings.Contains(leaf, "<redacted:aws_access_key>") {
		t.Errorf("deep leaf not scrubbed: %q", leaf)
	}
}
