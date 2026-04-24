package daemon

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// All cases here mutate LISTEN_FDS/LISTEN_PID via t.Setenv — they
// cannot use t.Parallel (process-wide env).

func TestListenFromSystemd_UnsetMeansNotActivated(t *testing.T) {
	t.Setenv("LISTEN_FDS", "")
	t.Setenv("LISTEN_PID", "")
	l, err := ListenFromSystemd()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if l != nil {
		t.Fatalf("expected nil listener, got %v", l)
	}
}

func TestListenFromSystemd_ZeroMeansNotActivated(t *testing.T) {
	t.Setenv("LISTEN_FDS", "0")
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	l, err := ListenFromSystemd()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if l != nil {
		t.Fatalf("expected nil listener, got %v", l)
	}
}

func TestListenFromSystemd_PIDMismatchIsError(t *testing.T) {
	t.Setenv("LISTEN_FDS", "1")
	t.Setenv("LISTEN_PID", "1") // never our pid
	_, err := ListenFromSystemd()
	if err == nil {
		t.Fatal("expected error on pid mismatch")
	}
	if !strings.Contains(err.Error(), "LISTEN_PID") {
		t.Errorf("expected LISTEN_PID in error, got %v", err)
	}
}

func TestListenFromSystemd_MissingPIDIsError(t *testing.T) {
	t.Setenv("LISTEN_FDS", "1")
	t.Setenv("LISTEN_PID", "")
	_, err := ListenFromSystemd()
	if err == nil {
		t.Fatal("expected error when LISTEN_PID absent but LISTEN_FDS set")
	}
}

func TestListenFromSystemd_NonIntegerFDsIsError(t *testing.T) {
	t.Setenv("LISTEN_FDS", "banana")
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	_, err := ListenFromSystemd()
	if err == nil {
		t.Fatal("expected error on non-integer LISTEN_FDS")
	}
}

func TestListenFromSystemd_MultipleFDsIsError(t *testing.T) {
	t.Setenv("LISTEN_FDS", "2")
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	_, err := ListenFromSystemd()
	if err == nil {
		t.Fatal("expected error when LISTEN_FDS != 1")
	}
	if !strings.Contains(err.Error(), "exactly 1") {
		t.Errorf("expected 'exactly 1' in error, got %v", err)
	}
}
