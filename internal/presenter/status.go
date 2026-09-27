// Package presenter renders power-probe results the same way for every
// f3sctl surface.
//
// Both the local CLI (`f3sctl power status`) and the --remote HTTP client
// (`f3sctl --remote power status` / `f3sctl --remote fans status`) display the
// same underlying data -- a []status.HostStatus plus a status.FansState -- and
// until this package existed each maintained its own copy of the table
// renderer. The two had already diverged: the local table grew a ROLE column
// the remote one never got, and the remote client's own "unmeasured vs off"
// labelling had silently fallen out of sync with the local one (see hz0).
// Rendering both from one place is what stops that happening again.
package presenter

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/snonux/f3sctl/internal/status"
)

// Options controls the parts of the table that are not implied by the probe
// data itself.
type Options struct {
	// ShowRole prints a ROLE column sourced from HostStatus.Role.
	//
	// The local CLI always sets this: power.Engine.ProbeAll populates Role for
	// every host it probes, straight out of the inventory. The remote client
	// leaves it off by choice: powerapi's hostEntity does carry the role as
	// the second entry of a host entity's "class" (["host", role]), which
	// docs/CLIENT.md now lists as stable, but the remote status table has
	// never shown a ROLE column. Turning it on is a presentation change, not
	// a contract one.
	ShowRole bool
}

// Status renders the probe table -- one row per host, in the order given --
// followed by the rack-fan and f-host AC lines, onto out.
//
// fansErr / acErr, when non-nil, mean that plug's state could not be read at
// all (network or auth failure): that is reported as "unknown", never as
// "off", because an unreachable plug is not evidence of anything -- see
// printFans / printAC.
func Status(out io.Writer, statuses []status.HostStatus, opts Options,
	fans status.FansState, fansErr error, ac status.ACState, acErr error) error {
	if err := printHosts(out, statuses, opts); err != nil {
		return err
	}
	printFans(out, fans, fansErr)
	printAC(out, ac, acErr)
	return nil
}

// printHosts renders the aligned host table.
func printHosts(out io.Writer, statuses []status.HostStatus, opts Options) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if opts.ShowRole {
		fmt.Fprintln(w, "HOST\tROLE\tADDRESS\tPING\tSSH\tRTT\tSTATE")
	} else {
		fmt.Fprintln(w, "HOST\tADDRESS\tPING\tSSH\tRTT\tSTATE")
	}

	for _, st := range statuses {
		rtt := "-"
		if st.Ping {
			rtt = fmt.Sprintf("%.1fms", st.MS)
		}
		if opts.ShowRole {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				st.Name, st.Role, st.IP, YesNo(st.Ping), YesNo(st.SSH), rtt, Describe(st))
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				st.Name, st.IP, YesNo(st.Ping), YesNo(st.SSH), rtt, Describe(st))
		}
	}
	return w.Flush()
}

// printFans renders the rack-fan line: state and IP normally, or "unknown"
// plus the reason when the plug could not be read at all.
//
// The fan state is part of "is the rack healthy", so it belongs in the same
// glance as the host table rather than a separate command.
func printFans(out io.Writer, fans status.FansState, err error) {
	if err != nil {
		fmt.Fprintf(out, "\nrack fans: unknown (%v)\n", err)
		return
	}
	fmt.Fprintf(out, "\nrack fans: %s (%s)\n", OnOff(fans.On), fans.IP)
}

// printAC renders the f-host mains AC line, same unknown-not-off rule as
// printFans. shelly2 is independent of power on/off; operators still need to
// see its state in the same glance as the host table.
//
// An empty IP with no error means the status document never carried an ac
// entity (older server, partial /status): that is unknown, never "off" — a
// false "AC off" reading for a mains plug is actively dangerous.
func printAC(out io.Writer, ac status.ACState, err error) {
	if err != nil {
		fmt.Fprintf(out, "f-host AC: unknown (%v)\n", err)
		return
	}
	if ac.IP == "" {
		fmt.Fprint(out, "f-host AC: unknown (not reported)\n")
		return
	}
	fmt.Fprintf(out, "f-host AC: %s (%s)\n", OnOff(ac.On), ac.IP)
}

// Describe turns a host's probe signals into the state they imply.
//
// The middle case is deliberately not called "booting": answering ICMP with
// no sshd means the host is in transition, and a single observation cannot
// tell a host coming up from one going down.
//
// PingKnown is checked explicitly before falling back to "off" -- this is
// hz0's fix. A host whose ping probe never ran (no ping(8), a context cut
// short, ...) must be reported as "unknown", never "off": docs/CLIENT.md's
// contract, and what the local CLI's describe() already did correctly before
// this package existed. The remote client's separate describe(ping, ssh) took
// only the two booleans and never saw PingKnown at all, so an unmeasured host
// rendered as "off (or hung in single-user)" there -- exactly the confusing
// state CLIENT.md's pingKnown paragraph exists to prevent, since the server
// treats an unmeasured host as possibly still running (keeping the rack fans
// on) while the table told the operator otherwise.
func Describe(st status.HostStatus) string {
	switch {
	case st.Ping && st.SSH:
		return "up"
	case st.Ping && !st.SSH:
		return "in transition"
	case !st.PingKnown:
		// The PING column reads "no" here too, and would be read as "off" --
		// but nothing was measured. Saying so also explains why `fans off`
		// then refuses on a rack this table appears to show as cold.
		return "unknown (the ping probe could not run)"
	default:
		// Also the signature of a host hung in single-user after a failed
		// shutdown: powered on, no network, and not wakeable by WoL. There is
		// no way to tell the two apart from here, so say so rather than
		// asserting "off".
		return "off (or hung in single-user)"
	}
}

// YesNo renders a boolean probe signal (PING, SSH) as a table cell.
func YesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// OnOff renders a boolean switch state (the rack fans) as a table cell.
func OnOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
