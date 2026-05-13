package store

// ptrTo returns a pointer to v. Test-only helper for constructing
// struct literals that carry optional *string / *int64 fields
// without the &someVar two-step. The store's exported types use
// pointer-of-T to signal optionality (see arch_review_2026_05_13
// MEDIUM #10); this helper lets test fixtures write
// `Cwd: ptrTo("/x")` instead of declaring a temp variable.
func ptrTo[T any](v T) *T { return &v }
