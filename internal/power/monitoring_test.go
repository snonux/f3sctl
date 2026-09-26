package power

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

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

	// unmuteGogios, if set, scripts UnmuteGogios; nil means every node
	// answered and every gateway un-muted.
	unmuteGogios func(ctx context.Context, log io.Writer, rewake func()) error
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
	return nil
}

func (f *fakeMonitor) Unmute(context.Context, io.Writer) error {
	f.record("unmute")
	return nil
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
	return nil
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

// gatewayAndNode is a config with one gateway and one k3s node, and an
// un-mute budget that has already run out, so UnmuteGogios answers at once.
func gatewayAndNode() config.Config {
	cfg := config.Default()
	cfg.Inventory.Hosts = []inventory.Host{
		{Name: "blowfish", Role: inventory.RoleGateway, IP: "192.0.2.1"},
		{Name: "r0", Role: inventory.RoleCluster, IP: "192.0.2.10"},
	}
	cfg.UnmuteTimeout = config.Duration(-time.Second)
	return cfg
}

// WithGatewaySwitch on an Engine that already holds a *gogios.Monitor sets the
// switch on that monitor rather than replacing it, so whatever it was built
// with (here its own verb and probe) survives; nil then clears only the
// switch.
func TestEngineWithGatewaySwitchKeepsAnInstalledMonitor(t *testing.T) {
	cfg := gatewayAndNode()
	ownVerb := &fakePower{}
	probed := 0
	ownProbe := func(_ context.Context, hosts []inventory.Host) []string {
		probed++
		return []string{hosts[0].Name}
	}
	engineVerb := &fakePower{}
	e := &Engine{
		cfg:   cfg,
		power: engineVerb,
		isUp:  func(context.Context, string) (up, known bool) { return true, true },
		probe: noSSH{},
	}
	e.monitor = gogios.NewMonitor(cfg, ownVerb, ownProbe)
	sw := &countingSwitch{}

	e.WithGatewaySwitch(sw)
	err := e.UnmuteGogios(context.Background(), io.Discard, nil)
	if probed == 0 || !errors.Is(err, gogios.ErrClusterIncomplete) {
		t.Fatalf("UnmuteGogios err = %v after %d probes, want the installed monitor's own probe (r0 down)", err, probed)
	}
	if got := sw.count(); got != 1 {
		t.Errorf("switch calls = %d, want the un-mute through the switch", got)
	}

	e.WithGatewaySwitch(nil)
	if err := e.UnmuteNow(context.Background(), io.Discard); err != nil {
		t.Fatalf("UnmuteNow: %v", err)
	}
	if got := ownVerb.verbCalls(); len(got) != 1 || got[0] != "gogios-unmute:blowfish" {
		t.Errorf("installed monitor's verb calls = %v, want the un-mute back on its own SSH verb", got)
	}
	if got := engineVerb.verbCalls(); len(got) != 0 {
		t.Errorf("engine verb calls = %v, want none: the installed monitor was kept", got)
	}
}

// New wires a real gogios.Monitor that waits on the Engine's own probing
// (downNodes over Probe): with r0 silent, the wake's un-mute reports it.
func TestNewWiresTheMonitorToTheEnginesProbe(t *testing.T) {
	e, err := New(gatewayAndNode())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.isUp = func(_ context.Context, ip string) (up, known bool) { return ip != "192.0.2.10", true }
	e.probe = noSSH{}
	sw := &countingSwitch{} // keeps the un-mute off real SSH
	e.WithGatewaySwitch(sw)

	err = e.UnmuteGogios(context.Background(), io.Discard, nil)
	if !errors.Is(err, gogios.ErrClusterIncomplete) || !strings.HasSuffix(err.Error(), ": r0") {
		t.Fatalf("UnmuteGogios err = %v, want ErrClusterIncomplete naming r0", err)
	}
	if got := sw.count(); got != 1 {
		t.Errorf("switch calls = %d, want the un-mute anyway", got)
	}
}

// TestEngineDelegatesMonitoringToTheMonitor pins the thin surface: each of
// Engine's monitoring methods reaches the monitor exactly once.
func TestEngineDelegatesMonitoringToTheMonitor(t *testing.T) {
	mon := &fakeMonitor{}
	e := &Engine{monitor: mon}
	ctx := context.Background()

	_ = e.MuteGogios(ctx, io.Discard)
	_ = e.UnmuteNow(ctx, io.Discard)
	_ = e.UnmuteGogios(ctx, io.Discard, nil)
	_ = e.MonitoringStatus(ctx)

	want := "mute,unmute,unmute-gogios,status"
	if got := strings.Join(mon.callsList(), ","); got != want {
		t.Errorf("monitor calls = %s, want %s", got, want)
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
