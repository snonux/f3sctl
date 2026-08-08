// Package power implements everything f3sctl actually does to the homelab:
// waking hosts, shutting them down safely, probing their state, and switching
// the rack fans.
//
// It is the single implementation behind both the CLI and the HTTP API, so the
// two cannot disagree about what "power off" means.
package power

import (
	"context"
	"fmt"
	"io"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/inventory"
)

// Engine performs power operations against the inventory in cfg.
type Engine struct {
	cfg config.Config
	ssh *runner
}

// New returns an Engine.
//
// It cannot fail: the SSH identity is resolved lazily, on the first operation
// that actually needs it, so status probing works on a machine that has no
// f3sctl key at all.
func New(cfg config.Config) (*Engine, error) {
	return &Engine{cfg: cfg, ssh: newRunner(cfg)}, nil
}

// Config exposes the resolved configuration to callers that need the
// inventory (the CLI's status table, the API's registry).
func (e *Engine) Config() config.Config { return e.cfg }

// On wakes the k3s bhyve hosts f0/f1/f2.
//
// Order matters: the fans go on before the hosts, not after, so the rack is
// never under load without cooling. If the plug cannot be switched, nothing is
// woken at all — running the cluster with no fans is worse than leaving it
// off.
func (e *Engine) On(ctx context.Context, log io.Writer) error {
	fmt.Fprintln(log, "Switching the rack fans on...")
	if _, err := e.FansSet(ctx, true); err != nil {
		return fmt.Errorf("refusing to wake hosts with the fans off: %w", err)
	}

	for _, h := range e.cfg.Inventory.PowerGroup() {
		fmt.Fprintf(log, "Sending a magic packet to %s (%s)...\n", h.Name, h.MAC)
		if err := e.Wake(h); err != nil {
			return err
		}
	}

	// Un-muting is best-effort: the hosts are already waking, and a monitoring
	// marker left behind is a smaller problem than reporting the whole wake as
	// failed. UnmuteGogios prints how to clear it by hand if it gives up.
	if err := e.UnmuteGogios(ctx, log); err != nil {
		fmt.Fprintf(log, "  (continuing anyway; the hosts are waking)\n")
	}

	fmt.Fprintln(log, "Magic packets sent. The hosts should be reachable in a minute or so.")
	return nil
}

// Off shuts down the k3s bhyve hosts f0/f1/f2 and switches the rack fans off.
//
// The sequence is ordered so that everything which can refuse does so before
// anything irreversible happens:
//
//  1. unmount local NFS      -- abort while the cluster is still fully up
//  2. export zusb where held -- ditto; needs the host it lives on to be alive
//  3. mute Gogios            -- only now, once the shutdown is going ahead
//  4. stop guests, power off -- host by host
//  5. fans off               -- only if every host accepted
func (e *Engine) Off(ctx context.Context, log io.Writer) error {
	return e.off(ctx, log, e.cfg.Inventory.PowerGroup(), true)
}

// OnHost wakes a single named host, without touching the fans or the Gogios
// marker. Used for f3, which is not part of the cluster.
func (e *Engine) OnHost(ctx context.Context, log io.Writer, name string) error {
	h, err := e.powerHost(name)
	if err != nil {
		return err
	}
	fmt.Fprintf(log, "Sending a magic packet to %s (%s)...\n", h.Name, h.MAC)
	return e.Wake(h)
}

// OffHost shuts down a single named host.
//
// The fans and the Gogios marker are deliberately left alone: f3 going down
// does not mean the rack is idle, and the muted checks are the cluster's, not
// f3's.
func (e *Engine) OffHost(ctx context.Context, log io.Writer, name string) error {
	h, err := e.powerHost(name)
	if err != nil {
		return err
	}
	return e.off(ctx, log, []inventory.Host{h}, false)
}

func (e *Engine) off(ctx context.Context, log io.Writer, hosts []inventory.Host, clusterWide bool) error {
	fmt.Fprintln(log, "Checking for locally mounted NFS filesystems...")
	if err := e.checkLocalNFS(ctx, log); err != nil {
		return err
	}

	fmt.Fprintln(log, "Checking whether the zusb backup pool is imported anywhere...")
	if err := e.zusbPreflight(ctx, log, hosts); err != nil {
		return err
	}

	if clusterWide {
		fmt.Fprintln(log, "Muting Gogios monitoring...")
		if err := e.MuteGogios(ctx, log); err != nil {
			// Not fatal: an un-muted alert is noise, and refusing to shut
			// down over noise would be worse. But say so loudly.
			fmt.Fprintf(log, "  ! %v\n", err)
			fmt.Fprintln(log, "  Continuing; expect alerts while the cluster is down.")
		}
	}

	var failed []string
	for _, h := range hosts {
		fmt.Fprintf(log, "Shutting down %s (%s)...\n", h.Name, h.IP)
		out, err := e.ssh.agentVerb(ctx, h, "poweroff")
		if out != "" {
			fmt.Fprintf(log, "  %s\n", indent(out))
		}
		if err != nil {
			fmt.Fprintf(log, "  ! %v\n", err)
			failed = append(failed, h.Name)
			continue
		}
		fmt.Fprintf(log, "  %s is powering off\n", h.Name)
	}

	if len(failed) > 0 {
		return fmt.Errorf("these hosts did not complete shutdown: %v. "+
			"Leaving the rack fans on", failed)
	}

	if clusterWide {
		fmt.Fprintln(log, "Switching the rack fans off...")
		if _, err := e.FansSet(ctx, false); err != nil {
			return err
		}
	}

	fmt.Fprintln(log, "All hosts accepted shutdown.")
	return nil
}

// powerHost looks up a host and confirms f3sctl is allowed to power it.
//
// Only the f-hosts qualify. The Pis run this tool, so powering them off would
// remove the only way to power anything back on; the r-VMs follow their bhyve
// host rather than being addressed directly.
func (e *Engine) powerHost(name string) (inventory.Host, error) {
	h, ok := e.cfg.Inventory.ByName(name)
	if !ok {
		return h, fmt.Errorf("unknown host %q", name)
	}
	if h.Role != inventory.RoleF {
		return h, fmt.Errorf("%s is not a host f3sctl can power (only f0-f3 are)", name)
	}
	return h, nil
}

// indent prefixes continuation lines of remote output so it reads as nested
// under the host it came from.
func indent(s string) string {
	out := ""
	for i, line := range splitLines(s) {
		if i > 0 {
			out += "\n  "
		}
		out += line
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			lines = append(lines, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}
