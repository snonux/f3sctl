package power

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/gogios"
	"github.com/snonux/f3sctl/internal/inventory"
)

// The Gogios mute mechanism (gateways, the cluster wait, the un-mute-anyway
// rule) is pinned in internal/gogios's monitor tests. These fakes let the
// Engine's tests pin only what Engine owns: when it mutes or un-mutes, and how
// it words each outcome the mechanism can report.

// fakeMonitor is the gogiosMonitor stand-in. It records each call ("mute",
// "unmute", "unmute-gogios") and returns scripted outcomes.
type fakeMonitor struct {
	mu    sync.Mutex
	calls []string

	muteErr, unmuteErr error
	// unmuteGogios, if set, scripts UnmuteGogios; nil means every node
	// answered and every gateway un-muted.
	unmuteGogios func(ctx context.Context, log io.Writer, rewake func()) error
	status       []gogios.GatewayMute
}

func (f *fakeMonitor) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeMonitor) callsList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeMonitor) Mute(context.Context, io.Writer) error {
	f.record("mute")
	return f.muteErr
}

func (f *fakeMonitor) Unmute(context.Context, io.Writer) error {
	f.record("unmute")
	return f.unmuteErr
}

func (f *fakeMonitor) UnmuteGogios(ctx context.Context, log io.Writer, rewake func()) error {
	f.record("unmute-gogios")
	if f.unmuteGogios == nil {
		return nil
	}
	return f.unmuteGogios(ctx, log, rewake)
}

func (f *fakeMonitor) Status(context.Context) []gogios.GatewayMute {
	f.record("status")
	return f.status
}

// clusterNeverAnswered is the error gogios.Monitor.UnmuteGogios returns when
// the budget ran out with down still unreachable (it un-muted anyway).
func clusterNeverAnswered(down ...string) error {
	return fmt.Errorf("%w after 1m0s: %s", gogios.ErrClusterIncomplete, strings.Join(down, ", "))
}

// abandonOnCancel scripts UnmuteGogios for a wait that never ends on its own:
// it returns what gogios.Monitor returns when the wait is cancelled, with the
// marker left untouched.
func abandonOnCancel(ctx context.Context, _ io.Writer, _ func()) error {
	<-ctx.Done()
	return fmt.Errorf("%w: %w", gogios.ErrWaitAbandoned, ctx.Err())
}

// countingSwitch is a gogios.GatewaySwitch that counts its SetMute calls.
type countingSwitch struct {
	mu    sync.Mutex
	calls int
}

func (s *countingSwitch) SetMute(context.Context, io.Writer, bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return nil
}

func (s *countingSwitch) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// WithGatewaySwitch works on a hand-built Engine (no monitor yet): mute and
// un-mute then go through the switch, not the SSH verb, and nil restores the
// verb.
func TestEngineWithGatewaySwitchInstallsAndClears(t *testing.T) {
	var cfg config.Config
	cfg.Inventory.Hosts = []inventory.Host{{Name: "blowfish", Role: inventory.RoleGateway}}
	verb := &fakePower{}
	e := &Engine{cfg: cfg, power: verb}
	sw := &countingSwitch{}

	e.WithGatewaySwitch(sw)
	if err := e.UnmuteNow(context.Background(), io.Discard); err != nil {
		t.Fatalf("UnmuteNow: %v", err)
	}
	if got := sw.count(); got != 1 {
		t.Errorf("switch calls = %d, want one", got)
	}
	if got := verb.verbCalls(); len(got) != 0 {
		t.Errorf("SSH verb calls = %v, want none while a switch is installed", got)
	}

	e.WithGatewaySwitch(nil)
	if err := e.UnmuteNow(context.Background(), io.Discard); err != nil {
		t.Fatalf("UnmuteNow: %v", err)
	}
	if got := sw.count(); got != 1 {
		t.Errorf("switch calls = %d after WithGatewaySwitch(nil), want still one", got)
	}
	if got := verb.verbCalls(); len(got) != 1 || got[0] != "gogios-unmute:blowfish" {
		t.Errorf("SSH verb calls = %v, want the un-mute back on the SSH verb", got)
	}
}

// TestEngineDownNodesListsTheHostsThatDidNotAnswer pins the adapter the real
// monitor waits on: Engine.Probe's failed pings, by name.
func TestEngineDownNodesListsTheHostsThatDidNotAnswer(t *testing.T) {
	hosts := []inventory.Host{{Name: "r0", IP: "192.0.2.10"}, {Name: "r1", IP: "192.0.2.11"}}
	e := &Engine{
		isUp:  func(_ context.Context, ip string) (up, known bool) { return ip == "192.0.2.11", true },
		probe: noSSH{},
	}

	if got := e.downNodes(context.Background(), hosts); len(got) != 1 || got[0] != "r0" {
		t.Errorf("downNodes = %v, want [r0]", got)
	}
}

// noSSH is a ProbeBackend whose SSH probe never answers and never dials.
type noSSH struct{}

func (noSSH) Ping(context.Context, string) (up, known bool) { return false, true }
func (noSSH) SSH(context.Context, inventory.Host) bool      { return false }
