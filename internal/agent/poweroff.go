package agent

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/snonux/f3sctl/internal/config"
)

// guestPollInterval is the default value of pollInterval, how often we
// re-check whether the guests have gone.
const guestPollInterval = 5 * time.Second

// pollInterval, killSettleWait, pidLookup and sendSignal are read through
// vars rather than used directly, so a test can shrink the waits and fake the
// PID/signal seams instead of depending on real bhyve processes and waiting
// out real timeouts -- the same pattern internal/power uses for
// downProbeInterval/powerDownTimeout.
var (
	pollInterval   = guestPollInterval
	killSettleWait = 2 * time.Second
	pidLookup      = bhyvePIDs
	sendSignal     = signalAll
)

// vmShutdownTimeout returns how long waitForGuests should wait for the bhyve
// guests to power themselves off before resorting to SIGKILL.
//
// It reads cfg.VMShutdownTimeout from the host's own local config instead of
// a hardcoded constant, so that an operator who lowers vm_shutdown_timeout
// actually gets a shorter real wait here -- see vz0. Before this, the config
// value was read only by internal/power/awaitdown.go's ShutdownWorstCase to
// compute the server's staleness ceiling, while the agent silently kept
// waiting its own hardcoded 240s regardless; lowering the config then shrank
// the ceiling without shrinking the real worst case, which could make a
// perfectly healthy shutdown look stale. A missing config file, a load
// error, or an unset/zero value all fall back to config.Default()'s 240s --
// a zero-duration wait would give up and SIGKILL every guest on the very
// first poll, which is never what an operator setting up a fresh config
// intends.
//
// Whatever this returns must stay BELOW the host's rcshutdown_timeout,
// currently 300s (raised from the 90s default on 2026-06-28 across
// f0/f1/f2). The reason is specific: if rc.shutdown overruns that watchdog,
// init logs "terminated abnormally, going to single user mode" and the host
// drops to single-user *still powered on, with no network*. Wake-on-LAN only
// wakes a powered-off NIC, so the host then cannot be woken remotely at all
// and needs physical access. Now that vm_shutdown_timeout is actually
// honored, an operator raising it above 300s reopens that trap just as
// surely as changing rcshutdown_timeout would.
//
// See the f3s skill, references/console-jetkvm-shutdown.md.
func vmShutdownTimeout() time.Duration {
	def := config.Default().VMShutdownTimeout.D()

	cfg, err := config.Load(os.Getenv("F3SCTL_CONFIG"))
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"WARNING: loading config for vm_shutdown_timeout failed (%v); using the %s default\n", err, def)
		return def
	}
	if d := cfg.VMShutdownTimeout.D(); d > 0 {
		return d
	}
	return def
}

// runPoweroff stops every bhyve guest, then powers the host off.
//
// It deliberately does not use `vm stopall`, and does not simply call
// shutdown(8) and let rc.shutdown handle it. vm-bhyve 1.7.3's rc script stops
// guests with `vm stopall -f`, which sends ACPI signals and then waits
// *indefinitely* via wait_for_pids -- that version has no per-guest
// stop_timeout. A k3s guest can take 45-90s to tear down containerd, which is
// what used to overrun the shutdown watchdog and strand the host in
// single-user mode.
//
// So the same two SIGTERMs vm-bhyve would send are sent here, and the wait is
// bounded by us instead.
func runPoweroff() error {
	if out, err := exec.Command("vm", "list").CombinedOutput(); err == nil {
		fmt.Printf("guests before shutdown:\n%s", out)
	}

	pids, err := pidLookup()
	if err != nil {
		return err
	}

	if len(pids) > 0 {
		// Two signals with a pause between, mirroring vm-bhyve: the first
		// initiates ACPI shutdown in the guest, the second covers a guest
		// that was not ready for the first.
		sendSignal(pids, syscall.SIGTERM)
		time.Sleep(time.Second)
		sendSignal(pids, syscall.SIGTERM)
	}

	if err := waitForGuests(vmShutdownTimeout()); err != nil {
		return err
	}

	fmt.Println("guests stopped; powering off")
	return exec.Command("poweroff").Run()
}

// waitForGuests polls until no bhyve process remains, SIGKILLing whatever is
// left once timeout expires.
//
// timeout comes from vmShutdownTimeout (cfg.VMShutdownTimeout, in
// production) rather than being read here directly, so a test can drive this
// function with a short duration instead of the real config.
//
// The forced kill is a last resort and says so loudly: SIGKILL can leave an
// etcd write-ahead log torn, which may need a health check or recovery on the
// next boot. It is still the better outcome than the host hanging and becoming
// un-wakeable.
func waitForGuests(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		pids, err := pidLookup()
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}

		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr,
				"WARNING: guests still running after %s; force-killing bhyve PIDs %v. "+
					"Check etcd health on the next boot.\n", timeout, pids)
			sendSignal(pids, syscall.SIGKILL)
			time.Sleep(killSettleWait)

			remaining, err := pidLookup()
			if err != nil {
				return err
			}
			if len(remaining) > 0 {
				// Powering off with a guest still live risks the very
				// corruption the ordered shutdown exists to avoid.
				return fmt.Errorf("bhyve PIDs %v survived SIGKILL; refusing to power off", remaining)
			}
			return nil
		}

		time.Sleep(pollInterval)
	}
}

// bhyvePIDs returns the PIDs of the running bhyve guests.
//
// pgrep exits 0 when it matched, 1 when it matched nothing, and >1 on a real
// error. That last case must not be read as "no guests are running": doing so
// would power the host off underneath a live VM. So anything other than 0 or 1
// is an error here.
func bhyvePIDs() ([]int, error) {
	cmd := exec.Command("pgrep", "-f", "bhyve:")
	out, err := cmd.Output()

	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot inspect bhyve processes, refusing to power off: %w", err)
	}

	var pids []int
	for _, field := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("unexpected pgrep output %q: %w", trimmed(out), err)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// signalAll sends sig to every PID, ignoring processes that have already gone.
func signalAll(pids []int, sig syscall.Signal) {
	for _, pid := range pids {
		p, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		_ = p.Signal(sig)
	}
}
