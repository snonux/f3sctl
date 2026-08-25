package power

import (
	"context"
	"sync"
	"time"

	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power/infra"
)

// HostStatus is one host's observed state.
//
// Two independent signals are reported rather than a single "up", because the
// difference between them is operationally meaningful:
//
//	Ping && SSH    the host is up and serving
//	Ping && !SSH   the host is booting (or sshd is wedged)
//	!Ping && !SSH  the host is off -- or hung in single-user after a failed
//	               shutdown, in which case it is powered on, has no network,
//	               and Wake-on-LAN will not wake it. Worth knowing before
//	               pressing "on" again and concluding the button is broken.
type HostStatus struct {
	Name string `json:"name"`
	Role string `json:"role"`
	IP   string `json:"ip"`
	Ping bool   `json:"ping"`
	// PingKnown says whether the ICMP probe reached a conclusion at all. False
	// means it never ran -- no ping(8), a binary that could not be started, a
	// context cut short -- which is NOT the same as a host that said nothing.
	//
	// It is a separate field rather than a third state of Ping because Ping is
	// what gets displayed, and a display wants two columns. Anything that
	// *decides* something must read both, through liveness(): the zero value of
	// this struct is therefore "silent, and we do not know why", which is the
	// safe reading for a HostStatus that some other package built by hand.
	PingKnown bool    `json:"pingKnown"`
	SSH       bool    `json:"ssh"`
	MS        float64 `json:"ms"`
}

// liveness folds the two ping fields back into the tri-state the fan guards
// decide on. See RackActivityFrom, which is the only caller that matters.
func (h HostStatus) liveness() hostLiveness {
	switch {
	case h.Ping:
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
	// HostStatus.liveness rather than Ping alone.
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
