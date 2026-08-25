package infra

// This file holds the probe mechanism the status table and the fan guards
// read from -- the ICMP echo (already in this package as Ping) and the bare
// TCP dial of sshd that says "finished booting" -- pulled off
// internal/power's Engine so that changing how a host is probed (a native
// ICMP socket, a different sshd-detection) is an edit here, not an edit to
// the Engine that also carries the shutdown and fan-guard policy.
//
// The retry/settle policy that sits on top -- confirmLiveness' consecutive
// silences, awaitPowerDown's bounded wait, the fan guard that wants
// confirmedDownProbes misses -- stays in internal/power. ProbeClient carries
// one probe of one host, no opinion about what to conclude from it. The
// tri-state contract (up/known) is enforced in Ping itself, not here; this is
// only the configured-timeout seam plus the sshd dial.

import (
	"context"
	"net"
	"strconv"
	"time"
)

// ProbeClient reaches a host to answer "is it there" without changing
// anything on it: an ICMP echo via ping(8), tri-state because a probe that
// could not be carried out must never be confused with a host that stayed
// silent (see Ping's doc), and a bare TCP dial of sshd, which only says
// whether the host has finished booting. It holds only the mechanism -- the
// configured per-probe timeout -- and leaves the retry/settle policy built on
// top of it to internal/power.
type ProbeClient struct {
	// Timeout bounds both halves of a probe: it is the per-packet deadline
	// handed to ping(8) (spelled with whichever flag this OS wants, see
	// pingArgs) and the TCP dial timeout for the sshd half. A field rather
	// than a hard-coded literal so a test or a slow network can widen it
	// without touching the policy layer.
	Timeout time.Duration
}

// Ping sends a single ICMP echo to ip via the resolved ping(8) and reports
// whether it was answered, and -- separately -- whether the probe reached a
// conclusion at all. This is the real-world default behind internal/power's
// isUp seam; see Ping for the tri-state contract this must not weaken.
func (c *ProbeClient) Ping(ctx context.Context, ip string) (up, known bool) {
	return Ping(ctx, PingPath(), ip, c.Timeout)
}

// PingWith is Ping against an explicitly named ping(8).
//
// Split out so the part that matters most -- telling "the host said nothing"
// apart from "the probe never happened" -- can be tested with an ordinary
// executable standing in for ping, without ICMP, root, or a network.
func (c *ProbeClient) PingWith(ctx context.Context, bin, ip string) (up, known bool) {
	return Ping(ctx, bin, ip, c.Timeout)
}

// SSH reports whether the host's sshd is accepting connections. This is the
// "finished booting" signal; no authentication is attempted. The dial is
// bounded by the client's configured timeout, the same one the ICMP half
// uses, so a host that is off does not hold the whole probe open longer than
// its ping would.
func (c *ProbeClient) SSH(ctx context.Context, ip string, port int) bool {
	d := net.Dialer{Timeout: c.Timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
