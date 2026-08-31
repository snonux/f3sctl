// Package presenter renders power-probe results the same way for every
// f3sctl surface.
//
// Both the local CLI (`f3sctl power status`) and the --remote HTTP client
// (`f3sctl --remote power status` / `f3sctl --remote fans status`) display the
// same underlying data -- a []power.HostStatus plus a power.FansState -- and
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

	"github.com/snonux/f3sctl/internal/power"
)

// Options controls the parts of the table that are not implied by the probe
// data itself.
type Options struct {
	// ShowRole prints a ROLE column sourced from HostStatus.Role.
	//
	// The local CLI always sets this: power.Engine.ProbeAll populates Role for
	// every host it probes, straight out of the inventory. The remote client
	// leaves it off. It could, in principle, dig it out: powerapi's hostEntity
	// puts the role in a host entity's "class" array (["host", role]) --
	// but docs/CLIENT.md section 11 lists the stable, depend-on-able host
	// properties as name/ip/ping/pingKnown/ssh/ms only, and explicitly calls
	// out "new entity classes... appearing at any time" as NOT stable. Reading
	// class[1] positionally would be coupling to exactly the kind of thing
	// that contract says not to. So the remote client passes ShowRole: false
	// and the column is simply absent there, same as it always effectively
	// was.
	ShowRole bool
}

// Status renders the probe table -- one row per host, in the order given --
// followed by the rack-fan line, onto out.
//
// fansErr, when non-nil, means the fan plug's state could not be read at all
// (network or auth failure): that is reported as "unknown", never as "off",
// because an unreachable plug is not evidence of anything -- see printFans.
func Status(out io.Writer, statuses []power.HostStatus, opts Options, fans power.FansState, fansErr error) error {
	if err := printHosts(out, statuses, opts); err != nil {
		return err
	}
	printFans(out, fans, fansErr)
	return nil
}

// printHosts renders the aligned host table.
func printHosts(out io.Writer, statuses []power.HostStatus, opts Options) error {
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
func printFans(out io.Writer, fans power.FansState, err error) {
	if err != nil {
		fmt.Fprintf(out, "\nrack fans: unknown (%v)\n", err)
		return
	}
	fmt.Fprintf(out, "\nrack fans: %s (%s)\n", OnOff(fans.On), fans.IP)
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
func Describe(st power.HostStatus) string {
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
