package power

import (
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
	Name string  `json:"name"`
	Role string  `json:"role"`
	IP   string  `json:"ip"`
	Ping bool    `json:"ping"`
	SSH  bool    `json:"ssh"`
	MS   float64 `json:"ms"`
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

	// Only the "did it answer" half is reported. HostStatus describes what was
	// observed rather than deciding anything with it, and a host that could
	// not be probed did not answer. The safety guards must not read it this
	// way, and do not: they go through Engine.isRunning, which keeps unknown
	// apart from silent.
	go func() {
		defer wg.Done()
		start := time.Now()
		up, _ := e.liveness()(ctx, h.IP)
		st.Ping = up
		if st.Ping {
			st.MS = float64(time.Since(start).Microseconds()) / 1000
		}
	}()

	go func() {
		defer wg.Done()
		st.SSH = e.dialSSH(ctx, h)
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

	err := exec.CommandContext(ctx, bin, args...).Run()
	switch {
	case err == nil:
		return true, true

	case ctx.Err() != nil:
		// ping was killed by the backstop above, or the caller went away
		// first. Either way it never got to report anything about the host.
		return false, false

	case errors.As(err, new(*exec.ExitError)):
		// ping ran to completion and exited non-zero: it sent the echo and
		// heard nothing back within the deadline.
		//
		// The exit *code* is deliberately not decoded. The platforms f3sctl
		// runs on disagree about it -- "no reply" is 1 on Linux and 2 on the
		// BSDs, where 1 means a hard error -- so a table keyed on GOOS would
		// have to be right about each of them or it would turn ordinary
		// powered-off hosts into "unknown", i.e. into a rack that can never be
		// declared idle. The gap left is a ping that runs but fails for its own
		// reasons (a raw socket it may not open, say) still reading as down;
		// that is no worse than before this distinction existed, and unlike a
		// missing binary it has not actually happened here.
		return false, true

	default:
		// The binary could not be started at all: not found, not executable,
		// fork failed. Nothing was measured.
		return false, false
	}
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
