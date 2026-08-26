package power

// This file is the Gogios monitoring concern, split off Engine (see o51):
// muting and un-muting the OpenBSD gateways' alerting, reading back the
// per-gateway state, and the wake path's wait for the k3s nodes before it
// clears a mute it made. Engine used to carry all of this alongside the
// shutdown and fan-guard policy; the split leaves Engine as the sequencing
// facade (off mutes, on un-mutes after the cluster answers) and puts the
// "talk to the gateways and wait for the cluster" mechanism here, on a type
// that holds only what that concern needs.
//
// The transport is the same allowlisted-agent-verb SSH call the shutdown and
// zusb paths make, narrowed to the one method this concern uses (gatewayVerb,
// below) so a fake standing in for the Monitor does not have to satisfy the
// whole PowerBackend. The cluster wait reuses Engine.Probe through a func
// field rather than reaching back into Engine, so the Monitor is testable
// without an Engine at all.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/inventory"
)

// gatewayVerb is the narrow transport the Monitor reaches the gateways
// through: one allowlisted agent verb over SSH -- the same PowerBackend.
// AgentVerb the shutdown and zusb paths use, but nothing else. Narrowing it
// to its own interface keeps a fake standing in for the Monitor from having
// to satisfy the whole PowerBackend (PowerOff, Wake, ...): the Monitor never
// wakes a host or powers one off.
type gatewayVerb interface {
	AgentVerb(ctx context.Context, h inventory.Host, verb string) (string, error)
}

// Monitor mutes, un-mutes and reports the Gogios alerting state on the
// OpenBSD gateways, and waits for the k3s nodes before un-muting at the end
// of a wake.
//
// It holds only what this concern needs: the gateway and cluster-node lists
// resolved out of the inventory, the un-mute wait budget, the verb it runs
// on each gateway, and the probe it uses to wait for the cluster. Engine
// holds one (New wires it) and delegates; the policy that decides WHEN to
// mute or un-mute (off mutes, on un-mutes) stays on Engine, since that is a
// sequencing decision about the shutdown, not a fact about the gateways.
type Monitor struct {
	// gateways are the OpenBSD frontends the marker is set on (and read from
	// by Status). Resolved out of the inventory once, at construction, so the
	// Monitor does not reach back into a config per call.
	gateways []inventory.Host
	// nodes are the k3s hosts UnmuteGogios waits for before clearing the
	// marker -- the alerts being suppressed are scraped from that cluster.
	nodes []inventory.Host
	// unmute bounds that wait. A field rather than a literal so a test or a
	// slow-boot fleet can widen it without touching the policy layer.
	unmute time.Duration
	// verb runs one allowlisted agent verb on a gateway. The same seam
	// eachGateway's mute, un-mute and Status calls go through, so a fake
	// stands in for the transport once rather than per call. Held by value
	// of the interface, set at construction (New) -- the Monitor is the seam
	// for faking monitoring, replacing the old "set Engine.power" route.
	verb gatewayVerb
	// probe reports each cluster node's reachability for UnmuteGogios's wait.
	// A func rather than a method so the Monitor is testable without an
	// Engine: a test hands it a stub that says "they are all up" and skips
	// the real ICMP. Wired to Engine.Probe in production.
	probe func(ctx context.Context, hosts []inventory.Host) []HostStatus
}

// NewMonitor builds a Monitor over the gateways and cluster nodes in cfg,
// reaching the gateways through verb and the cluster through probe.
func NewMonitor(cfg config.Config, verb gatewayVerb, probe func(ctx context.Context, hosts []inventory.Host) []HostStatus) *Monitor {
	return &Monitor{
		gateways: cfg.Inventory.ByRole(inventory.RoleGateway),
		nodes:    cfg.Inventory.ByRole(inventory.RoleCluster),
		unmute:   cfg.UnmuteTimeout.D(),
		verb:     verb,
		probe:    probe,
	}
}

// Mute creates the marker that suppresses the Gogios checks derived from the
// cluster's own Prometheus, on both OpenBSD gateways.
//
// Without it, deliberately taking the cluster down pages as if it had failed.
func (m *Monitor) Mute(ctx context.Context, log io.Writer) error {
	return m.eachGateway(ctx, log, "gogios-mute", "muted")
}

// GatewayMute is one gateway's monitoring state.
type GatewayMute struct {
	Name  string
	Muted bool
	Err   error
}

// Status reports, per gateway, whether Gogios is currently muted.
//
// This exists because a mute can outlive the shutdown that created it. The wake
// path un-mutes only after r0/r1/r2 answer, and on giving up it deliberately
// leaves the marker in place -- so "muted" is a state the fleet can sit in
// indefinitely with nobody watching. It has to be observable, not merely
// settable.
//
// Reads the verb through m.verb, the same seam eachGateway's mute and un-mute
// calls go through, so the transport is fakeable one way rather than carrying
// a second path to the SSH mechanism for tests to track.
func (m *Monitor) Status(ctx context.Context) []GatewayMute {
	out := make([]GatewayMute, 0, len(m.gateways))

	for _, gw := range m.gateways {
		st := GatewayMute{Name: gw.Name}
		res, err := m.verb.AgentVerb(ctx, gw, "gogios-status")
		switch {
		case err != nil:
			st.Err = err
		default:
			st.Muted = strings.TrimSpace(res) == "muted"
		}
		out = append(out, st)
	}
	return out
}

// AnyMuted reports whether monitoring is suppressed on at least one gateway.
//
// One of two muted is still a monitoring gap, and it is the state a failed
// un-mute leaves behind, so the "should we offer to un-mute?" question keys on
// any rather than all.
func AnyMuted(states []GatewayMute) bool {
	for _, st := range states {
		if st.Err == nil && st.Muted {
			return true
		}
	}
	return false
}

