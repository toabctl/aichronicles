package web

// ptrTo returns a pointer to v. Test-only helper for constructing
// store struct literals that carry optional *string / *int64
// fields. Mirrors the helper of the same name in internal/store
// (Go test packages can't share, so each test package keeps its
// own copy of the four-line helper).
func ptrTo[T any](v T) *T { return &v }
