package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseOutputFormat(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in      string
		want    OutputFormat
		wantErr bool
	}{
		"empty defaults to table":   {in: "", want: FormatTable},
		"explicit table":            {in: "table", want: FormatTable},
		"upper-case TABLE":          {in: "TABLE", want: FormatTable},
		"lower-case json":           {in: "json", want: FormatJSON},
		"upper-case JSON":           {in: "JSON", want: FormatJSON},
		"unknown value errors":      {in: "yaml", wantErr: true},
		"random garbage errors":     {in: "asdf", wantErr: true},
		"whitespace is not trimmed": {in: " json", wantErr: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseOutputFormat(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEmitJSON_IndentsAndSkipsHTMLEscape(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := emitJSON(&buf, map[string]string{"k": "<b>"}); err != nil {
		t.Fatalf("emitJSON: %v", err)
	}
	out := buf.String()
	// Indented (two-space) — newline + spaces inside.
	if !strings.Contains(out, "{\n  \"k\":") {
		t.Errorf("expected indented JSON, got %q", out)
	}
	// HTMLEscape disabled: the literal "<b>" must round-trip.
	if !strings.Contains(out, `"<b>"`) {
		t.Errorf("expected raw <b>, got %q", out)
	}
}
