package power

// This file is Engine's thin surface for Gogios monitoring. The mechanism --
// talking to the gateways, reading their state, waiting for the k3s nodes
// before an un-mute -- lives in internal/gogios (gogios.Monitor, moved out of
// this package in task ha); Engine keeps only the sequencing policy (off
// mutes, on un-mutes after the cluster answers) and reaches the mechanism
// through gogiosMonitor, the few methods that policy calls.

import (
	"context"
	"io"

	"github.com/snonux/f3sctl/internal/gogios"
	"github.com/snonux/f3sctl/internal/inventory"
)

// gogiosMonitor is what Engine needs from the Gogios mute mechanism: the
// mute, the two un-mutes, and the per-gateway read-back. *gogios.Monitor
// satisfies it in production; tests substitute a fake to script the
// mechanism's outcomes (ErrClusterIncomplete, ErrWaitAbandoned, a failed
// gateway) without a wait loop or a gateway.
type gogiosMonitor interface {
	// Mute sets the marker on every gateway. See gogios.Monitor.Mute.
	Mute(ctx context.Context, log io.Writer) error
	// Unmute clears it without waiting. See gogios.Monitor.Unmute.
	Unmute(ctx context.Context, log io.Writer) error
	// UnmuteGogios waits for the k3s nodes, calling rewake while any is
	// down, then clears the marker. See gogios.Monitor.UnmuteGogios.
	UnmuteGogios(ctx context.Context, log io.Writer, rewake func()) error
	// Status reads the marker from every gateway. See gogios.Monitor.Status.
	Status(ctx context.Context) []gogios.GatewayMute
}

// newMonitor builds the real Gogios monitor over e's config, reaching the
// gateways through the live powerBackend's allowlisted agent verb and the
// cluster through downNodes.
func (e *Engine) newMonitor() *gogios.Monitor {
	return gogios.NewMonitor(e.cfg, e.powerBackend(), e.downNodes)
}

// downNodes is the gogios.NodeProbe over Engine.Probe: the names of the hosts
// whose ping did not answer.
func (e *Engine) downNodes(ctx context.Context, hosts []inventory.Host) []string {
	var down []string
	for _, st := range e.Probe(ctx, hosts) {
		if !st.Ping {
			down = append(down, st.Name)
		}
	}
	return down
}

// monitorBackend returns the monitor the Engine delegates monitoring to,
// falling back to a real one built from cfg when the seam is unset (a
// hand-built Engine), the same nil-safe pattern as powerBackend/probeBackend/
// fansBackend/nfsBackend/zusbBackend. The fallback uses the live powerBackend
// and Engine.Probe, so an Engine somebody built with a struct literal reaches
// the gateways the same way one built with New does.
func (e *Engine) monitorBackend() gogiosMonitor {
	if e.monitor != nil {
		return e.monitor
	}
	return e.newMonitor()
}

// WithGatewaySwitch makes mute and un-mute reach the gateways through s
// instead of this process's SSH key; nil restores the SSH verb. The CLI
// installs the API route when no key is readable locally -- see
// gogios.GatewaySwitch.
//
// When the Engine already holds a *gogios.Monitor (New wires one), the switch
// is installed on that monitor, keeping everything else it was built with.
// Otherwise -- a hand-built Engine, or a test's fake monitor, which has no
// switch to set -- it installs a fresh gogios.Monitor built exactly as New
// builds one.
func (e *Engine) WithGatewaySwitch(s gogios.GatewaySwitch) *Engine {
	if m, ok := e.monitor.(*gogios.Monitor); ok {
		m.WithSwitch(s)
		return e
	}
	e.monitor = e.newMonitor().WithSwitch(s)
	return e
}

// MuteGogios creates the marker that suppresses Gogios alerting on both
// gateways. Delegates to the monitor; see gogios.Monitor.Mute.
func (e *Engine) MuteGogios(ctx context.Context, log io.Writer) error {
	return e.monitorBackend().Mute(ctx, log)
}

// UnmuteNow removes the marker without waiting for the k3s nodes. Delegates to
// the monitor; see gogios.Monitor.Unmute.
func (e *Engine) UnmuteNow(ctx context.Context, log io.Writer) error {
	return e.monitorBackend().Unmute(ctx, log)
}

// UnmuteGogios waits for the k3s nodes and then removes the marker, calling
// rewake (may be nil) periodically while nodes are still down. Delegates to
// the monitor; see gogios.Monitor.UnmuteGogios.
func (e *Engine) UnmuteGogios(ctx context.Context, log io.Writer, rewake func()) error {
	return e.monitorBackend().UnmuteGogios(ctx, log, rewake)
}

// MonitoringStatus reports, per gateway, whether Gogios is currently muted.
// Delegates to the monitor; see gogios.Monitor.Status.
func (e *Engine) MonitoringStatus(ctx context.Context) []gogios.GatewayMute {
	return e.monitorBackend().Status(ctx)
}
