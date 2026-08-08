package power

import (
	"context"
	"net"
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

	go func() {
		defer wg.Done()
		start := time.Now()
		st.Ping = e.pingOnce(ctx, h.IP)
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

// pingOnce sends a single ICMP echo and reports whether it was answered.
//
// This shells out to ping(8) instead of building an ICMP socket in Go. An
// unprivileged ICMP datagram socket exists on Linux but not on NetBSD, so a
// native implementation would need a raw socket and thus root — which the CGI
// (running as _httpd under bozohttpd) does not have and should not get.
// NetBSD's /sbin/ping is setuid root, so shelling out gives real ICMP with no
// privilege grant at all. Verified working as _httpd on pi0.
func (e *Engine) pingOnce(ctx context.Context, ip string) bool {
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

	return exec.CommandContext(ctx, "ping", args...).Run() == nil
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
