package power

import (
	"context"
	"fmt"
	"io"

	"github.com/snonux/f3sctl/internal/inventory"
)

// zusbPreflight exports the removable zusb backup pool from whichever host
// currently has it imported, before any host is powered off.
//
// zusb is a 4-disk raidz2 stack on USB-SATA bridges, plugged in roughly once a
// quarter for backups and imported by hand with zusb-load. Powering the host
// off with it still imported skips the snapshot, the clean export, and the
// SCSI STOP UNIT that parks the heads before USB power is cut.
//
// The pool lives on exactly one host at a time (f1 today), so this is a no-op
// almost every time it runs. That is the point: it costs one cheap SSH probe
// per host and removes the need to remember.
//
// A failure aborts the entire shutdown. This runs before the Gogios mute and
// before any poweroff, so at that moment nothing has been done yet and the
// cluster is still fully up — the cheapest possible place to stop.
func (e *Engine) zusbPreflight(ctx context.Context, log io.Writer, hosts []inventory.Host) error {
	for _, h := range hosts {
		state, err := e.ssh.agentVerb(ctx, h, "zusb-status")
		if err != nil {
			return fmt.Errorf("could not check the zusb pool on %s: %w", h.Name, err)
		}

		if state != "loaded" {
			continue
		}

		fmt.Fprintf(log, "  zusb is imported on %s; exporting it before shutdown...\n", h.Name)
		out, err := e.ssh.agentVerb(ctx, h, "zusb-unload")
		if out != "" {
			fmt.Fprintf(log, "  %s\n", indent(out))
		}
		if err != nil {
			return fmt.Errorf("exporting zusb on %s failed, aborting shutdown "+
				"so the backup pool is not cut off mid-write: %w", h.Name, err)
		}
		fmt.Fprintf(log, "  zusb exported and its disks powered down on %s\n", h.Name)
	}
	return nil
}
