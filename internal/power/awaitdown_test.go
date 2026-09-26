package power

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/inventory"
)

// TestShutdownWorstCaseAtDefaults pins ShutdownWorstCase's formula against the
// shipped default config: 4 hosts (f0-f3, the OffAll case -- the largest set
// any shutdown-shaped job ever walks) at the default 240s VMShutdownTimeout
// each, plus the default 2m powerDownTimeout confirmation wait. This is the
// number coordination.NewManager relies on to keep the server's job-staleness
// ceiling honest about the shutdown path; see kz0.
func TestShutdownWorstCaseAtDefaults(t *testing.T) {
	cfg := config.Default()
	want := 4*cfg.VMShutdownTimeout.D() + powerDownTimeout
	if got := ShutdownWorstCase(cfg); got != want {
		t.Errorf("ShutdownWorstCase(config.Default()) = %s, want %s", got, want)
	}
}

// TestShutdownWorstCaseScalesWithVMShutdownTimeout pins that raising
// VMShutdownTimeout raises ShutdownWorstCase proportionally -- the exact
// coupling a reviewer found missing from kz0's first fix, which derived the
// staleness ceiling from UnmuteTimeout alone and left VMShutdownTimeout
// free to grow the Off path's worst case with nothing on the ceiling side
// tracking it.
func TestShutdownWorstCaseScalesWithVMShutdownTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.VMShutdownTimeout = config.Duration(10 * time.Minute)

	want := 4*10*time.Minute + powerDownTimeout
	if got := ShutdownWorstCase(cfg); got != want {
		t.Errorf("ShutdownWorstCase with VMShutdownTimeout=10m = %s, want %s", got, want)
	}
}

// TestOffInterruptedWhileConfirmingIsNotAShutdownFailure pins what a Ctrl-C
// during the power-down wait reports. awaitPowerDown returns every host it
// had not confirmed yet when cancelled, and off() used to pass that list to
// shutdownFailure -- "did not complete shutdown", the hung-host emergency --
// for hosts that were simply still going down when the operator gave up.
func TestOffInterruptedWhileConfirmingIsNotAShutdownFailure(t *testing.T) {
	rig := newOffTestRig(t, "f0", "f1", "f2", "f3")
	rig.power.onPowerOff = nil // hosts keep answering: the wait never ends by itself
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig.power.onPowerOffEnd = func(h inventory.Host) {
		if h.Name == inventory.StorageMaster {
			go func() { time.Sleep(50 * time.Millisecond); cancel() }()
		}
	}

	err := rig.eng.OffAll(ctx, &rig.log)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a wrapped context.Canceled", err)
	}
	if strings.Contains(err.Error(), "did not complete shutdown") {
		t.Errorf("err = %v, want an interruption, not a shutdown failure", err)
	}
	if !strings.Contains(err.Error(), "interrupted") || !strings.Contains(err.Error(), "f0") {
		t.Errorf("err = %v, want it to say interrupted and name the unconfirmed hosts", err)
	}
}

// TestShutdownInterruptedWithNothingPending covers the other shape: every
// host confirmed off, the cancel landing just before the fans step.
func TestShutdownInterruptedWithNothingPending(t *testing.T) {
	err := shutdownInterrupted(nil, context.Canceled)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "every host was confirmed off") {
		t.Errorf("err = %v, want every host confirmed off and a wrapped context.Canceled", err)
	}
}
