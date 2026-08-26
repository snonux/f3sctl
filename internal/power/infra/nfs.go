// Package infra holds the platform-specific parsing internal/power depends
// on to answer two questions that vary by runtime.GOOS: what is mounted, and
// whether ping(8) heard anything back.
//
// This package has no opinion on what a caller does with the answer -- that
// is policy, and it stays in internal/power. checkLocalNFS deciding to
// refuse a shutdown because a mount would not let go, or the fan guard
// deciding an unprobeable host must count as running, are both judgment
// calls about consequences; nothing here makes one. What lives here is only
// "how do we read /proc/mounts on Linux versus `mount -t nfs` on the BSDs"
// and "where does ping(8) live, and which flag spells a per-packet deadline
// on this OS" -- infrastructure detail that would otherwise leak into the
// same files as the shutdown-ordering and refusal rules it has nothing to do
// with. internal/power is this package's only importer today, reaching it
// through the NFSChecker/ProbeBackend adapters in backends_exec.go and
// through the isUp seam (see Engine.pingOnce), but nothing here depends on
// power, so a different orchestrator with different fan or shutdown rules
// could reuse these parsers unchanged.
package infra

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// NFSMounts returns the mount points of every locally mounted NFS
// filesystem, using whichever parsing this OS needs.
func NFSMounts(ctx context.Context) ([]string, error) {
	if runtime.GOOS == "linux" {
		return linuxNFSMounts()
	}
	return bsdNFSMounts(ctx, MountPath())
}

// linuxNFSMounts reads /proc/mounts and hands it to parseProcMounts, which is
// the part that actually knows the format.
func linuxNFSMounts() ([]string, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	// /proc/mounts is read and parsed; the close error is not actionable and
	// is explicitly discarded so errcheck keeps flagging write-path os.File
	// closes (see .golangci.yml).
	defer func() { _ = f.Close() }()
	return parseProcMounts(f)
}

// parseProcMounts reads /proc/mounts-formatted text and returns the mount
// points whose third field (the filesystem type) starts with "nfs" (nfs or
// nfs4).
//
// Split out from linuxNFSMounts so the format can be pinned with an
// in-memory reader rather than depending on this test box's actual mount
// table, which may or may not have an NFS share mounted at test time.
func parseProcMounts(r io.Reader) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 3 && strings.HasPrefix(fields[2], "nfs") {
			out = append(out, fields[1])
		}
	}
	return out, sc.Err()
}

// mountCandidates are where mount(8) lives, most likely first.
//
// Looked up by absolute path rather than through PATH for the same reason as
// pingCandidates (ping.go): f3sctl's HTTP API CGI runs with the environment
// bozohttpd hands it -- on NetBSD that is
// /usr/bin:/bin:/usr/pkg/bin:/usr/local/bin, which does NOT include /sbin
// where mount actually is. Relying on PATH made checkLocalNFS's very first
// step -- listing what is mounted -- fail outright with "executable file not
// found in $PATH" on pi0/pi1, aborting every `power off`/`power all off` job
// before it touched a single host. Caught by an end-to-end power-off test
// against the real pi0 CGI on 2026-08-11.
var mountCandidates = []string{"/sbin/mount", "/usr/sbin/mount", "/bin/mount", "/usr/bin/mount"}

// MountPath resolves mount(8) once per process, falling back to PATH lookup
// as a last resort for a platform that keeps it somewhere else entirely.
var MountPath = sync.OnceValue(func() string {
	for _, p := range mountCandidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("mount"); err == nil {
		return p
	}
	return ""
})

// bsdNFSMounts runs `<bin> -t nfs` and hands its output to
// parseBSDMountOutput, which is the part that actually knows the format.
//
// bin takes an explicit path (resolved by the caller via MountPath) rather
// than a hard-coded "mount", the same seam Ping uses for ping(8): it lets a
// test drive this with a stub binary directly, without relying on PATH or on
// MountPath's own process-wide, once-only caching.
//
// A non-zero exit means "nothing of that type is mounted" on some of the
// BSDs here, so that is treated as an empty list rather than a failure.
// Failing to run mount(8) at all is a different matter and is reported: it is
// not evidence that nothing is mounted, and an empty list here would let a
// caller's shutdown proceed with the NFS share still mounted locally -- the
// exact hung mount and lost writes checkLocalNFS (internal/power) exists to
// prevent. The two are told apart by errors.As against *exec.ExitError: that
// type means mount(8) ran and exited non-zero, anything else means it never
// got to run at all (not found, not executable, fork failed).
func bsdNFSMounts(ctx context.Context, bin string) ([]string, error) {
	if bin == "" {
		return nil, errors.New("mount(8) not found")
	}
	out, err := exec.CommandContext(ctx, bin, "-t", "nfs").Output()
	if err != nil {
		if errors.As(err, new(*exec.ExitError)) {
			return nil, nil
		}
		return nil, err
	}
	return parseBSDMountOutput(string(out)), nil
}

// umountCandidates are where umount(8) lives, most likely first.
//
// Same reasoning and same incident as mountCandidates above: checkLocalNFS's
// real unmount step (internal/power/backends_exec.go's execNFS.Unmount) shells
// out to umount(8), which sits in /sbin alongside mount(8) and is equally
// absent from the CGI's PATH.
var umountCandidates = []string{"/sbin/umount", "/usr/sbin/umount", "/bin/umount", "/usr/bin/umount"}

// UmountPath resolves umount(8) once per process, falling back to PATH
// lookup as a last resort for a platform that keeps it somewhere else
// entirely.
var UmountPath = sync.OnceValue(func() string {
	for _, p := range umountCandidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("umount"); err == nil {
		return p
	}
	return ""
})

// parseBSDMountOutput parses `mount -t nfs`'s stdout, whose lines read
// "server:/export on /mountpoint (nfs, ...)" on all the BSDs here, into the
// list of mount points.
//
// Split out from bsdNFSMounts so the format can be pinned directly, without
// exec(2) or a real mount(8) -- the exit-code/ExitError handling around it is
// tested separately, against an explicit stub binary path.
func parseBSDMountOutput(out string) []string {
	var mounts []string
	for _, line := range strings.Split(out, "\n") {
		_, rest, ok := strings.Cut(line, " on ")
		if !ok {
			continue
		}
		mp, _, ok := strings.Cut(rest, " (")
		if !ok {
			continue
		}
		if mp = strings.TrimSpace(mp); mp != "" {
			mounts = append(mounts, mp)
		}
	}
	return mounts
}
