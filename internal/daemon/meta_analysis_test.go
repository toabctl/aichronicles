package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// The MetaAnalysisSweeper is structurally a clone of
// InductionSweeper with a different log line. The scenarios tested
// here (immediate fire + interval, panic recovery, ctx cancel,
// transient error continuation, nil-callback no-op) are the same
// invariants — duplicated rather than shared because the two
// sweepers may diverge later (one may grow per-tick gating, the
// other may not) and a single shared test would mask that.

func TestMetaAnalysisSweeper_FiresImmediatelyAndOnInterval(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	done := make(chan struct{}, 4)
	sw := &MetaAnalysisSweeper{
		Interval: 30 * time.Millisecond,
		Log:      quietLogger(),
		Sweep: func(_ context.Context) error {
			calls.Add(1)
			select {
			case done <- struct{}{}:
			default:
			}
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go sw.Run(ctx)

	deadline := time.After(500 * time.Millisecond)
	got := 0
	for got < 3 {
		select {
		case <-done:
			got++
		case <-deadline:
			t.Fatalf("expected ≥3 ticks within 500ms, got %d", calls.Load())
		}
	}
	cancel()
}

func TestMetaAnalysisSweeper_RecoversFromPanic(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	done := make(chan struct{}, 4)
	sw := &MetaAnalysisSweeper{
		Interval: 30 * time.Millisecond,
		Log:      quietLogger(),
		Sweep: func(_ context.Context) error {
			n := calls.Add(1)
			if n == 1 {
				panic("boom")
			}
			select {
			case done <- struct{}{}:
			default:
			}
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sw.Run(ctx)

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("sweeper did not recover from panic; calls=%d", calls.Load())
	}
	if calls.Load() < 2 {
		t.Errorf("expected ≥2 calls, got %d", calls.Load())
	}
}

func TestMetaAnalysisSweeper_ExitsOnContextCancel(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	sw := &MetaAnalysisSweeper{
		Interval: 10 * time.Millisecond,
		Log:      quietLogger(),
		Sweep: func(_ context.Context) error {
			calls.Add(1)
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		sw.Run(ctx)
		close(stopped)
	}()
	time.Sleep(40 * time.Millisecond)
	cancel()
	select {
	case <-stopped:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("sweeper did not exit within 500ms after cancel")
	}
}

func TestMetaAnalysisSweeper_LogsSweepErrorsButContinues(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	done := make(chan struct{}, 4)
	sw := &MetaAnalysisSweeper{
		Interval: 30 * time.Millisecond,
		Log:      quietLogger(),
		Sweep: func(_ context.Context) error {
			n := calls.Add(1)
			if n == 1 {
				return errors.New("transient failure")
			}
			select {
			case done <- struct{}{}:
			default:
			}
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sw.Run(ctx)

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("sweeper did not continue after error; calls=%d", calls.Load())
	}
}

func TestMetaAnalysisSweeper_NoOpWithoutCallback(t *testing.T) {
	t.Parallel()
	sw := &MetaAnalysisSweeper{Interval: 10 * time.Millisecond, Log: quietLogger()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		sw.Run(ctx)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("sweeper with nil Sweep did not exit promptly")
	}
}