// Unmute removes the marker without waiting for the k3s nodes.
//
// UnmuteGogios is the right call at the end of a wake, where waiting prevents a
// storm of alerts from a cluster that is still booting. This is the operator's
// escape hatch for the other case: the fleet is already up, monitoring is
// still muted because an earlier un-mute timed out, and there is nothing left
// to wait for. Without it a stranded mute can only be cleared by hand over SSH.
func (m *Monitor) Unmute(ctx context.Context, log io.Writer) error {
	return m.eachGateway(ctx, log, "gogios-unmute", "un-muted")
}

// UnmuteGogios waits for the k3s nodes to come back, then removes the marker.
//
// It waits because the alerts being suppressed are scraped from that cluster:
// clearing the marker while the nodes are still booting would fire every one
// of them. It does not wait forever, and on giving up it leaves the marker in
// place and says so — a stuck mute is a monitoring gap, so the operator needs
// to know it happened.
//
// Gogios does expire the marker itself after PrometheusOnlyIfNotExistsMaxS
// (24h). Relying on that expiry is what hid a two-day audiobookshelf outage in
// August 2026 after an early wake-up, which is why the wake path clears it
// explicitly instead.
func (m *Monitor) UnmuteGogios(ctx context.Context, log io.Writer) error {
	fmt.Fprintln(log, "Waiting for the k3s nodes before un-muting Gogios monitoring...")

	if err := m.waitForCluster(ctx, log); err != nil {
		fmt.Fprintf(log, "  %v\n", err)
		fmt.Fprintf(log, "  Leaving Gogios muted. Clear it by hand once the nodes are up:\n")
		for _, gw := range m.gateways {
			fmt.Fprintf(log, "    ssh -p %d %s@%s gogios-unmute\n", gw.SSHPort, gw.SSHUser, gw.IP)
		}
		return err
	}

	return m.eachGateway(ctx, log, "gogios-unmute", "un-muted")
}

// waitForCluster polls r0/r1/r2 until all three answer or the timeout expires.
func (m *Monitor) waitForCluster(ctx context.Context, log io.Writer) error {
	deadline := time.Now().Add(m.unmute)

	for {
		pending := 0
		for _, st := range m.probe(ctx, m.nodes) {
			if !st.Ping {
				pending++
			}
		}
		if pending == 0 {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("%d of %d k3s nodes still unreachable after %s",
				pending, len(m.nodes), m.unmute)
		}

		fmt.Fprintf(log, "  %d of %d k3s nodes still down; waiting...\n", pending, len(m.nodes))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Second):
		}
	}
}

// eachGateway runs verb on every gateway, reporting per-gateway failures but
// continuing to the rest.
//
// Both gateways are tried even if the first fails: they are independent Gogios
// installs, and muting one of two is strictly better than muting neither.
func (m *Monitor) eachGateway(ctx context.Context, log io.Writer, verb, past string) error {
	var failed []string

	for _, gw := range m.gateways {
		if _, err := m.verb.AgentVerb(ctx, gw, verb); err != nil {
			fmt.Fprintf(log, "  ! %v\n", err)
			failed = append(failed, gw.Name)
			continue
		}
		fmt.Fprintf(log, "  Gogios %s on %s\n", past, gw.Name)
	}

	if len(failed) > 0 {
		return fmt.Errorf("could not %s Gogios on: %v", verb, failed)
	}
	return nil
}

// --- Engine delegation ------------------------------------------------------
//
// The four methods below are the Engine's now-thin surface for monitoring:
// they delegate to monitorBackend() so the gateway/cluster-wait mechanism
// stays on Monitor and Engine reads as the sequencing facade only (off mutes,
// on un-mutes after the cluster answers). Kept here, next to Monitor, so the
// monitoring concern is in one file rather than split across engine.go and
// monitor.go.

// monitorBackend returns the Monitor the Engine delegates monitoring to,
// falling back to a real one built from cfg when the seam is unset (a
// hand-built Engine), the same nil-safe pattern as powerBackend/probeBackend/
// fansBackend/nfsBackend/zusbBackend. The fallback uses the live powerBackend
// and Engine.Probe, so an Engine somebody built with a struct literal reaches
// the gateways the same way one built with New does.
func (e *Engine) monitorBackend() *Monitor {
	if e.monitor != nil {
		return e.monitor
	}
	return NewMonitor(e.cfg, e.powerBackend(), e.Probe)
}

// MuteGogios creates the marker that suppresses Gogios alerting on both
// gateways. Delegates to the Monitor; see Monitor.Mute.
func (e *Engine) MuteGogios(ctx context.Context, log io.Writer) error {
	return e.monitorBackend().Mute(ctx, log)
}

// UnmuteNow removes the marker without waiting for the k3s nodes. Delegates to
// the Monitor; see Monitor.Unmute.
func (e *Engine) UnmuteNow(ctx context.Context, log io.Writer) error {
	return e.monitorBackend().Unmute(ctx, log)
}

// UnmuteGogios waits for the k3s nodes and then removes the marker. Delegates
// to the Monitor; see Monitor.UnmuteGogios.
func (e *Engine) UnmuteGogios(ctx context.Context, log io.Writer) error {
	return e.monitorBackend().UnmuteGogios(ctx, log)
}

// MonitoringStatus reports, per gateway, whether Gogios is currently muted.
// Delegates to the Monitor; see Monitor.Status.
func (e *Engine) MonitoringStatus(ctx context.Context) []GatewayMute {
	return e.monitorBackend().Status(ctx)
}
