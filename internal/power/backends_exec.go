package power

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power/infra"
)

// This file holds the adapters New wires the PowerBackend/ProbeBackend/
// FansBackend/NFSChecker/ZusbChecker fields to: thin structs that perform the
// real mechanism -- ssh(1), ping(8), umount(8), the Shelly HTTP RPC, a UDP
// magic packet. Each adapter delegates rather than reimplements; see
// backends.go for what each interface promises and why.
//
// The platform-specific parsing behind some of these mechanisms -- which
// ping(8) flags this OS wants, how to read the local NFS mount table -- has
// moved one layer further down, into internal/power/infra (see nfs.go and
// probe.go, which now delegate to it). As of the SRP refactor this file is
// also where the fan and probe mechanisms themselves are held rather than on
// the Engine: execFans owns an infra.ShellyClient and the settle read-back,
// and execProbe owns an infra.ProbeClient, so changing how the plug is
// reached or how a host is probed is an edit to the leaf struct (in infra) or
// this adapter, not to the Engine that carries the shutdown and fan-guard
// policy.

// newShellyClient builds the infra.ShellyClient the fans adapter reaches the
// plug through. A helper rather than an inline struct literal in New so the
// hand-built-Engine fallback (fansBackend below) can build the same client
// without duplicating the wiring.
func newShellyClient(ip string, password func() (string, error)) *infra.ShellyClient {
	return &infra.ShellyClient{IP: ip, Password: password, Timeout: 5 * time.Second}
}

// newProbeClient builds the infra.ProbeClient the probe adapter reaches hosts
// through. Same reasoning as newShellyClient: shared between New and the
// hand-built-Engine fallback in probeBackend below.
func newProbeClient(timeout time.Duration) *infra.ProbeClient {
	return &infra.ProbeClient{Timeout: timeout}
}

// execPower is the PowerBackend adapter: Wake.go's magic packet and the SSH
// agent verbs, poweroff included.
type execPower struct{ e *Engine }

func (p execPower) Wake(h inventory.Host) error { return p.e.Wake(h) }

func (p execPower) AgentVerb(ctx context.Context, h inventory.Host, verb string) (string, error) {
	return p.e.ssh.agentVerb(ctx, h, verb)
}

func (p execPower) PowerOff(ctx context.Context, h inventory.Host) (out, diag string, err error) {
	return p.e.ssh.agentVerbFull(ctx, h, "poweroff")
}

// execProbe is the ProbeBackend adapter.
//
// It holds the mechanism -- an infra.ProbeClient -- rather than reaching
// back into the Engine for it, so changing how a host is probed (a native
// ICMP socket, a different sshd-detection) is an edit to the client, not to
// the Engine that also carries the shutdown and fan-guard policy. Ping is
// implemented for completeness -- a fake standing in for this interface must
// satisfy it too -- but policy code does not reach Ping through here: it
// stays behind isUp/liveness(), the pre-existing seam; see ProbeBackend's doc
// in backends.go. The client's Ping is the same infra.Ping that isUp's real
// default (Engine.pingOnce) calls, so this and isUp are one mechanism
// reachable two ways, not two that could disagree. SSH has no such legacy
// seam, so probeOne reaches it through here.
type execProbe struct{ client *infra.ProbeClient }

func (p execProbe) Ping(ctx context.Context, ip string) (up, known bool) {
	return p.client.Ping(ctx, ip)
}

func (p execProbe) SSH(ctx context.Context, h inventory.Host) bool {
	return p.client.SSH(ctx, h.IP, h.SSHPort)
}

// execFans is the FansBackend adapter: the Shelly plug's authenticated HTTP
// RPC, including the read-back settle.
//
// It holds the mechanism -- an infra.ShellyClient -- rather than reaching
// back into the Engine for it, so changing how the plug is talked to (a
// different firmware, a second fan switch) is an edit to the client and this
// adapter, not to the Engine. Engine carries the fan-guard and shutdown
// policy; this carries the I/O and the settle read-back the plug's slow
// relay forces.
type execFans struct{ shelly *infra.ShellyClient }

func (f execFans) Status(ctx context.Context) (FansState, error) {
	on, err := f.shelly.Status(ctx)
	return FansState{On: on, IP: f.shelly.IP}, err
}

