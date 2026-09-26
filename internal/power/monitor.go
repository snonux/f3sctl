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
// whole PowerBackend. Mute and un-mute can instead go through a GatewaySwitch
// (the pi0/pi1 API) when this process holds no key the gateways accept -- a
// local wake from a laptop. The cluster wait reuses Engine.Probe through a func
// field rather than reaching back into Engine, so the Monitor is testable
// without an Engine at all.

import (
	"context"
	"errors"
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

// GatewaySwitch sets or clears the Gogios mute on every gateway by some route
// other than this process's own SSH key -- in production, the pi0/pi1 HTTP
// API, which reaches the gateways with the key that is pinned to those two
// hosts.
//
// It exists because the wake path runs where the magic packet can be sent
// (any LAN host, e.g. a laptop) but the gateways only accept the restricted
// f3sctl key from pi0/pi1 (README "Security model"). Without it a local
// "power on" from a laptop woke the rack and then failed to un-mute with "no
// readable SSH identity", while "monitoring unmute" -- routed through the API
// -- worked moments later (task 5l2). The power package cannot import the
// API client (the client imports power), so the CLI's composition root
// supplies the implementation and Engine.WithGatewaySwitch installs it.
//
// SetMute returns an error naming the gateways left in the wrong state, in
// the same "could not gogios-<verb> Gogios on: [...]" shape eachGateway uses,
// and logs one line per gateway like eachGateway does.
type GatewaySwitch interface {
	SetMute(ctx context.Context, log io.Writer, mute bool) error
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
	// poll is the gap between waitForCluster's probes, and rewakeEvery the
	// gap between the rewake callbacks it fires while nodes are still down.
	// Fields only so tests need not wait real seconds; zero means the
	// defaults (clusterPollInterval, rewakeInterval), read through pollGap
	// and rewakeGap.
	poll        time.Duration
	rewakeEvery time.Duration
	// via, when set, replaces the per-gateway SSH verb for mute and un-mute
	// (not for Status, which only the API server and a --local run on a Pi
	// call -- both hold the key). Nil means the SSH verb, which is right
	// wherever the pinned key is readable. See GatewaySwitch.
	via GatewaySwitch
}

// clusterPollInterval is how often waitForCluster re-probes r0/r1/r2.
const clusterPollInterval = 15 * time.Second

// rewakeInterval is how often the wake path re-sends its magic packets while
// k3s nodes are still unreachable. A single packet is not reliable: on
// 2026-09-25 f1 ignored the one "power on" sent, stayed off for seven hours,
// and a second packet sent by hand woke it first time. Re-sending to a host
// that is already up is harmless -- a running NIC ignores a magic packet.
const rewakeInterval = 2 * time.Minute

func (m *Monitor) pollGap() time.Duration {
	if m.poll <= 0 {
		return clusterPollInterval
	}
	return m.poll
}

func (m *Monitor) rewakeGap() time.Duration {
	if m.rewakeEvery <= 0 {
		return rewakeInterval
	}
	return m.rewakeEvery
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
	return m.setMute(ctx, log, true)
}

// setMute is the one place mute and un-mute pick their transport: the
// GatewaySwitch when one is installed, otherwise the allowlisted SSH verb on
// each gateway. An inventory without gateways has nothing to mute, so the
// switch is not consulted either -- the same no-op eachGateway makes of an
// empty list, rather than an API round trip about gateways nobody listed.
func (m *Monitor) setMute(ctx context.Context, log io.Writer, mute bool) error {
	if m.via != nil && len(m.gateways) > 0 {
		return m.via.SetMute(ctx, log, mute)
	}
	if mute {
		return m.eachGateway(ctx, log, "gogios-mute", "muted")
	}
	return m.eachGateway(ctx, log, "gogios-unmute", "un-muted")
}

// GatewayMute is one gateway's monitoring state.
type GatewayMute struct {
	Name  string
	Muted bool
	Err   error
}

// Status reports, per gateway, whether Gogios is currently muted.
//
// This exists because a mute can outlive the shutdown that created it: a mute
// set by "power off" stays until something clears it, and a wake that stops
// early never reaches the un-mute: the fans would not switch on, a magic
// packet could not be sent, a gateway could not be reached, or a local run was
// killed mid-wait. So
// "muted" is a state the fleet can sit in indefinitely with nobody watching.
// It has to be observable, not merely settable.
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
// escape hatch for the other case: monitoring is still muted with nothing left
// to wait for -- a "power off" never followed by a wake, a local wake killed
// mid-wait, or a gateway that could not be reached for the un-mute. Without it
// a stranded mute can only be cleared by hand over SSH.
func (m *Monitor) Unmute(ctx context.Context, log io.Writer) error {
	return m.setMute(ctx, log, false)
}

// UnmuteGogios waits for the k3s nodes to come back, then removes the marker.
//
// It waits because the alerts being suppressed are scraped from that cluster:
// clearing the marker while the nodes are still booting would fire every one
// of them. While it waits it calls rewake (if non-nil) every rewakeGap, so
// the wake path can re-send magic packets to hosts that ignored the first one.
//
// It does not wait forever, and when the budget runs out it un-mutes ANYWAY
// and returns an error naming the nodes that are missing. Leaving the marker
// in place on timeout is what hid f1 being down for seven hours on
// 2026-09-25: the node that failed to come back was exactly the one whose
// alerts the mute suppressed. A node still down after the budget is an
// outage, and an outage should page.
//
// Only a cancelled context leaves the marker, since then the caller abandoned
// the wake and nobody asked for monitoring to resume yet; it prints how to
// clear it by hand. This is what an operator's Ctrl-C (or a SIGTERM to the
// API's detached job) reaches: main binds the run's context to both signals.
//
// Gogios does expire the marker itself after PrometheusOnlyIfNotExistsMaxS
// (24h). Relying on that expiry is what hid a two-day audiobookshelf outage in
// August 2026 after an early wake-up, which is why the wake path clears it
// explicitly instead.
func (m *Monitor) UnmuteGogios(ctx context.Context, log io.Writer, rewake func()) error {
	fmt.Fprintln(log, "Waiting for the k3s nodes before un-muting Gogios monitoring...")

	waitErr := m.waitForCluster(ctx, log, rewake)
	if waitErr != nil && ctx.Err() != nil {
		fmt.Fprintf(log, "  %v\n", waitErr)
		fmt.Fprintf(log, "  Leaving Gogios muted. Clear it by hand once the nodes are up:\n")
		if m.via != nil {
			// No local key to SSH with; the same route the wake would have
			// used is the one to suggest.
			fmt.Fprintln(log, "    f3sctl monitoring unmute")
			return waitErr
		}
		for _, gw := range m.gateways {
			fmt.Fprintf(log, "    ssh -p %d %s@%s gogios-unmute\n", gw.SSHPort, gw.SSHUser, gw.IP)
		}
		return waitErr
	}
	if waitErr != nil {
		fmt.Fprintf(log, "  %v\n", waitErr)
		fmt.Fprintln(log, "  Un-muting Gogios anyway, so the missing nodes alert.")
	}

	unmuteErr := m.setMute(ctx, log, false)
	switch {
	case waitErr != nil && unmuteErr != nil:
		// One line, like every other error this package returns; %w keeps
		// errors.Is(err, ErrClusterIncomplete) working for Engine.on.
		return fmt.Errorf("%w; %v", waitErr, unmuteErr)
	case waitErr != nil:
		return waitErr
	default:
		return unmuteErr
	}
}

// ErrClusterIncomplete marks an UnmuteGogios error caused by k3s nodes that
// never answered, as opposed to a gateway that could not be un-muted. Engine.on
// tells the two apart: the first is an incomplete wake, the second a complete
// wake with a monitoring problem.
var ErrClusterIncomplete = errors.New("k3s nodes still unreachable")

// waitForCluster polls r0/r1/r2 until all three answer or the timeout expires,
// calling rewake (if non-nil) every rewakeGap while any node is still down.
func (m *Monitor) waitForCluster(ctx context.Context, log io.Writer, rewake func()) error {
	start := time.Now()
	deadline := start.Add(m.unmute)
	lastRewake := start

	for {
		var down []string
		for _, st := range m.probe(ctx, m.nodes) {
			if !st.Ping {
				down = append(down, st.Name)
			}
		}
		if len(down) == 0 {
			return nil
		}
		names := strings.Join(down, ", ")

		if time.Now().After(deadline) {
			return fmt.Errorf("%w after %s: %s", ErrClusterIncomplete, m.unmute, names)
		}

		if rewake != nil && time.Since(lastRewake) >= m.rewakeGap() {
			fmt.Fprintf(log, "  still down: %s; re-sending the magic packets...\n", names)
			rewake()
			lastRewake = time.Now()
		} else {
			fmt.Fprintf(log, "  still down: %s; waiting...\n", names)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.pollGap()):
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

// WithGatewaySwitch makes mute and un-mute reach the gateways through s
// instead of this process's SSH key; nil restores the SSH verb. The CLI
// installs the API route when no key is readable locally -- see GatewaySwitch.
func (e *Engine) WithGatewaySwitch(s GatewaySwitch) *Engine {
	if e.monitor == nil {
		e.monitor = NewMonitor(e.cfg, e.powerBackend(), e.Probe)
	}
	e.monitor.via = s
	return e
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

// UnmuteGogios waits for the k3s nodes and then removes the marker, calling
// rewake (may be nil) periodically while nodes are still down. Delegates to
// the Monitor; see Monitor.UnmuteGogios.
func (e *Engine) UnmuteGogios(ctx context.Context, log io.Writer, rewake func()) error {
	return e.monitorBackend().UnmuteGogios(ctx, log, rewake)
}

// MonitoringStatus reports, per gateway, whether Gogios is currently muted.
// Delegates to the Monitor; see Monitor.Status.
func (e *Engine) MonitoringStatus(ctx context.Context) []GatewayMute {
	return e.monitorBackend().Status(ctx)
}
