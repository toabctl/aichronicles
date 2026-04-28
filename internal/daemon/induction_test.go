package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestInductionSweeper_FiresImmediatelyAndOnInterval(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	done := make(chan struct{}, 4)
	sw := &InductionSweeper{
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

	// Should fire ~immediately on start, then again after Interval.
	// Wait for at least 3 ticks to observe both behaviours.
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

func TestInductionSweeper_RecoversFromPanic(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	done := make(chan struct{}, 4)
	sw := &InductionSweeper{
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

	// First tick panics; we should still see a second tick within
	// the next interval. Wait up to 500ms for it.
	select {
	case <-done:
		// success — survived the panic.
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("sweeper did not recover from panic; calls=%d", calls.Load())
	}
	if calls.Load() < 2 {
		t.Errorf("expected ≥2 calls, got %d", calls.Load())
	}
}

func TestInductionSweeper_ExitsOnContextCancel(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	sw := &InductionSweeper{
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
	// Let it tick a couple of times.
	time.Sleep(40 * time.Millisecond)
	cancel()
	select {
	case <-stopped:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("sweeper did not exit within 500ms after cancel")
	}
}

func TestInductionSweeper_LogsSweepErrorsButContinues(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	done := make(chan struct{}, 4)
	sw := &InductionSweeper{
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

func TestInductionSweeper_NoOpWithoutCallback(t *testing.T) {
	t.Parallel()
	sw := &InductionSweeper{Interval: 10 * time.Millisecond, Log: quietLogger()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		sw.Run(ctx)
		close(stopped)
	}()
	select {
	case <-stopped:
		// Run returns immediately when Sweep is nil.
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("sweeper with nil Sweep did not exit promptly")
	}
}
