package cli

import (
	"testing"

	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/wire"
)

// TestApplyUsage_CarriesCacheCounters is the regression gate for
// undercounted token usage.
//
// Every request marks its system prompt cacheable, and those prompts
// are the large constants — proposeSystem and verifyProposalSystem
// are ~4 KB each. Anthropic reports the cache counters SEPARATELY
// from input_tokens, and the adapter mapped neither, so for a call
// like propose verify (a few hundred user tokens against a 4 KB
// cached block) the recorded input count was a small fraction of the
// truth. That fed llm_outputs, `aichronicles usage`, and every cost
// figure derived from them.
//
// Four commands open-coded this mapping, which is how two new fields
// managed to be dropped at all four sites at once.
func TestApplyUsage_CarriesCacheCounters(t *testing.T) {
	t.Parallel()
	var req wire.SaveLLMOutputRequest
	applyUsage(&req, llm.Usage{
		InputTokens:              120,
		OutputTokens:             340,
		CacheCreationInputTokens: 4096,
		CacheReadInputTokens:     8192,
	})

	for _, tc := range []struct {
		name string
		got  *int64
		want int64
	}{
		{"input", req.InputTokens, 120},
		{"output", req.OutputTokens, 340},
		{"cache write", req.CacheWriteTokens, 4096},
		{"cache read", req.CacheReadTokens, 8192},
	} {
		if tc.got == nil {
			t.Errorf("%s token count was dropped", tc.name)
			continue
		}
		if *tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, *tc.got, tc.want)
		}
	}
}

// TestApplyUsage_ZeroStaysNil preserves the existing distinction
// between "the provider did not report this" and "it reported zero".
func TestApplyUsage_ZeroStaysNil(t *testing.T) {
	t.Parallel()
	var req wire.SaveLLMOutputRequest
	applyUsage(&req, llm.Usage{InputTokens: 5})

	if req.InputTokens == nil || *req.InputTokens != 5 {
		t.Errorf("input should be 5, got %v", req.InputTokens)
	}
	for name, got := range map[string]*int64{
		"output":      req.OutputTokens,
		"cache write": req.CacheWriteTokens,
		"cache read":  req.CacheReadTokens,
	} {
		if got != nil {
			t.Errorf("%s should stay nil when unreported, got %d", name, *got)
		}
	}
}
