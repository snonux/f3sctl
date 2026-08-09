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
	cfg    config.Config
	ssh    *runner
	report Reporter
}

// New returns an Engine.
//
// It cannot fail: the SSH identity is resolved lazily, on the first operation
// that actually needs it, so status probing works on a machine that has no
// f3sctl key at all.
func New(cfg config.Config) (*Engine, error) {
	return &Engine{cfg: cfg, ssh: newRunner(cfg), report: nopReporter{}}, nil
}

// Config exposes the resolved configuration to callers that need the
// inventory (the CLI's status table, the API's registry).
func (e *Engine) Config() config.Config { return e.cfg }

// logWarnings routes diagnostics from successful agent verbs into log.
//
// Called at the start of each operation that has somewhere to write, because
// the Engine holds no log of its own. Without it these messages are dropped:
// an agent that force-kills a bhyve guest warns on stderr and still exits 0,
// so the warning arrives on the success path or not at all.
func (e *Engine) logWarnings(log io.Writer) {
	e.ssh.warn = func(host, verb, msg string) {
		fmt.Fprintf(log, "  ! %s (%s): %s\n", host, verb, indent(msg))
	}
}

// On wakes the k3s bhyve hosts f0/f1/f2.
//
// Order matters: the fans go on before the hosts, not after, so the rack is
// never under load without cooling. If the plug cannot be switched, nothing is
// woken at all — running the cluster with no fans is worse than leaving it
// off.
func (e *Engine) On(ctx context.Context, log io.Writer) error {
	return e.on(ctx, log, e.cfg.Inventory.PowerGroup())
}

