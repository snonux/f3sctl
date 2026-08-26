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
// internal/power/monitor.go waitForCluster). A deadline that is only equal to
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

// TestParseHostReadsPingKnown is the regression test for hz0 at the point the
// bug actually lived: the old showStatus built its table row inline and read
// only ping/ssh off the entity, never pingKnown, so a host the probe never
// reached was indistinguishable here from one it measured and found silent.
// parseHost is what feeds presenter.Describe now, so this pins that the
// pingKnown property actually reaches the power.HostStatus it builds.
func TestParseHostReadsPingKnown(t *testing.T) {
	measured, ok := parseHost(Entity{Properties: map[string]any{
		"name": "f3", "ip": "192.168.1.13", "ping": false, "pingKnown": true, "ssh": false,
	}})
	if !ok || measured.PingKnown != true {
		t.Errorf("parseHost(pingKnown=true) = %+v, ok=%v, want PingKnown true", measured, ok)
	}

	unmeasured, ok := parseHost(Entity{Properties: map[string]any{
		"name": "f3", "ip": "192.168.1.13", "ping": false, "pingKnown": false, "ssh": false,
	}})
	if !ok || unmeasured.PingKnown != false {
		t.Errorf("parseHost(pingKnown=false) = %+v, ok=%v, want PingKnown false", unmeasured, ok)
	}
}

// TestParseHostDefaultsPingKnownWhenAbsent pins the back-compat rule
// docs/client-reference.js documents for the same field: a server that
// predates pingKnown omits the property entirely, and its absence means the
// probe ran (not that it didn't) -- so the zero value of a missing bool
// property, which is false, must not be read as PingKnown=false here.
func TestParseHostDefaultsPingKnownWhenAbsent(t *testing.T) {
	st, ok := parseHost(Entity{Properties: map[string]any{
		"name": "f3", "ip": "192.168.1.13", "ping": false, "ssh": false,
	}})
	if !ok || !st.PingKnown {
		t.Errorf("parseHost with pingKnown absent = %+v, ok=%v, want PingKnown true (older-server default)", st, ok)
	}
}

// TestParseFansReportsAnErrorPropertyAsAnError pins that a "fans" entity
// carrying an error property (the plug was unreachable) turns into a Go
// error, not a FansState claiming the plug is off -- see parseFans and
// presenter.Status.
func TestParseFansReportsAnErrorPropertyAsAnError(t *testing.T) {
	_, err := parseFans(Entity{Properties: map[string]any{"error": "dial tcp: timeout"}})
	if err == nil || err.Error() != "dial tcp: timeout" {
		t.Errorf("parseFans with an error property = %v, want it surfaced as an error", err)
	}

	fans, err := parseFans(Entity{Properties: map[string]any{"on": true, "ip": "192.168.1.99"}})
	if err != nil {
		t.Fatalf("parseFans with no error property: %v", err)
	}
	if !fans.On || fans.IP != "192.168.1.99" {
		t.Errorf("parseFans(on=true) = %+v, want On=true IP=192.168.1.99", fans)
	}
}
