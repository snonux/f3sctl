package agent

import (
	"fmt"
	"os"
	"os/exec"
)

// carpScript is the host-side CARP management script, present only on the
// CARP pair (f0/f1). Its absence is how this verb tells "not a CARP member"
// from "a CARP member whose failover daemons need stopping" -- see the
// f3s-storage skill, references/carp.md.
const carpScript = "/usr/local/bin/carp"

// runCommand runs a host command and returns its combined output.
//
// A var rather than a direct exec call for the same reason poweroff.go has
// pidLookup and sendSignal: this verb's whole job is to run service(8) and a
// local script, neither of which exists on the machine the tests run on.
var runCommand = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// fileExists is the seam for "is this host a CARP member", for the same
// reason as runCommand.
var fileExists = func(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// runCARPQuiesce stops everything on this host that can react to a CARP state
// change, so that a rack-wide shutdown can take several hosts down at once.
//
// Two things react to CARP here, and both are dangerous once a host is on its
// way down:
//
//   - devd runs /usr/local/bin/carpcontrol.sh on every MASTER/BACKUP
//     transition. On MASTER it mounts the NFS dataset (loading a ZFS key off
//     the USB /keys) and starts rpcbind/mountd/nfsd/nfsuserd/stunnel; on
//     BACKUP it stops them. A host that takes the VIP seconds before its own
//     poweroff goes down as a freshly-started NFS server with clients still
//     talking to it, and hangs in the last phase of shutdown -- powered on,
//     off the network, and NOT wakeable by Wake-on-LAN. That is exactly how
//     f1 was stranded on 2026-08-08.
//   - cron runs carp-auto-failback.sh on f0 once a minute, which promotes f0
//     back to MASTER whenever it looks healthy. During a shutdown "healthy"
//     is a racing certainty, not a fact.
//
// Stopping devd is the load-bearing half: with it stopped, a transition
// changes no services, whoever wins the election. Stopping cron is the cheap
// second half -- it stops the rack generating pointless transitions in the
// first place.
//
// cron is stopped rather than `carp auto-failback disable` used, even though
// that is the documented way to hold failback off for maintenance. That
// command works by creating /data/nfs/nfs.NO_AUTO_FAILBACK -- a file on the
// NFS dataset itself, which survives the reboot. A shutdown that disabled
// auto-failback would therefore leave it disabled for good, and on f1 (where
// /data/nfs is only mounted while it holds the VIP) it would write the marker
// into an unmounted mountpoint on the root filesystem, to surface later as a
// file that shadows the real one. Stopping the daemon has neither problem:
// the effect is exactly as long-lived as this shutdown, because both daemons
// start again at boot.
//
// Deliberately NOT stopped: nfsd, mountd, rpcbind, nfsuserd and stunnel
// themselves. They are what CARP failover *starts*, not what performs it, and
// the k3s guests on the other hosts are still writing to that NFS export
// while they shut down. Stopping the export first would hang the very guests
// this is trying to let power off in parallel.
//
// Both daemons stay stopped until the host reboots, which for the run that
// calls this is a few minutes away. A shutdown that is aborted after this
// point leaves them stopped on a running host: no service changes state on
// its own, but the pair no longer fails over and cron jobs no longer fire.
// The caller says so in its log, because a half-run shutdown is exactly when
// someone walks away thinking the rack is fine.
func runCARPQuiesce() error {
	if !fileExists(carpScript) {
		fmt.Println("no CARP configuration here; nothing to quiesce")
		return nil
	}

	stopFailbackCron()
	return stopService("devd",
		"a CARP transition can no longer start or stop NFS here")
}

// stopFailbackCron stops cron, and with it f0's minutely promotion back to
// MASTER.
//
// A failure is a warning, not an error: with devd stopped a promotion starts
// no services, so this is the belt to that braces. Saying so on stderr still
// matters -- the agent's stderr reaches the job log even on success (see
// runner.agentVerbFull) -- because it means the rack will keep electing a
// master while it goes down.
func stopFailbackCron() {
	if err := stopService("cron", "CARP auto-failback can no longer promote this host"); err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: %v\n", err)
	}
}

// stopService stops one rc service, reporting why it was stopped.
//
// A service that is already stopped is success, not failure: service(8) exits
// non-zero for it, and refusing to shut the rack down because the thing we
// wanted stopped is stopped would be absurd. Anything else is a real error --
// for devd the caller uses it to decide that hosts must go down one at a time
// after all.
func stopService(name, because string) error {
	if _, err := runCommand("service", name, "status"); err != nil {
		fmt.Printf("%s is not running; %s\n", name, because)
		return nil
	}

	if out, err := runCommand("service", name, "stop"); err != nil {
		return fmt.Errorf("stopping %s: %w: %s", name, err, trimmed(out))
	}
	fmt.Printf("%s stopped; %s\n", name, because)
	return nil
}
