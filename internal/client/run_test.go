package client

import (
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
)

// TestJobWaitTimeoutAtLeastUnmuteTimeoutPlusBuffer is the regression test for
// the 2026-08-09 bug: waitForJob's poll deadline (jobWaitTimeout) must exceed
// the server's worst-case job runtime, which for `power on` is anchored on
// cfg.UnmuteTimeout (see jobWaitTimeout's doc comment and
// internal/power/gogios.go waitForCluster). A deadline that is only equal to
// UnmuteTimeout, with no buffer for the wake prelude and gateway SSH round
// trips, is exactly what let the client report "gave up waiting for the job"
// moments before the job actually finished.
func TestJobWaitTimeoutAtLeastUnmuteTimeoutPlusBuffer(t *testing.T) {
	cfg := config.Default()
	c := &Client{cfg: cfg}

	got := c.jobWaitTimeout()
	min := cfg.UnmuteTimeout.D() + jobWaitBuffer

	if got < min {
		t.Fatalf("jobWaitTimeout() = %s, want at least UnmuteTimeout + buffer = %s", got, min)
	}
}

// TestJobWaitTimeoutTracksConfiguredUnmuteTimeout is the negative case: a
// non-default UnmuteTimeout (an operator budgeting for a slower gateway, as
// happened 2026-08-09 when UnmuteTimeout itself was raised 600s -> 1200s)
// must actually change jobWaitTimeout's result. Falling back to a hardcoded
// value here would silently reintroduce the drift the fix exists to close,
// just against a different, config-invisible number.
func TestJobWaitTimeoutTracksConfiguredUnmuteTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.UnmuteTimeout = config.Duration(37 * time.Minute)
	c := &Client{cfg: cfg}

	got := c.jobWaitTimeout()
	want := 37*time.Minute + jobWaitBuffer

	if got != want {
		t.Fatalf("jobWaitTimeout() with UnmuteTimeout=37m = %s, want %s", got, want)
	}

	defaultTimeout := (&Client{cfg: config.Default()}).jobWaitTimeout()
	if got == defaultTimeout {
		t.Fatalf("jobWaitTimeout() = %s did not change when UnmuteTimeout was configured away from its default", got)
	}
}
