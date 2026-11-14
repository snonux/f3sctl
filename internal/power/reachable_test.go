package power

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/powertest"
)

// sshOnly is a ProbeBackend whose sshd answers for exactly the given
// addresses. Paired with an isUp that never answers, it models the laptop's
// view of the r-nodes over wg2: TCP admitted, ICMP echo dropped by design.
type sshOnly map[string]bool

func (sshOnly) Ping(context.Context, string) (up, known bool)  { return false, true }
func (s sshOnly) SSH(_ context.Context, h inventory.Host) bool { return s[h.IP] }

// icmpFiltered is an isUp whose probe runs and never hears an echo reply.
func icmpFiltered(context.Context, string) (up, known bool) { return false, true }

// TestAnswersFoldsSSHIntoLiveness pins Engine.answers, the probe every strict
// "may this host be off" decision goes through: either probe answering is up,
// either reaching a conclusion is known.
func TestAnswersFoldsSSHIntoLiveness(t *testing.T) {
	h := inventory.Host{Name: "r0", IP: "192.0.2.20", SSHPort: 22}
	for _, tc := range []struct {
		name              string
		pingUp, pingKnown bool
		ssh               bool
		wantUp, wantKnown bool
	}{
		{"icmp filtered, sshd answers", false, true, true, true, true},
		{"ping unmeasured, sshd answers", false, false, true, true, true},
		{"ping answers, no sshd", true, true, false, true, true},
		{"silent", false, true, false, false, true},
		{"unmeasured, no sshd", false, false, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Engine{
				isUp:  func(context.Context, string) (bool, bool) { return tc.pingUp, tc.pingKnown },
				probe: sshOnly{h.IP: tc.ssh},
			}
			up, known := e.answers(context.Background(), h)
			if up != tc.wantUp || known != tc.wantKnown {
				t.Errorf("answers = (%v, %v), want (%v, %v)", up, known, tc.wantUp, tc.wantKnown)
			}
		})
	}
}

// TestSnapshotGuardCountsSSHOnlyHostAsRunning is the cheap (API snapshot)
// half of the fan and AC guards: a host with ICMP filtered but sshd up keeps
// the plugs on.
func TestSnapshotGuardCountsSSHOnlyHostAsRunning(t *testing.T) {
	inv := inventory.Default()
	st := HostStatus{Name: "f1", PingKnown: true, SSH: true}
	for name, a := range map[string]RackActivity{
		"fans": RackActivityFrom(inv, []HostStatus{st}),
		"ac":   ACActivityFrom(inv, []HostStatus{st}),
	} {
		if !a.Busy() || a.Why() != "f1 still running" {
			t.Errorf("%s guard: busy=%v why=%q, want f1 still running", name, a.Busy(), a.Why())
		}
	}
	// Negative: silent on both probes is idle.
	if a := RackActivityFrom(inv, []HostStatus{{Name: "f1", PingKnown: true}}); a.Busy() {
		t.Errorf("a host silent on ping and ssh keeps the rack busy: %q", a.Why())
	}
}

// TestFansOffKeepsFansOnForSSHOnlyHost is the strict half: the shutdown's
// last step must not cut the fans while a power-group host answers sshd,
// however silent its ICMP.
func TestFansOffKeepsFansOnForSSHOnlyHost(t *testing.T) {
	shelly := powertest.NewFakeShelly(t, true)
	eng := testEngine(t, shelly)
	eng.isUp = icmpFiltered
	eng.probe = sshOnly{hostIP(t, eng, "f2"): true}

	leftOn, err := eng.fansOffOnceTheRackIsIdle(context.Background(), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("fansOffOnceTheRackIsIdle: %v", err)
	}
	if len(leftOn) != 1 || leftOn[0] != "f2" {
		t.Errorf("left on for %v, want [f2]", leftOn)
	}
	if got := shelly.SetCalls(); len(got) != 0 {
		t.Errorf("Switch.Set calls = %v, want none while f2 answers sshd", got)
	}

	// Negative: with sshd silent too, the rack is idle and the fans go off.
	eng.probe = noSSH{}
	if leftOn, err := eng.fansOffOnceTheRackIsIdle(context.Background(), &bytes.Buffer{}); err != nil || len(leftOn) != 0 {
		t.Errorf("idle rack: leftOn = %v, err = %v, want the fans switched off", leftOn, err)
	}
}

