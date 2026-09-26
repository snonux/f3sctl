package main

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

// TestSignalContextCancelsOnSignal delivers each real signal to this process.
// Without signalContext's handler installed the default action would kill the
// test binary outright, which is exactly how an interrupted `power all cycle`
// used to leave the rack without mains; surviving and seeing ctx cancelled is
// the behaviour the cycle's AC restore depends on.
func TestSignalContextCancelsOnSignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, stop := signalContext()
			defer stop()

			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatalf("sending %v to self: %v", sig, err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("context not cancelled by %v", sig)
			}
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Errorf("ctx.Err() = %v, want context.Canceled", ctx.Err())
			}
		})
	}
}

// TestSignalContextIsLiveUntilSignalled is the negative case: nothing but a
// signal (or stop) may cancel the context the whole run is bound to.
func TestSignalContextIsLiveUntilSignalled(t *testing.T) {
	ctx, stop := signalContext()
	defer stop()

	select {
	case <-ctx.Done():
		t.Fatalf("context cancelled without a signal: %v", ctx.Err())
	case <-time.After(50 * time.Millisecond):
	}
}