// fansSettleAttempts bounds the read-back retry after Switch.Set, and
// fansSettleInterval is the gap between attempts.
//
// The read-back is not belt-and-braces: Shelly's RPC reports some failures in
// a 200 response body, and a digest auth failure returns a 401 with a body
// too. Trusting the status code alone would report success while the plug sat
// untouched -- which, on the "off" path, means the fans keep running after a
// shutdown, and on the "on" path means the rack heats up under load.
//
// The read-back is retried rather than taken once, because the relay does not
// flip instantly: Switch.Set returns as soon as the command is accepted, and
// a GetStatus issued immediately after can still report the previous state. A
// single eager read turned a perfectly good switch-on into a failed job on
// 2026-08-09 -- "asked for on=true, it reports on=false" -- while the plug was
// in fact on, and the whole rack was left powered down as a result.
//
// This does not weaken the check: a plug that genuinely never changes still
// fails, just after a bounded wait instead of instantly.
const (
	fansSettleAttempts = 6
	fansSettleInterval = 500 * time.Millisecond
)

// Set switches the plug and returns its state read back from the device,
// retrying the read-back until the relay reports the requested state or the
// settle budget runs out. See the constants above for why the read-back
// exists at all and why it is retried.
func (f execFans) Set(ctx context.Context, on bool) (FansState, error) {
	if err := f.shelly.Set(ctx, on); err != nil {
		return FansState{IP: f.shelly.IP}, err
	}

	output, err := f.shelly.Status(ctx)
	if err != nil {
		return FansState{IP: f.shelly.IP}, err
	}
	for attempt := 1; attempt < fansSettleAttempts && output != on; attempt++ {
		select {
		case <-ctx.Done():
			return FansState{On: output, IP: f.shelly.IP}, ctx.Err()
		case <-time.After(fansSettleInterval):
		}
		output, err = f.shelly.Status(ctx)
		if err != nil {
			return FansState{On: output, IP: f.shelly.IP}, err
		}
	}
	if output != on {
		return FansState{On: output, IP: f.shelly.IP},
			fmt.Errorf("shelly plug did not change state: asked for on=%t, "+
				"it still reports on=%t after %s", on, output,
				time.Duration(fansSettleAttempts-1)*fansSettleInterval)
	}
	return FansState{On: output, IP: f.shelly.IP}, nil
}

// execNFS is the NFSChecker adapter.
//
// Mounts is implemented for completeness, the same reason as execProbe.Ping:
// checkLocalNFS lists what is mounted through Engine.localMounts/nfsMounts,
// the pre-existing seam, not through here; see NFSChecker's doc in
// backends.go. It still has to go THROUGH that seam rather than around it --
// delegating to n.e.localMounts, which falls back to the free function
// localNFSMounts only when nfsMounts is unset, rather than calling
// localNFSMounts directly -- so there is exactly one path to "what is
// mounted", not two that could disagree. A test that overrides eng.nfsMounts
// to fake the mount table but forgets to also stub eng.nfs would otherwise
// have nfsBackend().Mounts() silently fall through to the real mount table of
// whatever machine runs the test. Unmount is new -- checkLocalNFS shelled out
// to umount(8) inline before this refactor, with no seam a test could
// substitute.
type execNFS struct{ e *Engine }

func (n execNFS) Mounts(ctx context.Context) ([]string, error) {
	return n.e.localMounts(ctx)
}

func (n execNFS) Unmount(ctx context.Context, mountpoint string) (out string, err error) {
	bin := infra.UmountPath()
	if bin == "" {
		return "", errors.New("umount(8) not found")
	}
	raw, err := exec.CommandContext(ctx, bin, mountpoint).CombinedOutput()
	return strings.TrimSpace(string(raw)), err
}

// execZusb is the ZusbChecker adapter: the zusb-status and zusb-unload agent
// verbs zusbPreflight runs before any host in the shutdown list loses power.
type execZusb struct{ e *Engine }

func (z execZusb) Status(ctx context.Context, h inventory.Host) (string, error) {
	return z.e.ssh.agentVerb(ctx, h, "zusb-status")
}

func (z execZusb) Unload(ctx context.Context, h inventory.Host) (string, error) {
	return z.e.ssh.agentVerb(ctx, h, "zusb-unload")
}