// on is the shared wake sequence: fans first, then magic packets, then wait for
// the cluster and clear the Gogios mute.
func (e *Engine) on(ctx context.Context, log io.Writer, hosts []inventory.Host) error {
	e.logWarnings(log)

	e.report.Step("switching the rack fans on")
	fmt.Fprintln(log, "Switching the rack fans on...")
	if _, err := e.FansSet(ctx, true); err != nil {
		return fmt.Errorf("refusing to wake hosts with the fans off: %w", err)
	}

	e.report.Step("sending Wake-on-LAN packets")
	for _, h := range hosts {
		fmt.Fprintf(log, "Sending a magic packet to %s (%s)...\n", h.Name, h.MAC)
		if err := e.Wake(h); err != nil {
			e.report.HostState(h.Name, HostFailed, err.Error())
			return err
		}
		e.report.HostState(h.Name, HostDone, "magic packet sent")
	}

	e.report.Step("waiting for the k3s nodes, then un-muting Gogios")

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
//  4. stop guests, power off -- host by host, storage master LAST
//  5. fans off               -- only if every host actually went down
//
// The host order is not incidental: taking the CARP storage master first fails
// the VIP over onto a host that is itself about to be shut down, which is what
// wedged f1 on 2026-08-08. See inventory.ShutdownOrder.
func (e *Engine) Off(ctx context.Context, log io.Writer) error {
	return e.off(ctx, log, e.cfg.Inventory.ShutdownOrder(), true)
}

// OffAll shuts down every f-host, f3 included, and switches the rack fans off.
//
// Identical to Off apart from the host set: same NFS and zusb pre-flight, same
// Gogios mute, same storage-master-last ordering, same fans-off only once every
// host has actually gone silent. This is "the whole rack goes dark", which
// previously meant running `power off` and `power f3 off` and remembering that
// only the first of them touches the fans.
func (e *Engine) OffAll(ctx context.Context, log io.Writer) error {
	return e.off(ctx, log, e.cfg.Inventory.ShutdownOrderAll(), true)
}

// OnAll wakes every f-host, f3 included.
func (e *Engine) OnAll(ctx context.Context, log io.Writer) error {
	return e.on(ctx, log, e.cfg.Inventory.EveryFHost())
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
	e.logWarnings(log)

	for _, h := range hosts {
		e.report.HostState(h.Name, HostPending, "")
	}

	// Drop hosts that are already powered off.
	//
	// Everything below this point speaks SSH -- the zusb pre-flight, the guest
	// stop, the poweroff itself -- and a powered-off host cannot answer any of
	// it. Treating that as an error is wrong twice over: it is not a failure,
	// and it aborts work that still needs doing on the hosts that *are* up.
	//
	// This is not hypothetical. On 2026-08-09 a `power off` run with f0
	// already down failed at the zusb pre-flight with "connect to host
	// 192.168.1.130 port 22: Operation timed out" and left f1 and f2 running.
	// Shutting the rack down in stages, or re-running after a partial run, is
	// ordinary use.
	//
	// Ping decides this, not SSH. A host answering ICMP but not SSH is powered
	// on and unreachable, and that case must still abort: there is no way to
	// tell whether it has the zusb pool imported, and guessing risks cutting
	// USB power to a mounted backup pool.
	live, alreadyOff := partitionLive(hosts, func(ip string) bool {
		return e.pingOnce(ctx, ip)
	})
	for _, h := range alreadyOff {
		fmt.Fprintf(log, "%s is already powered off; skipping it\n", h.Name)
		e.report.HostState(h.Name, HostDone, "already powered off")
	}
	hosts = live

	e.report.Step("checking for locally mounted NFS filesystems")
	fmt.Fprintln(log, "Checking for locally mounted NFS filesystems...")
	if err := e.checkLocalNFS(ctx, log); err != nil {
		return err
	}

	e.report.Step("checking the zusb backup pool")
	fmt.Fprintln(log, "Checking whether the zusb backup pool is imported anywhere...")
	if err := e.zusbPreflight(ctx, log, hosts); err != nil {
		return err
	}

	if clusterWide {
		e.report.Step("muting Gogios monitoring")
		fmt.Fprintln(log, "Muting Gogios monitoring...")
		if err := e.MuteGogios(ctx, log); err != nil {
			// Not fatal: an un-muted alert is noise, and refusing to shut
			// down over noise would be worse. But say so loudly.
			fmt.Fprintf(log, "  ! %v\n", err)
			fmt.Fprintln(log, "  Continuing; expect alerts while the cluster is down.")
		}
	}

	var failed []string
	var accepted []inventory.Host
	for _, h := range hosts {
		e.report.Step("shutting down " + h.Name)
		e.report.HostState(h.Name, HostWorking, "stopping guests")
		fmt.Fprintf(log, "Shutting down %s (%s)...\n", h.Name, h.IP)
		out, diag, err := e.ssh.agentVerbFull(ctx, h, "poweroff")
		if out != "" {
			fmt.Fprintf(log, "  %s\n", indent(out))
		}
		if err != nil {
			fmt.Fprintf(log, "  ! %v\n", err)
			e.report.HostState(h.Name, HostFailed, err.Error())
			failed = append(failed, h.Name)
			continue
		}

		// A forced guest stop still exits 0, so it arrives here rather than in
		// the error branch. Carry it into the host's progress detail: a run
		// that SIGKILLed a k3s guest may have torn an etcd write-ahead log,
		// and that has to be visible to whoever reads the job, not buried in a
		// log file on whichever node happened to run it.
		detail := "accepted; waiting for it to go silent"
		if diag != "" {
			detail = "accepted, but the guests were force-stopped; check etcd on next boot"
		}
		fmt.Fprintf(log, "  %s accepted the shutdown\n", h.Name)
		e.report.HostState(h.Name, HostConfirming, detail)
		accepted = append(accepted, h)
	}

	// Accepting the command is not the same as completing it. A host can run
	// the whole shutdown sequence and then wedge in the final phase -- after
	// syslogd has exited, so nothing is logged -- leaving it powered on, off
	// the network, and NOT wakeable by Wake-on-LAN, which only wakes a NIC
	// that actually powered down. Recovering from that needs a console or the
	// physical button.
	//
	// f1 did exactly this on 2026-08-08 while f0 and f2 powered off cleanly.
	// Nothing reported a problem at the time: the tool said "shutdown sent"
	// and moved on, and the failure only surfaced later when the host would
	// not wake. Confirming each host actually goes silent turns that into an
	// error at the moment it happens.
	e.report.Step("confirming the hosts actually powered down")
	if stuck := e.awaitPowerDown(ctx, log, accepted); len(stuck) > 0 {
		failed = append(failed, stuck...)
	}

	if len(failed) > 0 {
		return fmt.Errorf("these hosts did not complete shutdown: %v. "+
			"Leaving the rack fans on", failed)
	}

	if clusterWide {
		e.report.Step("switching the rack fans off")
		fmt.Fprintln(log, "Switching the rack fans off...")
		if _, err := e.FansSet(ctx, false); err != nil {
			return err
		}
	}

	fmt.Fprintln(log, "All hosts accepted shutdown.")
	return nil
}

// partitionLive splits hosts into those answering ICMP and those already off.
//
// Pulled out as a plain function so the rule can be tested without an SSH
// client, a Shelly plug or a live rack: isUp is the only thing it touches.
func partitionLive(hosts []inventory.Host, isUp func(ip string) bool) (live, off []inventory.Host) {
	for _, h := range hosts {
		if isUp(h.IP) {
			live = append(live, h)
			continue
		}
		off = append(off, h)
	}
	return live, off
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
