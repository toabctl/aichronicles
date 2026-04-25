package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCmd_PrintsVersionAndGoLine(t *testing.T) {
	t.Parallel()
	cmd := newVersionCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "aichronicles "+Version) {
		t.Errorf("missing version line in:\n%s", got)
	}
	// runtime.Version() always starts with "go" in our test environment.
	if !strings.Contains(got, "go:") || !strings.Contains(got, "go1.") {
		t.Errorf("missing go toolchain line in:\n%s", got)
	}
}
