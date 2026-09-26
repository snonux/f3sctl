package power

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/inventory"
)

// These tests pin the GatewaySwitch route (task 5l2): when the CLI installs a
// switch because no SSH key is readable, every mute and un-mute -- including
// the one at the end of a wake -- must go through it and never through the
// per-gateway SSH verb, which could only fail with "no readable SSH identity".

// fakeSwitch records every SetMute call and returns a scripted error.
type fakeSwitch struct {
	mu    sync.Mutex
	calls []bool // the mute argument of each call, in order
	err   error
}

func (f *fakeSwitch) SetMute(_ context.Context, log io.Writer, mute bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, mute)
	if f.err == nil {
		_, _ = io.WriteString(log, "  switched\n")
	}
	return f.err
}

func (f *fakeSwitch) callsList() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.calls...)
}

func allNodesUp(_ context.Context, hosts []inventory.Host) []HostStatus {
	out := make([]HostStatus, len(hosts))
	for i, h := range hosts {
		out[i] = HostStatus{Name: h.Name, Ping: true}
	}
	return out
}

func TestMonitorMuteAndUnmuteUseTheSwitchInsteadOfSSH(t *testing.T) {
	verb := &fakeGatewayVerb{out: map[string]string{}, err: map[string]error{}}
	sw := &fakeSwitch{}
	m := newTestMonitor(t, verb, nil, []string{"blowfish", "fishfinger"}, nil, time.Minute)
	m.via = sw

	if err := m.Mute(context.Background(), io.Discard); err != nil {
		t.Fatalf("Mute: %v", err)
	}
	if err := m.Unmute(context.Background(), io.Discard); err != nil {
		t.Fatalf("Unmute: %v", err)
	}
	if got := sw.callsList(); len(got) != 2 || !got[0] || got[1] {
		t.Errorf("switch calls = %v, want [true false]", got)
	}
	if got := verb.callsList(); len(got) != 0 {
		t.Errorf("SSH verb calls = %v, want none while a switch is installed", got)
	}
}

// The reported bug: a complete wake whose closing un-mute has to go through
// the switch.
func TestMonitorUnmuteGogiosUsesTheSwitchAfterTheClusterAnswers(t *testing.T) {
	verb := &fakeGatewayVerb{out: map[string]string{}, err: map[string]error{}}
	sw := &fakeSwitch{}
	m := newTestMonitor(t, verb, allNodesUp, []string{"blowfish", "fishfinger"}, []string{"r0", "r1", "r2"}, time.Minute)
	m.via = sw

	if err := m.UnmuteGogios(context.Background(), io.Discard, nil); err != nil {
		t.Fatalf("UnmuteGogios: %v", err)
	}
	if got := sw.callsList(); len(got) != 1 || got[0] {
		t.Errorf("switch calls = %v, want one un-mute", got)
	}
	if got := verb.callsList(); len(got) != 0 {
		t.Errorf("SSH verb calls = %v, want none", got)
	}
}

func TestMonitorUnmuteGogiosTimeoutStillUnmutesThroughTheSwitch(t *testing.T) {
	sw := &fakeSwitch{}
	m := newTestMonitor(t, &fakeGatewayVerb{}, oneNodeDown, []string{"blowfish"}, []string{"r0", "r1"}, -time.Second)
	m.via = sw

	err := m.UnmuteGogios(context.Background(), io.Discard, nil)
	if !errors.Is(err, ErrClusterIncomplete) {
		t.Fatalf("err = %v, want ErrClusterIncomplete", err)
	}
	if got := sw.callsList(); len(got) != 1 || got[0] {
		t.Errorf("switch calls = %v, want the un-mute anyway", got)
	}
}

// Negative: a failing switch is the wake's monitoring error, not swallowed.
func TestMonitorUnmuteGogiosReportsAFailingSwitch(t *testing.T) {
	sw := &fakeSwitch{err: errors.New("could not gogios-unmute Gogios on: [fishfinger]")}
	m := newTestMonitor(t, &fakeGatewayVerb{}, allNodesUp, []string{"blowfish", "fishfinger"}, []string{"r0"}, time.Minute)
	m.via = sw

	err := m.UnmuteGogios(context.Background(), io.Discard, nil)
	if err == nil || errors.Is(err, ErrClusterIncomplete) || !strings.Contains(err.Error(), "fishfinger") {
		t.Fatalf("err = %v, want the switch's gateway error, not a cluster timeout", err)
	}
}

// A cancelled wake with a switch installed must not suggest an SSH command
// that this machine has no key for.
func TestMonitorUnmuteGogiosCancelledWithSwitchSuggestsTheCLI(t *testing.T) {
	sw := &fakeSwitch{}
	m := newTestMonitor(t, &fakeGatewayVerb{}, oneNodeDown, []string{"blowfish"}, []string{"r0", "r1"}, time.Hour)
	m.via = sw
	m.poll = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var log bytes.Buffer
	if err := m.UnmuteGogios(ctx, &log, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(sw.callsList()) != 0 {
		t.Error("an aborted wake un-muted, want the mute kept")
	}
	if !strings.Contains(log.String(), "f3sctl monitoring unmute") || strings.Contains(log.String(), "ssh -p") {
		t.Errorf("log = %q, want the CLI hint and no SSH hint", log.String())
	}
}

// An inventory without gateways makes no call through the switch, matching
// the SSH path's no-op over an empty gateway list.
func TestMonitorWithoutGatewaysDoesNotCallTheSwitch(t *testing.T) {
	sw := &fakeSwitch{err: errors.New("must not be called")}
	m := newTestMonitor(t, &fakeGatewayVerb{}, allNodesUp, nil, []string{"r0"}, time.Minute)
	m.via = sw
	if err := m.UnmuteGogios(context.Background(), io.Discard, nil); err != nil {
		t.Fatalf("UnmuteGogios: %v", err)
	}
	if len(sw.callsList()) != 0 {
		t.Error("switch called with no gateways in the inventory")
	}
}

// WithGatewaySwitch works on a hand-built Engine (no monitor yet), and nil
// restores the SSH verb.
func TestEngineWithGatewaySwitchInstallsAndClears(t *testing.T) {
	var cfg config.Config
	cfg.Inventory.Hosts = []inventory.Host{{Name: "blowfish", Role: inventory.RoleGateway}}
	e := &Engine{cfg: cfg}
	sw := &fakeSwitch{}
	e.WithGatewaySwitch(sw)
	if e.monitor == nil || e.monitor.via != sw {
		t.Fatalf("monitor.via = %v, want the switch", e.monitor)
	}
	if err := e.UnmuteNow(context.Background(), io.Discard); err != nil {
		t.Fatalf("UnmuteNow: %v", err)
	}
	if got := sw.callsList(); len(got) != 1 {
		t.Errorf("switch calls = %v, want one", got)
	}

	e.WithGatewaySwitch(nil)
	if e.monitor.via != nil {
		t.Error("WithGatewaySwitch(nil) left a switch installed")
	}
}
