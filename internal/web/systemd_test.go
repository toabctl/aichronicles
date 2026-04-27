package web

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// All cases here mutate LISTEN_FDS / LISTEN_PID via t.Setenv —
// runs serial because t.Setenv is incompatible with t.Parallel.

func TestListenFromSystemd_NoEnvIsNoop(t *testing.T) {
	t.Setenv("LISTEN_FDS", "")
	t.Setenv("LISTEN_PID", "")
	l, err := listenFromSystemd()
	if err != nil {
		t.Fatalf("expected (nil, nil), got err %v", err)
	}
	if l != nil {
		t.Fatalf("expected nil listener, got %v", l)
	}
}

func TestListenFromSystemd_ZeroFdsIsNoop(t *testing.T) {
	t.Setenv("LISTEN_FDS", "0")
	l, err := listenFromSystemd()
	if err != nil || l != nil {
		t.Fatalf("zero fds should be a no-op: l=%v err=%v", l, err)
	}
}

func TestListenFromSystemd_RejectsMissingPid(t *testing.T) {
	t.Setenv("LISTEN_FDS", "1")
	t.Setenv("LISTEN_PID", "")
	if _, err := listenFromSystemd(); err == nil {
		t.Fatal("expected error when LISTEN_PID absent but LISTEN_FDS set")
	}
}

func TestListenFromSystemd_RejectsBadFdCount(t *testing.T) {
	t.Setenv("LISTEN_FDS", "banana")
	if _, err := listenFromSystemd(); err == nil {
		t.Fatal("expected error on non-integer LISTEN_FDS")
	}
}

func TestListenFromSystemd_RejectsMultipleFds(t *testing.T) {
	t.Setenv("LISTEN_FDS", "2")
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	if _, err := listenFromSystemd(); err == nil ||
		!strings.Contains(err.Error(), "LISTEN_FDS=2") {
		t.Fatalf("expected single-fd error, got %v", err)
	}
}

func TestListenFromSystemd_RejectsForeignPid(t *testing.T) {
	t.Setenv("LISTEN_FDS", "1")
	// 1 is init; never us.
	t.Setenv("LISTEN_PID", "1")
	if _, err := listenFromSystemd(); err == nil ||
		!strings.Contains(err.Error(), "does not match our pid") {
		t.Fatalf("expected pid-mismatch error, got %v", err)
	}
}
