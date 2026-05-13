package cli

// ptrTo returns a pointer to v. Test-only helper for constructing
// store struct literals that carry optional *string / *int64
// fields. Mirrors the same-named helpers in internal/store and
// internal/web (Go test packages can't share types across
// packages, so each test package keeps its own copy).
func ptrTo[T any](v T) *T { return &v }
