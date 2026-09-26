package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lockedBuffer is a notice writer the signal goroutine and the test can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// raise sends sig to this test process.
func raise(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), sig); err != nil {
		t.Fatalf("sending %v to self: %v", sig, err)
	}
}

// TestSignalContextCancelsOnSignal delivers each real signal to this process.
// Without signalContext's handler installed the default action would kill the
// test binary outright, which is exactly how an interrupted `power all cycle`
// used to leave the rack without mains; surviving and seeing ctx cancelled is
// the behaviour the cycle's AC restore depends on. SIGHUP is what a dropped
// SSH session sends.
func TestSignalContextCancelsOnSignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			var notice lockedBuffer
			ctx, stop := signalContext(&notice)
			defer stop()

			raise(t, sig)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatalf("context not cancelled by %v", sig)
			}
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Errorf("ctx.Err() = %v, want context.Canceled", ctx.Err())
			}
			if !strings.Contains(notice.String(), "winding down") {
				t.Errorf("notice = %q, want the winding-down message", notice.String())
			}
		})
	}
}

// TestSignalContextAbsorbsLaterSignals pins that a second Ctrl-C neither
// kills the process nor repeats the notice: the run is already winding down,
// and the restore it may be doing must be allowed to finish.
func TestSignalContextAbsorbsLaterSignals(t *testing.T) {
	var notice lockedBuffer
	ctx, stop := signalContext(&notice)
	defer stop()

	raise(t, syscall.SIGINT)
	<-ctx.Done()
	raise(t, syscall.SIGINT)
	time.Sleep(100 * time.Millisecond)

	if n := strings.Count(notice.String(), "winding down"); n != 1 {
		t.Errorf("notice printed %d times, want once: %q", n, notice.String())
	}
}

// TestSignalContextSwallowsSIGPIPE: a closed log pipe must neither kill the
// process (the default action) nor abort the run.
func TestSignalContextSwallowsSIGPIPE(t *testing.T) {
	var notice lockedBuffer
	ctx, stop := signalContext(&notice)
	defer stop()

	raise(t, syscall.SIGPIPE)
	select {
	case <-ctx.Done():
		t.Fatal("SIGPIPE cancelled the run")
	case <-time.After(100 * time.Millisecond):
	}
	if notice.String() != "" {
		t.Errorf("notice = %q, want nothing for SIGPIPE", notice.String())
	}
}

// TestSignalContextIsLiveUntilSignalled is the negative case: nothing but a
// signal (or stop) may cancel the context the whole run is bound to.
func TestSignalContextIsLiveUntilSignalled(t *testing.T) {
	ctx, stop := signalContext(&lockedBuffer{})
	defer stop()

	select {
	case <-ctx.Done():
		t.Fatalf("context cancelled without a signal: %v", ctx.Err())
	case <-time.After(50 * time.Millisecond):
	}
}
