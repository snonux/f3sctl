package power

import (
	"context"
	"os/exec"
	"strings"

	"github.com/snonux/f3sctl/internal/inventory"
)

// This file holds the adapters New wires the PowerBackend/ProbeBackend/
// FansBackend/NFSChecker/ZusbChecker fields to: thin structs that perform the
// real mechanism -- ssh(1), ping(8), umount(8), the Shelly HTTP RPC, a UDP
// magic packet -- which already lived elsewhere in this package (wol.go,
// ssh.go, shelly.go, probe.go, nfs.go) before this refactor. Each adapter
// delegates rather than reimplements; see backends.go for what each
// interface promises and why.
//
// The platform-specific parsing behind two of these mechanisms -- which
// ping(8) flags this OS wants, how to read the local NFS mount table -- has
// since moved one layer further down, into internal/power/infra (see nfs.go
// and probe.go, which now delegate to it). This file is unaffected: it still
// reaches those mechanisms through the same power-package functions/methods
// as before, which are themselves now thin wrappers over infra.

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
// Ping is implemented for completeness -- a fake standing in for this
// interface must satisfy it too -- but policy code does not reach Ping
// through here: it stays behind isUp/liveness(), the pre-existing seam; see
// ProbeBackend's doc in backends.go. pingOnce is itself the real default
// behind that seam, so this and isUp are one mechanism reachable two ways,
// not two that could disagree. SSH has no such legacy seam, so probeOne
// reaches it through here.
type execProbe struct{ e *Engine }

func (p execProbe) Ping(ctx context.Context, ip string) (up, known bool) {
	return p.e.pingOnce(ctx, ip)
}

func (p execProbe) SSH(ctx context.Context, h inventory.Host) bool {
	return p.e.dialSSH(ctx, h)
}

// execFans is the FansBackend adapter: the Shelly plug's authenticated HTTP
// RPC, including FansSet's read-back retry.
type execFans struct{ e *Engine }

func (f execFans) Status(ctx context.Context) (FansState, error) {
	return f.e.FansStatus(ctx)
}

func (f execFans) Set(ctx context.Context, on bool) (FansState, error) {
	return f.e.FansSet(ctx, on)
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
	raw, err := exec.CommandContext(ctx, "umount", mountpoint).CombinedOutput()
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
