package power

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/snonux/f3sctl/internal/inventory"
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
	// lacked /sbin (see pingCandidates) advertised fans-off, unforced, against a
	// fully running rack. Anything deciding on this must go through
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

// pingCandidates are where ping(8) lives, most likely first.
//
// It is looked up by absolute path rather than through PATH because the CGI
// runs with the environment bozohttpd hands it -- on NetBSD that is
// /usr/bin:/bin:/usr/pkg/bin:/usr/local/bin, which does NOT include /sbin
// where ping actually is. Relying on PATH made every host report ping=false
// while plainly answering on port 22, which in turn withheld the power-off
// action entirely.
//
// That incident is also why a missing ping(8) reports unknown rather than
// down: it is a whole-fleet false negative, in the environment `power off`
// actually runs in, and read as "every host is silent" it says the rack is
// cold and its cooling can go.
var pingCandidates = []string{"/sbin/ping", "/usr/sbin/ping", "/bin/ping", "/usr/bin/ping"}

// pingPath resolves ping(8) once per process.
var pingPath = sync.OnceValue(func() string {
	for _, p := range pingCandidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	// Last resort, for a platform that keeps it somewhere else entirely.
	if p, err := exec.LookPath("ping"); err == nil {
		return p
	}
	return ""
})

// pingOnce sends a single ICMP echo and reports whether it was answered, and
// whether the probe reached a conclusion at all.
//
// This shells out to ping(8) instead of building an ICMP socket in Go. An
// unprivileged ICMP datagram socket exists on Linux but not on NetBSD, so a
// native implementation would need a raw socket and thus root -- which the CGI
// (running as _httpd under bozohttpd) does not have and should not get.
// NetBSD's /sbin/ping is setuid root, so shelling out gives real ICMP with no
// privilege grant at all. Verified working as _httpd on pi0.
func (e *Engine) pingOnce(ctx context.Context, ip string) (up, known bool) {
	return e.pingWith(ctx, pingPath(), ip)
}

// pingWith is pingOnce against an explicitly named ping(8).
//
// Split out so the part that matters most -- telling "the host said nothing"
// apart from "the probe never happened" -- can be tested with an ordinary
// executable standing in for ping, without ICMP, root, or a network.
func (e *Engine) pingWith(ctx context.Context, bin, ip string) (up, known bool) {
	if bin == "" {
		return false, false
	}

	timeout := e.cfg.ProbeTimeout.D()
	secs := strconv.Itoa(int(timeout.Seconds()))

	// The per-packet deadline flag differs per platform and there is no
	// portable spelling: -w on NetBSD and Linux, -t on FreeBSD/OpenBSD/macOS.
	var args []string
	switch runtime.GOOS {
	case "freebsd", "openbsd", "darwin":
		args = []string{"-c", "1", "-t", secs, ip}
	default: // netbsd, linux
		args = []string{"-c", "1", "-w", secs, ip}
	}

	// The context deadline is a backstop in case ping ignores its own flag;
	// without it a wedged ping would hold the whole probe open.
	ctx, cancel := context.WithTimeout(ctx, timeout+time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	switch {
	case err == nil:
		return true, true

	case ctx.Err() != nil:
		// ping was killed by the backstop above, or the caller went away
		// first. Either way it never got to report anything about the host.
		return false, false

	case errors.As(err, new(*exec.ExitError)) && probeRan(out):
		// ping ran to completion, reported its statistics and exited non-zero:
		// it sent the echo and heard nothing back within the deadline.
		return false, true

	default:
		// Everything else is "no measurement was taken": the binary could not
		// be started (not found, not executable, fork failed), or it started
		// and gave up before sending anything.
		return false, false
	}
}

// probeRan reports whether ping's output contains the summary it prints once it
// has actually sent packets and counted the replies.
//
// This is what separates "the host said nothing" from "the probe never
// happened", and it replaces decoding the exit code, which cannot be done
// portably here: "no reply" is 1 on Linux and 2 on the BSDs, where 1 is a hard
// error, so a table keyed on GOOS would have to be right about every platform
// or it would turn either ordinary powered-off hosts into "unknown" (a rack
// that can never be declared idle) or hard errors into "down" (a rack declared
// idle that is nothing of the sort). The latter is the dangerous one and it was
// live: every *exec.ExitError counted as a confirmed silence, so any condition
// hitting all four hosts alike -- no route, ICMP filtered by a switch, a ping(8)
// that is neither setuid nor setcap and so cannot open its socket -- reported
// the whole rack down on all three probes and cut the fan plug.
//
// Verified on this Linux box: a genuine "no reply" exits 1 having printed
// "1 packets transmitted, 0 received", while `ping no.such.host.invalid` exits 2
// printing "Name or service not known" and no statistics at all. Every ping(8)
// on Linux, NetBSD, FreeBSD, OpenBSD and macOS prints this line when it got as
// far as transmitting, and none print it when it did not, so matching on it
// needs no per-platform knowledge.
func probeRan(out []byte) bool {
	return bytes.Contains(out, []byte("packets transmitted"))
}

// dialSSH reports whether the host's sshd is accepting connections. This is
// the "finished booting" signal; no authentication is attempted.
func (e *Engine) dialSSH(ctx context.Context, h inventory.Host) bool {
	d := net.Dialer{Timeout: e.cfg.ProbeTimeout.D()}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(h.IP, strconv.Itoa(h.SSHPort)))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
