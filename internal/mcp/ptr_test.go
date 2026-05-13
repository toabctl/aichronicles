package mcp

// ptrTo returns a pointer to v. Test-only helper for store struct
// literals with optional *string / *int64 fields.
func ptrTo[T any](v T) *T { return &v }
