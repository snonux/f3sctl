package power

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/snonux/f3sctl/internal/inventory"
)

// MuteGogios creates the marker that suppresses the Gogios checks derived from
// the cluster's own Prometheus, on both OpenBSD gateways.
//
// Without it, deliberately taking the cluster down pages as if it had failed.
func (e *Engine) MuteGogios(ctx context.Context, log io.Writer) error {
	return e.eachGateway(ctx, log, "gogios-mute", "muted")
}

// GatewayMute is one gateway's monitoring state.
type GatewayMute struct {
	Name  string
	Muted bool
	Err   error
}

// MonitoringStatus reports, per gateway, whether Gogios is currently muted.
//
// This exists because a mute can outlive the shutdown that created it. The wake
// path un-mutes only after r0/r1/r2 answer, and on giving up it deliberately
// leaves the marker in place -- so "muted" is a state the fleet can sit in
// indefinitely with nobody watching. It has to be observable, not merely
// settable.
func (e *Engine) MonitoringStatus(ctx context.Context) []GatewayMute {
	gateways := e.cfg.Inventory.ByRole(inventory.RoleGateway)
	out := make([]GatewayMute, 0, len(gateways))

	for _, gw := range gateways {
		st := GatewayMute{Name: gw.Name}
		res, err := e.ssh.agentVerb(ctx, gw, "gogios-status")
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

// UnmuteNow removes the marker without waiting for the k3s nodes.
//
// UnmuteGogios is the right call at the end of a wake, where waiting prevents a
// storm of alerts from a cluster that is still booting. This is the operator's
// escape hatch for the other case: the fleet is already up, monitoring is still
// muted because an earlier un-mute timed out, and there is nothing left to wait
// for. Without it a stranded mute can only be cleared by hand over SSH.
func (e *Engine) UnmuteNow(ctx context.Context, log io.Writer) error {
	return e.eachGateway(ctx, log, "gogios-unmute", "un-muted")
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
func (e *Engine) UnmuteGogios(ctx context.Context, log io.Writer) error {
	fmt.Fprintln(log, "Waiting for the k3s nodes before un-muting Gogios monitoring...")

	if err := e.waitForCluster(ctx, log); err != nil {
		fmt.Fprintf(log, "  %v\n", err)
		fmt.Fprintf(log, "  Leaving Gogios muted. Clear it by hand once the nodes are up:\n")
		for _, gw := range e.cfg.Inventory.ByRole(inventory.RoleGateway) {
			fmt.Fprintf(log, "    ssh -p %d %s@%s gogios-unmute\n", gw.SSHPort, gw.SSHUser, gw.IP)
		}
		return err
	}

	return e.eachGateway(ctx, log, "gogios-unmute", "un-muted")
}

// waitForCluster polls r0/r1/r2 until all three answer or the timeout expires.
func (e *Engine) waitForCluster(ctx context.Context, log io.Writer) error {
	nodes := e.cfg.Inventory.ByRole(inventory.RoleCluster)
	deadline := time.Now().Add(e.cfg.UnmuteTimeout.D())

	for {
		pending := 0
		for _, st := range e.Probe(ctx, nodes) {
			if !st.Ping {
				pending++
			}
		}
		if pending == 0 {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("%d of %d k3s nodes still unreachable after %s",
				pending, len(nodes), e.cfg.UnmuteTimeout.D())
		}

		fmt.Fprintf(log, "  %d of %d k3s nodes still down; waiting...\n", pending, len(nodes))
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
func (e *Engine) eachGateway(ctx context.Context, log io.Writer, verb, past string) error {
	gateways := e.cfg.Inventory.ByRole(inventory.RoleGateway)
	var failed []string

	for _, gw := range gateways {
		if _, err := e.powerBackend().AgentVerb(ctx, gw, verb); err != nil {
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
