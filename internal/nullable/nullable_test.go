package nullable

import (
	"database/sql"
	"testing"
)

func TestOrDash(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   sql.NullString
		want string
	}{
		{"invalid", sql.NullString{Valid: false}, "-"},
		{"valid empty", sql.NullString{Valid: true, String: ""}, "-"},
		{"valid populated", sql.NullString{Valid: true, String: "hello"}, "hello"},
	}
	for _, c := range cases {
		if got := OrDash(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestInt64Ptr(t *testing.T) {
	t.Parallel()
	if got := Int64Ptr(sql.NullInt64{Valid: false}); got != nil {
		t.Errorf("invalid: got %v, want nil", got)
	}
	got := Int64Ptr(sql.NullInt64{Valid: true, Int64: 42})
	if got == nil || *got != 42 {
		t.Errorf("valid: got %v, want *int64(42)", got)
	}
}

func TestStringPtr(t *testing.T) {
	t.Parallel()
	if got := StringPtr(sql.NullString{Valid: false}); got != nil {
		t.Errorf("invalid: got %v, want nil", got)
	}
	// Empty-but-valid stays as "", distinct from nil.
	got := StringPtr(sql.NullString{Valid: true, String: ""})
	if got == nil || *got != "" {
		t.Errorf("valid empty: got %v, want *string(\"\")", got)
	}
	got = StringPtr(sql.NullString{Valid: true, String: "x"})
	if got == nil || *got != "x" {
		t.Errorf("valid populated: got %v, want *string(\"x\")", got)
	}
}
