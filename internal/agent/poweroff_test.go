package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
)

// writeConfigFile writes a minimal JSON config setting only
// vm_shutdown_timeout, and points F3SCTL_CONFIG at it for the duration of the
// test -- mirroring how cmd/f3sctl/main.go resolves the config path, so the
// test exercises the real config.Load path rather than a config.Config value
// built by hand.
func writeConfigFile(t *testing.T, vmShutdownTimeout string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "f3sctl.json")
	body, err := json.Marshal(map[string]string{"vm_shutdown_timeout": vmShutdownTimeout})
	if err != nil {
		t.Fatalf("marshaling test config: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	t.Setenv("F3SCTL_CONFIG", path)
}

// TestVMShutdownTimeoutReadsConfig is the regression test for vz0: before
// this fix, vmShutdownTimeout was a hardcoded 240s constant that never
// consulted cfg.VMShutdownTimeout at all, so lowering the config's
// vm_shutdown_timeout changed nothing about the agent's real wait.
func TestVMShutdownTimeoutReadsConfig(t *testing.T) {
	writeConfigFile(t, "37s")

	if got, want := vmShutdownTimeout(), 37*time.Second; got != want {
		t.Errorf("vmShutdownTimeout() = %s, want %s (the value in the config file)", got, want)
	}
}

// TestVMShutdownTimeoutFallsBackToDefault checks the two "no real value"
// cases both land on config.Default()'s 240s rather than on a nonsensical
// zero-duration wait, which would SIGKILL every guest on the very first
// poll.
func TestVMShutdownTimeoutFallsBackToDefault(t *testing.T) {
	want := config.Default().VMShutdownTimeout.D()

	t.Run("no config file installed", func(t *testing.T) {
		t.Setenv("F3SCTL_CONFIG", filepath.Join(t.TempDir(), "does-not-exist.json"))
		if got := vmShutdownTimeout(); got != want {
			t.Errorf("vmShutdownTimeout() = %s, want the %s default", got, want)
		}
	})

	t.Run("vm_shutdown_timeout explicitly zero", func(t *testing.T) {
		writeConfigFile(t, "0s")
		if got := vmShutdownTimeout(); got != want {
			t.Errorf("vmShutdownTimeout() = %s, want the %s default", got, want)
		}
	})
}

// fakeGuests simulates the bhyve PID/signal seams for waitForGuests without
// touching real OS processes: it reports back as still running until
// signalled with sig, after which pidLookup reports it gone.
type fakeGuests struct {
	mu      sync.Mutex
	sig     syscall.Signal // zero until sendSignal is called
	signals []syscall.Signal
}

func (g *fakeGuests) lookup() ([]int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sig != 0 {
		return nil, nil
	}
	return []int{4242}, nil
}

func (g *fakeGuests) signal(pids []int, sig syscall.Signal) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.signals = append(g.signals, sig)
	g.sig = sig
}

// install points the package's pidLookup/sendSignal seams, and the poll/kill
// waits, at fast test-controlled values, restoring the real ones afterward.
func (g *fakeGuests) install(t *testing.T) {
	t.Helper()
	oldLookup, oldSignal := pidLookup, sendSignal
	oldPoll, oldKillWait := pollInterval, killSettleWait
	pidLookup, sendSignal = g.lookup, g.signal
	pollInterval, killSettleWait = time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		pidLookup, sendSignal = oldLookup, oldSignal
		pollInterval, killSettleWait = oldPoll, oldKillWait
	})
}

// TestWaitForGuestsHonorsInjectedTimeout is the other half of vz0's
// regression coverage: it proves waitForGuests actually gives up at the
// duration it is handed, rather than at the old hardcoded 240s. A guest that
// never dies on its own forces the SIGKILL branch; the test asserts that
// happens close to the requested timeout, not anywhere near four minutes.
func TestWaitForGuestsHonorsInjectedTimeout(t *testing.T) {
	guests := &fakeGuests{}
	guests.install(t)

	const timeout = 20 * time.Millisecond
	start := time.Now()
	err := waitForGuests(timeout)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("waitForGuests: unexpected error: %v", err)
	}
	// Generous slack for scheduling jitter, but nowhere near the old 240s
	// hardcoded wait -- proving the timeout parameter (and so
	// cfg.VMShutdownTimeout, once vmShutdownTimeout() feeds it) is what
	// actually bounds the wait now.
	if elapsed < timeout {
		t.Errorf("waitForGuests returned after %s, before its %s timeout even elapsed", elapsed, timeout)
	}
	if elapsed > 5*time.Second {
		t.Errorf("waitForGuests took %s to give up on a %s timeout; still bounded by something like the old hardcoded 240s", elapsed, timeout)
	}

	guests.mu.Lock()
	defer guests.mu.Unlock()
	if len(guests.signals) != 1 || guests.signals[0] != syscall.SIGKILL {
		t.Errorf("signals sent = %v, want exactly one SIGKILL", guests.signals)
	}
}

// TestWaitForGuestsReturnsPromptlyWhenGuestsExit checks the happy path is not
// held hostage by the timeout at all: a guest that exits on its own should
// let waitForGuests return well before the deadline.
func TestWaitForGuestsReturnsPromptlyWhenGuestsExit(t *testing.T) {
	guests := &fakeGuests{}
	guests.install(t)
	// Simulate the guest having already gone by the first poll.
	guests.sig = syscall.SIGTERM

	start := time.Now()
	if err := waitForGuests(time.Hour); err != nil {
		t.Fatalf("waitForGuests: unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waitForGuests took %s to notice the guest was already gone", elapsed)
	}

	guests.mu.Lock()
	defer guests.mu.Unlock()
	if len(guests.signals) != 0 {
		t.Errorf("signals sent = %v, want none: the guest was already gone", guests.signals)
	}
}
