package cli

import (
	"github.com/toabctl/aichronicles/internal/llm"
	"github.com/toabctl/aichronicles/internal/wire"
)

// applyUsage copies an llm.Usage onto a save request.
//
// Four commands persisted LLM outputs and each open-coded the same
// "if n > 0 { v := int64(n); req.X = &v }" pair. That is how the
// prompt-cache counters came to be dropped everywhere at once: there
// was no single place that knew what "the token counts" meant, so
// adding two more fields would have meant remembering four sites.
//
// A zero count stays nil rather than becoming a stored 0, preserving
// the existing distinction between "not reported" and "reported as
// zero".
func applyUsage(req *wire.SaveLLMOutputRequest, u llm.Usage) {
	req.InputTokens = nonZeroInt64(u.InputTokens)
	req.OutputTokens = nonZeroInt64(u.OutputTokens)
	req.CacheWriteTokens = nonZeroInt64(u.CacheCreationInputTokens)
	req.CacheReadTokens = nonZeroInt64(u.CacheReadInputTokens)
}

func nonZeroInt64(n int) *int64 {
	if n <= 0 {
		return nil
	}
	v := int64(n)
	return &v
}
