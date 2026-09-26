package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
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

// waitForNotice waits for the winding-down notice. signalContext cancels
// before it writes, so the notice can trail ctx.Done() by a moment.
func waitForNotice(t *testing.T, notice *lockedBuffer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(notice.String(), "winding down") {
		if time.Now().After(deadline) {
			t.Fatalf("notice = %q, want the winding-down message", notice.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// skipIfIgnored skips a test that needs sig caught when the test binary was
// started with it ignored (`nohup go test`, a `trap "" INT` shell):
// signalContext rightly leaves such a signal alone, so there is nothing to
// catch, and raising it would test nothing.
func skipIfIgnored(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if signal.Ignored(sig) {
		t.Skipf("%v was ignored when the test started; signalContext does not catch it", sig)
	}
}

// awaitCancel waits, boundedly, for ctx to be cancelled by sig.
func awaitCancel(t *testing.T, ctx context.Context, sig syscall.Signal) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("context not cancelled by %v", sig)
	}
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
			skipIfIgnored(t, sig)
			var notice lockedBuffer
			ctx, stop := signalContext(&notice)
			defer stop()

			raise(t, sig)
			awaitCancel(t, ctx, sig)
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Errorf("ctx.Err() = %v, want context.Canceled", ctx.Err())
			}
			waitForNotice(t, &notice)
		})
	}
}

// TestSignalContextAbsorbsLaterSignals pins that a second Ctrl-C neither
// kills the process nor repeats the notice: the run is already winding down,
// and the restore it may be doing must be allowed to finish.
func TestSignalContextAbsorbsLaterSignals(t *testing.T) {
	skipIfIgnored(t, syscall.SIGINT)
	var notice lockedBuffer
	ctx, stop := signalContext(&notice)
	defer stop()

	raise(t, syscall.SIGINT)
	awaitCancel(t, ctx, syscall.SIGINT)
	waitForNotice(t, &notice)
	raise(t, syscall.SIGINT)
	time.Sleep(100 * time.Millisecond)

	if n := strings.Count(notice.String(), "winding down"); n != 1 {
		t.Errorf("notice printed %d times, want once: %q", n, notice.String())
	}
}

// helperEnv selects what TestSignalHelperProcess does when this test binary
// is re-run as a child. Child processes are the only honest way to test
// dispositions: a SIGPIPE raised by kill(2) is ignored by the Go runtime
// whatever signalContext does, and a disposition ignored at startup cannot be
// recreated inside an already-running process.
const helperEnv = "F3SCTL_SIGNAL_HELPER"

// Exit codes the helper reports with; anything else (or death by signal) is a
// failure the parent names.
const (
	helperOK        = 0
	helperCancelled = 3
	helperNoEPIPE   = 4
)

// TestSignalHelperProcess is not a test: it is the child body for the tests
// below, and returns at once in a normal run.
func TestSignalHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		return
	}
	ctx, stop := signalContext(io.Discard)
	defer stop()

	switch mode {
	case "pipe":
		// stdout's read end is already closed: without SIGPIPE caught, this
		// write kills the process instead of returning EPIPE.
		if _, err := os.Stdout.WriteString("into the void\n"); !errors.Is(err, syscall.EPIPE) {
			os.Exit(helperNoEPIPE)
		}
		os.Exit(helperOK)
	case "hup":
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			os.Exit(1)
		}
		select {
		case <-ctx.Done():
			os.Exit(helperCancelled)
		case <-time.After(200 * time.Millisecond):
			os.Exit(helperOK)
		}
	}
	os.Exit(1)
}

// helperCommand re-runs this test binary as the signal helper in mode.
func helperCommand(mode string, argv0 ...string) *exec.Cmd {
	args := append(argv0, os.Args[0], "-test.run=^TestSignalHelperProcess$")
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), helperEnv+"="+mode)
	return cmd
}

// TestSignalContextSurvivesAClosedStdout is the `f3sctl power all cycle |
// tee log` case: Ctrl-C kills tee too, and the next log line is a write to a
// pipe nobody reads. That must fail with EPIPE, not kill the process between
// cutting AC and restoring it.
func TestSignalContextSurvivesAClosedStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	r.Close()
	defer w.Close()

	cmd := helperCommand("pipe")
	cmd.Stdout = w
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper with a closed stdout: %v, want a normal exit after EPIPE", err)
	}
}

// TestSignalContextKeepsAStartupIgnoredSIGHUPIgnored is the nohup case: a
// SIGHUP the process was started with ignored must not cancel the run, as it
// would if signal.Notify un-ignored it. The shell's `trap "" HUP` before exec
// is exactly how nohup(1) leaves it.
func TestSignalContextKeepsAStartupIgnoredSIGHUPIgnored(t *testing.T) {
	cmd := helperCommand("hup", "/bin/sh", "-c", `trap "" HUP; exec "$@"`, "sh")
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == helperCancelled {
		t.Fatalf("a startup-ignored SIGHUP cancelled the run\n%s", out)
	}
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
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
