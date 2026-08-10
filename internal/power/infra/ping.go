package infra

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// pingCandidates are where ping(8) lives, most likely first.
//
// It is looked up by absolute path rather than through PATH because f3sctl's
// HTTP API CGI runs with the environment bozohttpd hands it -- on NetBSD that
// is /usr/bin:/bin:/usr/pkg/bin:/usr/local/bin, which does NOT include /sbin
// where ping actually is. Relying on PATH made every host report ping=false
// while plainly answering on port 22, which in turn withheld the power-off
// action entirely (internal/power.Engine.off).
//
// That incident is also why a missing ping(8) reports unknown rather than
// down: it is a whole-fleet false negative, in the environment `power off`
// actually runs in, and read as "every host is silent" it says the rack is
// cold and its cooling can go.
var pingCandidates = []string{"/sbin/ping", "/usr/sbin/ping", "/bin/ping", "/usr/bin/ping"}

// PingPath resolves ping(8) once per process, falling back to PATH lookup as
// a last resort for a platform that keeps it somewhere else entirely.
//
// Exported because internal/power's isUp default (Engine.pingOnce) needs the
// real location; tests drive Ping directly with an explicit stub path
// instead of going through this.
var PingPath = sync.OnceValue(func() string {
	for _, p := range pingCandidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("ping"); err == nil {
		return p
	}
	return ""
})

// Ping sends a single ICMP echo to ip via bin and reports whether it was
// answered, and -- separately -- whether the probe reached a conclusion at
// all.
//
// This shells out to ping(8) instead of building an ICMP socket in Go. An
// unprivileged ICMP datagram socket exists on Linux but not on NetBSD, so a
// native implementation would need a raw socket and thus root -- which the
// f3sctl HTTP API's CGI (running as _httpd under bozohttpd) does not have and
// should not get. NetBSD's /sbin/ping is setuid root, so shelling out gives
// real ICMP with no privilege grant at all. Verified working as _httpd on
// pi0.
//
// timeout is the per-packet deadline handed to ping(8) itself: the flag that
// spells it differs per platform and there is no portable spelling -- -w on
// NetBSD and Linux, -t on FreeBSD/OpenBSD/macOS. A second, slightly longer
// context deadline is a backstop in case ping ignores its own flag; without
// it a wedged ping would hold the whole probe open.
//
// The tri-state result is the whole point of this function, and it must not
// be weakened: (false, false) has to mean "nothing was learned", never "the
// host is down", or a probe that could not run reads as a rack that is
// safely cold. See probeRan for how a hard error is told apart from a
// confirmed silence.
func Ping(ctx context.Context, bin, ip string, timeout time.Duration) (up, known bool) {
	if bin == "" {
		return false, false
	}

	args := pingArgs(runtime.GOOS, ip, timeout)

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

// pingArgs builds the ping(8) argument list for goos: one echo, with a
// per-packet deadline of timeout spelled with whichever flag that OS's
// ping(8) wants -- -w on NetBSD and Linux, -t on FreeBSD/OpenBSD/macOS, with
// no portable spelling between them.
//
// Split out from Ping purely so every branch can be pinned by a single test
// binary, by passing goos in rather than reading runtime.GOOS -- this
// package cannot otherwise be exercised for an OS other than the one running
// `go test`.
func pingArgs(goos, ip string, timeout time.Duration) []string {
	secs := strconv.Itoa(int(timeout.Seconds()))
	switch goos {
	case "freebsd", "openbsd", "darwin":
		return []string{"-c", "1", "-t", secs, ip}
	default: // netbsd, linux
		return []string{"-c", "1", "-w", secs, ip}
	}
}

// probeRan reports whether ping's output contains the summary it prints once
// it has actually sent packets and counted the replies.
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
// Verified on a Linux box: a genuine "no reply" exits 1 having printed
// "1 packets transmitted, 0 received", while `ping no.such.host.invalid` exits 2
// printing "Name or service not known" and no statistics at all. Every ping(8)
// on Linux, NetBSD, FreeBSD, OpenBSD and macOS prints this line when it got as
// far as transmitting, and none print it when it did not, so matching on it
// needs no per-platform knowledge.
func probeRan(out []byte) bool {
	return bytes.Contains(out, []byte("packets transmitted"))
}
