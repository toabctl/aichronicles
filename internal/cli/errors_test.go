package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/store"
)

func TestHintForError(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		err          error
		wantContains string // empty → expect "" hint
	}{
		"nil err yields nothing": {
			err:          nil,
			wantContains: "",
		},
		"unknown error yields nothing": {
			err:          errors.New("unrelated explosion"),
			wantContains: "",
		},
		"no-such-session points at sessions list": {
			err:          fmt.Errorf("summarize: %w", store.ErrNoSuchSession),
			wantContains: "aichronicles sessions",
		},
		"ambiguous prefix asks for more chars": {
			err:          fmt.Errorf("foo: %w", store.ErrAmbiguousSessionPrefix),
			wantContains: "longer prefix",
		},
		"missing anthropic key explains both env and config knob": {
			err:          fmt.Errorf("anthropic: %w (expected in ANTHROPIC_API_KEY)", llm.ErrNoAPIKey),
			wantContains: "api_key_command",
		},
		"missing openai key triggers the same hint": {
			err:          fmt.Errorf("openai: %w (expected in OPENAI_API_KEY)", llm.ErrNoAPIKey),
			wantContains: "api_key_command",
		},
		"daemon socket missing points at setup": {
			err:          fmt.Errorf("post to aichronicles-api: %w", apiclient.ErrSocketUnavailable),
			wantContains: "systemctl --user",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := hintForError(tc.err)
			if tc.wantContains == "" {
				if got != "" {
					t.Errorf("expected no hint, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantContains) {
				t.Errorf("hint missing %q; got %q", tc.wantContains, got)
			}
		})
	}
}
