package power

import (
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
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
