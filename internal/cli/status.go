package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/snonux/f3sctl/internal/power"
)

// printStatus renders the probe results as an aligned table.
//
// Both signals are shown rather than a single "up" column, because the
// combination is what tells you which state a host is in -- see
// power.HostStatus.
func printStatus(ctx context.Context, eng *power.Engine, out io.Writer) error {
	statuses := eng.ProbeAll(ctx)

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HOST\tROLE\tADDRESS\tPING\tSSH\tRTT\tSTATE")

	for _, st := range statuses {
		rtt := "-"
		if st.Ping {
			rtt = fmt.Sprintf("%.1fms", st.MS)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			st.Name, st.Role, st.IP, yesNo(st.Ping), yesNo(st.SSH), rtt, describe(st))
	}

	if err := w.Flush(); err != nil {
		return err
	}

	// The fan state is part of "is the rack healthy", so it belongs in the
	// same glance. A failure to read it is reported but is not fatal: the
	// host status above is still useful and is the more important half.
	if fans, err := eng.FansStatus(ctx); err != nil {
		fmt.Fprintf(out, "\nrack fans: unknown (%v)\n", err)
	} else {
		fmt.Fprintf(out, "\nrack fans: %s (%s)\n", onOff(fans.On), fans.IP)
	}
	return nil
}

// describe turns the two probe signals into the state they imply.
//
// The middle case is deliberately not called "booting": answering ICMP with no
// sshd means the host is in transition, and a single observation cannot tell a
// host coming up from one going down.
func describe(st power.HostStatus) string {
	switch {
	case st.Ping && st.SSH:
		return "up"
	case st.Ping && !st.SSH:
		return "in transition"
	default:
		// Also the signature of a host hung in single-user after a failed
		// shutdown: powered on, no network, and not wakeable by WoL. There is
		// no way to tell the two apart from here, so say so rather than
		// asserting "off".
		return "off (or hung in single-user)"
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
