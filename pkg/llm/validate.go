package llm

import (
	"errors"
	"fmt"
)

// validateRequest enforces the small set of provider-neutral
// invariants every Client.Complete implementation needs to check
// before issuing the upstream call. Provider-specific validation
// (e.g. Anthropic's role-alternation rule) happens at encode time
// inside the respective SDK.
//
// Adapters MUST call this at the top of their Complete method.
// Skipping it produces obscure provider-side 4xx errors instead of
// our own actionable messages. The function is intentionally
// strict: empty content, non-user opening turn, zero MaxTokens,
// unknown roles all fail loudly so a buggy caller is told exactly
// which message in the slice is wrong.
func validateRequest(req Request) error {
	if len(req.Messages) == 0 {
		return errors.New("Request.Messages is empty")
	}
	if req.Messages[0].Role != RoleUser {
		return errors.New("Request.Messages must start with a user turn")
	}
	if req.MaxTokens <= 0 {
		return errors.New("Request.MaxTokens must be positive")
	}
	for i, m := range req.Messages {
		if m.Role != RoleUser && m.Role != RoleAssistant {
			return fmt.Errorf("Request.Messages[%d].Role %q not recognised", i, m.Role)
		}
		if m.Content == "" {
			return fmt.Errorf("Request.Messages[%d].Content is empty", i)
		}
	}
	return nil
}
