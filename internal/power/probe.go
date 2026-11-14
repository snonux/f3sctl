package power

import (
	"context"
	"sync"
	"time"

	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power/infra"
	"github.com/snonux/f3sctl/internal/status"
)

// HostStatus is one host's observed state. The type lives in the leaf
// package internal/status so that the presenter and the remote client can
// use it without importing the engine; the alias keeps it the engine's own
// name, so nothing that already speaks power.HostStatus has to change.
type HostStatus = status.HostStatus

// livenessOf folds a status's probe fields back into the tri-state the fan
// guards decide on. Its only caller is snapshotActivity, which both
// RackActivityFrom (the fan guard) and ACActivityFrom (the AC guard) go
// through.
//
// Up is HostStatus.Reachable, not Ping: a host whose sshd answers is running
// even when ICMP to it is filtered, and must keep its cooling and its mains.
func livenessOf(h HostStatus) hostLiveness {
	switch {
	case h.Reachable():
		return livenessUp
	case !h.PingKnown:
		return livenessUnknown
	default:
		return livenessDown
	}
}

// Probe returns the status of every host in hosts, probed concurrently.
func (e *Engine) Probe(ctx context.Context, hosts []inventory.Host) []HostStatus {
	out := make([]HostStatus, len(hosts))
	var wg sync.WaitGroup

	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h inventory.Host) {
			defer wg.Done()
			out[i] = e.probeOne(ctx, h)
		}(i, h)
	}

	wg.Wait()
	return out
}

func (e *Engine) probeOne(ctx context.Context, h inventory.Host) HostStatus {
	st := HostStatus{Name: h.Name, Role: string(h.Role), IP: h.IP}

	// Both probes run concurrently: the TCP dial is the slow one when a host
	// is off (it waits out the full timeout), and there is no reason to make
	// the ping wait behind it.
	var wg sync.WaitGroup
	wg.Add(2)

	// Both halves are recorded, not just "did it answer".
	//
	// Discarding the second one was a real hole: HostStatus is the evidence the
	// HTTP API's fan guard reads, and with `known` thrown away a probe that
	// could not run looked exactly like a silent host -- so the CGI whose PATH
	// lacked /sbin (see infra.pingCandidates) advertised fans-off, unforced,
	// against a fully running rack. Anything deciding on this must go through
	// livenessOf rather than Ping alone.
	go func() {
		defer wg.Done()
		start := time.Now()
		up, known := e.liveness()(ctx, h.IP)
		st.Ping, st.PingKnown = up, known
		if st.Ping {
			st.MS = float64(time.Since(start).Microseconds()) / 1000
		}
	}()

	go func() {
		defer wg.Done()
		st.SSH = e.probeBackend().SSH(ctx, h)
	}()

	wg.Wait()
	return st
}

// answers probes h with ICMP and an sshd dial at once, and folds the two into
// the (up, known) pair the strict decisions read: up when either answered,
// known when either reached a conclusion.
//
// This is what every "may this host be off" decision that probes on demand
// goes through -- skipAlreadyOff (via isRunning), confirmLiveness (the fan and
// AC guards) and awaitPowerDown -- so that a host whose ICMP is filtered but
// whose sshd accepts a connection is never treated as off, silent or cold.
// probeOne keeps the two signals apart instead, because it describes what was
// observed rather than acting on it.
//
// The two probes run concurrently, as in probeOne: on a silent host both wait
// out ProbeTimeout, and running them one after the other would double the
// cost of every confirmation round. An echo reply settles the question, so
// it cancels the dial rather than waiting out a filtered or not-yet-started
// sshd (a host mid-boot or mid-shutdown answers ping long before or after
// sshd).
func (e *Engine) answers(ctx context.Context, h inventory.Host) (up, known bool) {
	sshCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var sshUp bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		sshUp = e.probeBackend().SSH(sshCtx, h)
	}()
	pingUp, pingKnown := e.liveness()(ctx, h.IP)
	if pingUp {
		cancel()
	}
	wg.Wait()
	return pingUp || sshUp, pingKnown || sshUp
}

// pingOnce sends a single ICMP echo and reports whether it was answered, and
// whether the probe reached a conclusion at all.
//
// The actual mechanism -- locating ping(8) (which differs per OS: see
// infra.pingCandidates) and shelling out to it rather than building an ICMP
// socket in Go -- is platform detail with no policy attached, so it lives in
// internal/power/infra. What stays here is that this IS the seam Engine.isUp
// defaults to (see New and the isUp field's doc in engine.go): three
// safety-critical decisions read isUp/liveness rather than infra.Ping
// directly, and this function is what ties the two together for real use.
func (e *Engine) pingOnce(ctx context.Context, ip string) (up, known bool) {
	return e.pingWith(ctx, infra.PingPath(), ip)
}

// pingWith is pingOnce against an explicitly named ping(8).
//
// Split out so the part that matters most -- telling "the host said nothing"
// apart from "the probe never happened" -- can be tested with an ordinary
// executable standing in for ping, without ICMP, root, or a network. The
// tri-state contract (see infra.Ping's doc) is enforced there, not here; this
// is only the seam plus the timeout this Engine is configured with.
//
// The ICMP mechanism itself lives in infra.Ping; this and infra.ProbeClient.Ping
// are the same mechanism reachable two ways (see ProbeBackend's doc in
// backends.go) -- both call infra.Ping with this Engine's configured timeout,
// so they can never disagree about what a probe concluded.
func (e *Engine) pingWith(ctx context.Context, bin, ip string) (up, known bool) {
	return infra.Ping(ctx, bin, ip, e.cfg.ProbeTimeout.D())
}
