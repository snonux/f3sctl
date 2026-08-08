package agent

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// vmShutdownTimeout bounds how long we wait for the bhyve guests to power
// themselves off before resorting to SIGKILL.
//
// This must stay BELOW the host's rcshutdown_timeout, currently 300s (raised
// from the 90s default on 2026-06-28 across f0/f1/f2). The reason is specific:
// if rc.shutdown overruns that watchdog, init logs "terminated abnormally,
// going to single user mode" and the host drops to single-user *still powered
// on, with no network*. Wake-on-LAN only wakes a powered-off NIC, so the host
// then cannot be woken remotely at all and needs physical access. Changing one
// of these two numbers without the other reopens exactly that trap.
//
// See the f3s skill, references/console-jetkvm-shutdown.md.
const vmShutdownTimeout = 240 * time.Second

// guestPollInterval is how often we re-check whether the guests have gone.
const guestPollInterval = 5 * time.Second

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

	pids, err := bhyvePIDs()
	if err != nil {
		return err
	}

	if len(pids) > 0 {
		// Two signals with a pause between, mirroring vm-bhyve: the first
		// initiates ACPI shutdown in the guest, the second covers a guest
		// that was not ready for the first.
		signalAll(pids, syscall.SIGTERM)
		time.Sleep(time.Second)
		signalAll(pids, syscall.SIGTERM)
	}

	if err := waitForGuests(); err != nil {
		return err
	}

	fmt.Println("guests stopped; powering off")
	return exec.Command("poweroff").Run()
}

// waitForGuests polls until no bhyve process remains, SIGKILLing whatever is
// left once the timeout expires.
//
// The forced kill is a last resort and says so loudly: SIGKILL can leave an
// etcd write-ahead log torn, which may need a health check or recovery on the
// next boot. It is still the better outcome than the host hanging and becoming
// un-wakeable.
func waitForGuests() error {
	deadline := time.Now().Add(vmShutdownTimeout)

	for {
		pids, err := bhyvePIDs()
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}

		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr,
				"WARNING: guests still running after %s; force-killing bhyve PIDs %v. "+
					"Check etcd health on the next boot.\n", vmShutdownTimeout, pids)
			signalAll(pids, syscall.SIGKILL)
			time.Sleep(2 * time.Second)

			remaining, err := bhyvePIDs()
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

		time.Sleep(guestPollInterval)
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
