package extract

import (
	"reflect"
	"sort"
	"testing"

	"github.com/toabctl/aichronicles/pkg/ingest"
)

// kindValue is a compact (kind, value) pair used for assertion on
// extractor output when exact ordering doesn't matter.
type kindValue struct{ kind, value string }

func toKV(xs []Extraction) []kindValue {
	out := make([]kindValue, len(xs))
	for i, x := range xs {
		out[i] = kindValue{x.Kind, x.Value}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].kind != out[j].kind {
			return out[i].kind < out[j].kind
		}
		return out[i].value < out[j].value
	})
	return out
}

func TestURL_SingleMatch(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{ContentText: "see https://example.com for details"}
	got := toKV(FromEnvelope(env))
	want := []kindValue{{KindURL, "https://example.com"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestURL_MultipleDeduped(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{ContentText: `
		first https://foo.dev/a
		second: https://bar.dev/b?x=1
		first again https://foo.dev/a
	`}
	got := toKV(FromEnvelope(env))
	want := []kindValue{
		{KindURL, "https://bar.dev/b?x=1"},
		{KindURL, "https://foo.dev/a"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestURL_TrailingPunctuationStripped(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{ContentText: "check https://example.com/a/b."}
	got := toKV(FromEnvelope(env))
	want := []kindValue{{KindURL, "https://example.com/a/b"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestURL_HTTPAlsoMatched(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{ContentText: "legacy http://old.example/"}
	got := toKV(FromEnvelope(env))
	want := []kindValue{{KindURL, "http://old.example/"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestURL_FTPIsNotExtracted(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{ContentText: "ftp://anon.example/ and file:///tmp/x"}
	got := FromEnvelope(env)
	if len(got) != 0 {
		t.Errorf("expected 0 URL extractions, got %v", got)
	}
}

func TestURL_NoContentNoExtractions(t *testing.T) {
	t.Parallel()
	got := FromEnvelope(&ingest.Envelope{})
	if len(got) != 0 {
		t.Errorf("empty envelope should produce no extractions, got %v", got)
	}
}

func TestFilePath_ReadToolExtractsFileField(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "Read"},
		Payload: map[string]any{
			"tool_input": map[string]any{
				"file_path": "/home/user/project/main.go",
			},
		},
	}
	got := toKV(FromEnvelope(env))
	want := []kindValue{{KindFilePath, "/home/user/project/main.go"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilePath_WriteAndEditAlsoExtract(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{"Write", "Edit", "NotebookEdit"} {
		env := &ingest.Envelope{
			Tool: &ingest.Tool{Name: tool},
			Payload: map[string]any{
				"tool_input": map[string]any{"file_path": "/p.go"},
			},
		}
		got := toKV(FromEnvelope(env))
		want := []kindValue{{KindFilePath, "/p.go"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("tool %s: got %v, want %v", tool, got, want)
		}
	}
}

func TestFilePath_RelativePathJoinedWithCwd(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "Edit"},
		Cwd:  "/home/me/repo",
		Payload: map[string]any{
			"tool_input": map[string]any{
				"file_path": "internal/store/migrate.go",
			},
		},
	}
	got := toKV(FromEnvelope(env))
	want := []kindValue{{KindFilePath, "/home/me/repo/internal/store/migrate.go"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilePath_AbsolutePathPassesThroughUntouched(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "Read"},
		Cwd:  "/home/me/repo",
		Payload: map[string]any{
			"tool_input": map[string]any{
				"file_path": "/etc/hosts",
			},
		},
	}
	got := toKV(FromEnvelope(env))
	want := []kindValue{{KindFilePath, "/etc/hosts"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilePath_RelativePathNoCwdKeptAsIs(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "Read"},
		// Cwd intentionally empty — agent didn't supply one.
		Payload: map[string]any{
			"tool_input": map[string]any{
				"file_path": "config.go",
			},
		},
	}
	got := toKV(FromEnvelope(env))
	// Best-effort fallback: with no anchor, store the literal.
	// Better than dropping the row outright.
	want := []kindValue{{KindFilePath, "config.go"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilePath_NonFileToolSkipped(t *testing.T) {
	t.Parallel()
	// Grep does not emit a canonical file_path — skip even if present.
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "Grep"},
		Payload: map[string]any{
			"tool_input": map[string]any{"file_path": "/p.go"},
		},
	}
	got := FromEnvelope(env)
	if len(got) != 0 {
		t.Errorf("Grep file_path shouldn't be extracted: %v", got)
	}
}

func TestFilePath_MissingFieldSkipped(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "Read"},
		Payload: map[string]any{
			"tool_input": map[string]any{"not_a_file": "/p.go"},
		},
	}
	got := FromEnvelope(env)
	if len(got) != 0 {
		t.Errorf("missing file_path shouldn't extract: %v", got)
	}
}

func TestShellCommand_BashExtractsCommand(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "Bash"},
		Payload: map[string]any{
			"tool_input": map[string]any{
				"command":     "npm test",
				"description": "run unit tests",
			},
		},
	}
	got := FromEnvelope(env)
	if len(got) != 1 {
		t.Fatalf("got %d extractions, want 1: %v", len(got), got)
	}
	if got[0].Kind != KindShellCommand || got[0].Value != "npm test" {
		t.Errorf("got %+v", got[0])
	}
	if got[0].Extra["description"] != "run unit tests" {
		t.Errorf("description not attached to Extra: %+v", got[0])
	}
}

func TestShellCommand_NonBashToolSkipped(t *testing.T) {
	t.Parallel()
	// Some other tool happens to carry a "command" — don't extract.
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "Read"},
		Payload: map[string]any{
			"tool_input": map[string]any{"command": "vim"},
		},
	}
	got := FromEnvelope(env)
	if len(got) != 0 {
		t.Errorf("non-Bash command shouldn't extract: %v", got)
	}
}

func TestShellCommand_MissingCommandSkipped(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "Bash"},
		Payload: map[string]any{
			"tool_input": map[string]any{"description": "stub"},
		},
	}
	got := FromEnvelope(env)
	if len(got) != 0 {
		t.Errorf("Bash w/o command shouldn't extract: %v", got)
	}
}

func TestShellCommand_NoDescriptionOmitsExtra(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "Bash"},
		Payload: map[string]any{
			"tool_input": map[string]any{"command": "ls"},
		},
	}
	got := FromEnvelope(env)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].Extra != nil {
		t.Errorf("Extra should be nil without description, got %v", got[0].Extra)
	}
}

func TestMixed_EnvelopeWithMultipleExtractions(t *testing.T) {
	t.Parallel()
	// Contrived but legal: a Bash tool_use with a URL in its
	// content_text (e.g. the flattened command description). We
	// expect both a URL and a shell_command extraction.
	env := &ingest.Envelope{
		ContentText: "curl https://api.example.com/healthz",
		Tool:        &ingest.Tool{Name: "Bash"},
		Payload: map[string]any{
			"tool_input": map[string]any{
				"command": "curl https://api.example.com/healthz",
			},
		},
	}
	got := toKV(FromEnvelope(env))
	want := []kindValue{
		{KindShellCommand, "curl https://api.example.com/healthz"},
		{KindURL, "https://api.example.com/healthz"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFromEnvelope_NilReturnsNil(t *testing.T) {
	t.Parallel()
	if got := FromEnvelope(nil); got != nil {
		t.Errorf("nil envelope should return nil, got %v", got)
	}
}

func TestRegistry_FromEnvelopeHonorsRegisteredOrder(t *testing.T) {
	// Swap Registered for a custom slice that appends a sentinel kind,
	// verify FromEnvelope returns only what we ask for. Not t.Parallel:
	// Registered is package-global.
	orig := Registered
	defer func() { Registered = orig }()

	Registered = []Extractor{
		func(env *ingest.Envelope) []Extraction {
			return []Extraction{{Kind: "first", Value: "1"}}
		},
		func(env *ingest.Envelope) []Extraction {
			return []Extraction{{Kind: "second", Value: "2"}}
		},
	}

	got := toKV(FromEnvelope(&ingest.Envelope{}))
	want := []kindValue{{"first", "1"}, {"second", "2"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRegistry_EmptySliceYieldsEmpty(t *testing.T) {
	orig := Registered
	defer func() { Registered = orig }()
	Registered = nil

	got := FromEnvelope(&ingest.Envelope{ContentText: "https://example.com"})
	if len(got) != 0 {
		t.Errorf("empty registry should produce no extractions, got %v", got)
	}
}

// Compile-time confirmation the exported extractors satisfy the
// Extractor function type. If someone ever changes a signature this
// test file stops compiling.
var (
	_ Extractor = URLExtractor
	_ Extractor = FilePathExtractor
	_ Extractor = ShellCommandExtractor
	_ Extractor = WebFetchExtractor
	_ Extractor = SkillLoadExtractor
)

func TestWebFetch_URLExtractedAsKindURL(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "WebFetch"},
		Payload: map[string]any{
			"tool_input": map[string]any{"url": "https://example.com/x"},
		},
	}
	got := toKV(FromEnvelope(env))
	// WebFetch reuses KindURL so it joins the existing URL pool;
	// snippets stay labelled `[url] ...` regardless of source.
	want := []kindValue{{KindURL, "https://example.com/x"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// --- SkillLoad ----------------------------------------------------------

// TestSkillLoad_HookShape covers live ingest from the Claude Code
// hook: payload.tool_input.{skill,args} on a tool_use envelope with
// tool_name="Skill". Args is optional and lands under Extra.
func TestSkillLoad_HookShape(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "Skill"},
		Payload: map[string]any{
			"tool_input": map[string]any{
				"skill": "build-test",
				"args":  "--package internal/web",
			},
		},
	}
	got := FromEnvelope(env)
	if len(got) != 1 || got[0].Kind != KindSkillLoad {
		t.Fatalf("got %+v, want one skill_load", got)
	}
	if got[0].Value != "build-test" {
		t.Errorf("value: got %q, want build-test", got[0].Value)
	}
	if !reflect.DeepEqual(got[0].Extra, map[string]any{"args": "--package internal/web"}) {
		t.Errorf("extra: got %+v, want args=…", got[0].Extra)
	}
}

// TestSkillLoad_ImportShape covers transcripts ingested via
// import-claude: the skill name lives at
// payload.message.content[*].input.skill rather than tool_input.
// This is the shape that produced the 50/52 of skill events on the
// dev box, so the regression case lives here.
func TestSkillLoad_ImportShape(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "Skill"},
		Payload: map[string]any{
			"message": map[string]any{
				"content": []any{
					map[string]any{
						"type": "text",
						"text": "I'll use the grade skill.",
					},
					map[string]any{
						"type": "tool_use",
						"name": "Skill",
						"input": map[string]any{
							"skill": "grade",
							"args":  "--repo https://example/x",
						},
					},
				},
			},
		},
	}
	got := FromEnvelope(env)
	if len(got) != 1 || got[0].Kind != KindSkillLoad {
		t.Fatalf("got %+v, want one skill_load", got)
	}
	if got[0].Value != "grade" {
		t.Errorf("value: got %q, want grade", got[0].Value)
	}
	if !reflect.DeepEqual(got[0].Extra, map[string]any{"args": "--repo https://example/x"}) {
		t.Errorf("extra: got %+v", got[0].Extra)
	}
}

// TestSkillLoad_NoArgsOmitsExtra confirms Extra stays nil rather
// than {args:""} when the invocation carried no arguments.
func TestSkillLoad_NoArgsOmitsExtra(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "Skill"},
		Payload: map[string]any{
			"tool_input": map[string]any{"skill": "effective-go"},
		},
	}
	got := FromEnvelope(env)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].Extra != nil {
		t.Errorf("extra should be nil when args absent: %+v", got[0].Extra)
	}
}

// TestSkillLoad_NonSkillToolSkipped confirms the extractor only
// fires on Tool.Name="Skill" — a Bash tool_use with a stray "skill"
// key in tool_input must not produce a skill_load row.
func TestSkillLoad_NonSkillToolSkipped(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "Bash"},
		Payload: map[string]any{
			"tool_input": map[string]any{
				"command": "ls",
				"skill":   "should-not-extract",
			},
		},
	}
	for _, x := range FromEnvelope(env) {
		if x.Kind == KindSkillLoad {
			t.Errorf("non-Skill tool produced skill_load: %+v", x)
		}
	}
}

// TestSkillLoad_MissingSkillFieldSkipped confirms a tool_use envelope
// with Tool.Name="Skill" but no skill string in the payload (drift
// from the documented contract) is silently dropped, not stored as
// a row with empty value.
func TestSkillLoad_MissingSkillFieldSkipped(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "Skill"},
		Payload: map[string]any{
			"tool_input": map[string]any{"args": "stray args, no skill"},
		},
	}
	for _, x := range FromEnvelope(env) {
		if x.Kind == KindSkillLoad {
			t.Errorf("missing skill field should not extract: %+v", x)
		}
	}
}

// TestSkillLoad_ImportShape_NoToolUseBlock confirms that an import
// envelope whose message.content has only text blocks (no tool_use
// of name="Skill") is skipped — defensive against drift in Claude's
// transcript shape.
func TestSkillLoad_ImportShape_NoToolUseBlock(t *testing.T) {
	t.Parallel()
	env := &ingest.Envelope{
		Tool: &ingest.Tool{Name: "Skill"},
		Payload: map[string]any{
			"message": map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": "no tool use here"},
				},
			},
		},
	}
	for _, x := range FromEnvelope(env) {
		if x.Kind == KindSkillLoad {
			t.Errorf("text-only content should not extract: %+v", x)
		}
	}
}
