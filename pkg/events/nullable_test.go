package events

import "testing"

func TestNullString_Ptr(t *testing.T) {
	t.Parallel()
	t.Run("invalid returns nil", func(t *testing.T) {
		t.Parallel()
		if got := (NullString{}).Ptr(); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("valid returns fresh pointer to value", func(t *testing.T) {
		t.Parallel()
		got := NullString{String: "hi", Valid: true}.Ptr()
		if got == nil || *got != "hi" {
			t.Fatalf("got %v, want pointer to %q", got, "hi")
		}
	})
	t.Run("valid empty preserved as pointer to empty", func(t *testing.T) {
		t.Parallel()
		got := NullString{String: "", Valid: true}.Ptr()
		if got == nil || *got != "" {
			t.Fatalf("got %v, want pointer to %q", got, "")
		}
	})
}

func TestNullInt64_Ptr(t *testing.T) {
	t.Parallel()
	t.Run("invalid returns nil", func(t *testing.T) {
		t.Parallel()
		if got := (NullInt64{}).Ptr(); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("valid returns fresh pointer to value", func(t *testing.T) {
		t.Parallel()
		got := NullInt64{Int64: 42, Valid: true}.Ptr()
		if got == nil || *got != 42 {
			t.Fatalf("got %v, want pointer to 42", got)
		}
	})
	t.Run("valid zero preserved as pointer to zero", func(t *testing.T) {
		t.Parallel()
		got := NullInt64{Int64: 0, Valid: true}.Ptr()
		if got == nil || *got != 0 {
			t.Fatalf("got %v, want pointer to 0", got)
		}
	})
}
