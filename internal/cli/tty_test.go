package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestUseColor_BufferIsNotTTY(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if useColor(&buf) {
		t.Error("bytes.Buffer must never be classified as a TTY")
	}
}

func TestStyled_NoTTYReturnsBare(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	got := styled(&buf, "hello", ansiBold)
	if got != "hello" {
		t.Errorf("expected unstyled %q, got %q", "hello", got)
	}
}

func TestUseColor_NoColorEnvDisables(t *testing.T) {
	// Cannot t.Parallel: mutates env.
	t.Setenv(EnvNoColor, "1")
	var buf bytes.Buffer
	if useColor(&buf) {
		t.Error("NO_COLOR must disable colour even on a TTY (and a buffer is already non-TTY)")
	}
}

func TestStyled_NoColorPreservesString(t *testing.T) {
	// Cannot t.Parallel: mutates env.
	t.Setenv(EnvNoColor, "1")
	var buf bytes.Buffer
	got := styled(&buf, "hello", ansiBold)
	if strings.Contains(got, "\x1b[") {
		t.Errorf("NO_COLOR must strip ANSI sequences, got %q", got)
	}
}