// TestSkipAlreadyOffKeepsSSHOnlyHost: a host whose ICMP is filtered but whose
// sshd answers is running, so a shutdown must not skip it as "already
// powered off" and leave it on.
func TestSkipAlreadyOffKeepsSSHOnlyHost(t *testing.T) {
	hosts := []inventory.Host{
		{Name: "f0", IP: "192.0.2.30", SSHPort: 22},
		{Name: "f1", IP: "192.0.2.31", SSHPort: 22},
	}
	e := &Engine{isUp: icmpFiltered, probe: sshOnly{"192.0.2.31": true}}

	var log bytes.Buffer
	live := e.skipAlreadyOff(context.Background(), &log, hosts)
	if len(live) != 1 || live[0].Name != "f1" {
		t.Errorf("live = %v, want [f1]", names(live))
	}
	if !bytes.Contains(log.Bytes(), []byte("f0 is already powered off")) {
		t.Errorf("log = %q, want f0 (silent on both probes) skipped", log.String())
	}
}

// TestAwaitPowerDownDoesNotConfirmSSHOnlyHost: a host still accepting sshd
// connections has not powered down, even with its ICMP silent.
func TestAwaitPowerDownDoesNotConfirmSSHOnlyHost(t *testing.T) {
	hosts := []inventory.Host{
		{Name: "f0", IP: "192.0.2.40", SSHPort: 22},
		{Name: "f1", IP: "192.0.2.41", SSHPort: 22},
	}
	e := &Engine{
		isUp:              icmpFiltered,
		probe:             sshOnly{"192.0.2.41": true},
		downProbeInterval: time.Millisecond,
		powerDownTimeout:  200 * time.Millisecond,
	}

	var log bytes.Buffer
	got := e.awaitPowerDown(context.Background(), &log, hosts)
	if len(got) != 1 || got[0] != "f1" {
		t.Errorf("unconfirmed = %v, want [f1]: it still answers sshd", got)
	}
	if !bytes.Contains(log.Bytes(), []byte("f0 is down")) {
		t.Errorf("log = %q, want f0 confirmed down", log.String())
	}
}

// TestDownNodesTreatsSSHOnlyNodeAsUp: the post-wake wait for the k3s nodes
// must end once they answer sshd, or a local wake from the laptop (r-nodes'
// ICMP dropped over wg2) re-sends magic packets to a running rack and only
// un-mutes Gogios after the timeout, as a failure.
func TestDownNodesTreatsSSHOnlyNodeAsUp(t *testing.T) {
	hosts := []inventory.Host{{Name: "r0", IP: "192.0.2.50"}, {Name: "r1", IP: "192.0.2.51"}}
	e := &Engine{isUp: icmpFiltered, probe: sshOnly{"192.0.2.50": true}}

	if got := e.downNodes(context.Background(), hosts); len(got) != 1 || got[0] != "r1" {
		t.Errorf("downNodes = %v, want [r1]", got)
	}
}

// blockingSSH is a ProbeBackend whose sshd dial hangs until its context is
// cancelled, like a filtered port waiting out its timeout.
type blockingSSH struct{}

func (blockingSSH) Ping(context.Context, string) (up, known bool) { return false, true }
func (blockingSSH) SSH(ctx context.Context, _ inventory.Host) bool {
	<-ctx.Done()
	return false
}

// TestAnswersStopsTheDialOnceICMPAnswers: an echo reply settles the question,
// so answers must not wait out a dial that cannot connect. Without the
// cancel this blocks until the test's deadline.
func TestAnswersStopsTheDialOnceICMPAnswers(t *testing.T) {
	e := &Engine{
		isUp:  func(context.Context, string) (bool, bool) { return true, true },
		probe: blockingSSH{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	up, known := e.answers(ctx, inventory.Host{Name: "f0", IP: "192.0.2.60", SSHPort: 22})
	if !up || !known {
		t.Errorf("answers = (%v, %v), want (true, true)", up, known)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("answers waited %s for a dial after ping had answered", waited)
	}
}
