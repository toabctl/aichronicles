package prompts

// ptrTo returns a pointer to v. Test-only helper for store struct
// literals with optional *string fields. Same four-line shape as
// the ptrTo helper in internal/store and internal/cli.
func ptrTo[T any](v T) *T { return &v }
