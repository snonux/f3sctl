package power

import (
	"context"

	"github.com/snonux/f3sctl/internal/inventory"
)

// This file collects the interfaces Engine's policy methods (Off, On,
// awaitPowerDown, zusbPreflight, and the smaller steps they call --
// checkLocalNFS, shutdownEach, fansOffOnceTheRackIsIdle) depend on, instead
// of reaching exec(2), an SSH client or an HTTP client directly. Each one is
// deliberately narrow -- one capability, not "do anything on a host" -- so a
// fake can stand in for exactly what a given policy method needs and nothing
// more.
//
// New wires every one of them to an adapter that performs the real mechanism
// (ssh(1), ping(8), umount(8), the Shelly HTTP RPC, a UDP magic packet); this
// file itself never touches the network or a subprocess. A hand-built Engine
// (one not constructed by New) still works: every field these interfaces sit
// in has a nil-safe accessor -- powerBackend, probeBackend, fansBackend,
// nfsBackend and zusbBackend, in engine.go -- following the same pattern
// already established there for isUp, nfsMounts and report.
//
// This is the seam that makes the ordering and refusal logic in off() and
// on() testable without a real rack: a test can construct an Engine with
// fakes here instead of real SSH keys, a Shelly plug and packets on the wire.
// It is also what a second delivery channel -- a scheduler running
// unattended, say, rather than the CLI or the HTTP API -- would need to reuse
// the same policy: call power.New for the real fleet, or build an Engine
// around its own backends, and get the same ordering and safety checks
// either way.

// PowerBackend reaches a host to change what it is doing: wake it with a
// magic packet, run one of its agent verbs, or ask it to power off.
//
// AgentVerb and PowerOff are kept apart because shutdownEach needs more than
// AgentVerb gives back: PowerOff surfaces the diagnostic a forced guest stop
// writes to stderr on an otherwise-successful poweroff (see
// runner.agentVerbFull), which AgentVerb's plain (stdout, error) pair would
// drop on the floor. Every other verb this package runs (gogios-mute,
// zusb-status, ...) never force-kills anything and has no such diagnostic, so
// it stays the simpler shape.
type PowerBackend interface {
	// Wake sends a Wake-on-LAN magic packet to h.
	Wake(h inventory.Host) error
	// AgentVerb runs one allowlisted verb on h's f3sctl agent and returns its
	// trimmed stdout.
	AgentVerb(ctx context.Context, h inventory.Host, verb string) (string, error)
	// PowerOff asks h to shut down, returning stdout, any stderr diagnostic
	// from an otherwise-successful run, and an error if the command itself
	// failed.
	PowerOff(ctx context.Context, h inventory.Host) (out, diag string, err error)
}

// ProbeBackend answers "is this host reachable" without changing anything on
// it: an ICMP echo, tri-state because a probe that could not be carried out
// must never be confused with a host that stayed silent (see the isUp field
// on Engine); and a bare TCP dial of sshd, which only says whether the host
// has finished booting.
//
// Ping is not dispatched through here today. It stays behind the isUp field,
// which predates this interface and is the seam three safety-critical
// decisions already depend on and are heavily tested against --
// Engine.isRunning, Engine.confirmLiveness, awaitPowerDown -- see isUp's own
// doc for the full reasoning; duplicating that reasoning onto a second seam
// risked exactly the kind of drift the tri-state fix was written to close.
// execProbe.Ping still implements Ping for real, though: pingOnce (isUp's
// real-world default) is defined in terms of it, so this is one mechanism
// reachable two ways, not two mechanisms that could disagree. SSH has no such
// legacy seam -- probeOne dialled it directly until this refactor -- so it is
// wired through this interface from the start.
type ProbeBackend interface {
	Ping(ctx context.Context, ip string) (up, known bool)
	SSH(ctx context.Context, h inventory.Host) bool
}

// FansBackend switches and reads back the rack-fan Shelly plug.
type FansBackend interface {
	Status(ctx context.Context) (FansState, error)
	Set(ctx context.Context, on bool) (FansState, error)
}

// NFSChecker lists and unmounts the NFS filesystems mounted locally, on the
// machine f3sctl itself runs on -- not on the hosts being powered off. See
// checkLocalNFS, in nfs.go, for why this runs before anything on the rack is
// touched.
//
// Mounts stays behind the nfsMounts field for the same reason Ping stays
// behind isUp: it is the pre-existing, already-tested seam, and a shutdown
// test needs to say what is mounted rather than depend on -- and act on --
// the mount table of whatever machine runs the test. checkLocalNFS reaches it
// through Engine.localMounts directly, and execNFS.Mounts (backends_exec.go)
// reaches the same field one hop further down rather than calling the free
// function localNFSMounts itself, so nfsBackend().Mounts() and the mount
// listing checkLocalNFS actually acts on can never disagree -- a test that
// stubs nfsMounts but forgets nfs cannot silently fall through to the real
// mount table via the latter. Unmount had no seam at all before this
// refactor: checkLocalNFS shelled out to umount(8) directly, so a test
// exercising the "could not unmount, abort" branch had no way to do so
// without a stuck mount on the test box.
type NFSChecker interface {
	Mounts(ctx context.Context) ([]string, error)
	Unmount(ctx context.Context, mountpoint string) (out string, err error)
}

// ZusbChecker reports whether the removable zusb backup pool is imported on a
// host, and exports it before that host loses power. Kept apart from
// PowerBackend.AgentVerb, which could run either verb just as well, because
// zusbPreflight's job is "check and export this one pool", not "run any verb
// on any host" -- the narrower interface is what stops a fake built for this
// test from being usable, by accident, to also stand in for a poweroff.
type ZusbChecker interface {
	// Status returns the zusb-status agent verb's trimmed output ("loaded" or
	// something else).
	Status(ctx context.Context, h inventory.Host) (string, error)
	// Unload exports the pool (zusb-unload), returning combined diagnostic
	// output for the log.
	Unload(ctx context.Context, h inventory.Host) (string, error)
}
