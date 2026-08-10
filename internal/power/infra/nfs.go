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
)

// NFSMounts returns the mount points of every locally mounted NFS
// filesystem, using whichever parsing this OS needs.
func NFSMounts(ctx context.Context) ([]string, error) {
	if runtime.GOOS == "linux" {
		return linuxNFSMounts()
	}
	return bsdNFSMounts(ctx)
}

// linuxNFSMounts reads /proc/mounts and hands it to parseProcMounts, which is
// the part that actually knows the format.
func linuxNFSMounts() ([]string, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()
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

// bsdNFSMounts runs `mount -t nfs` and hands its output to
// parseBSDMountOutput, which is the part that actually knows the format.
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
func bsdNFSMounts(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, "mount", "-t", "nfs").Output()
	if err != nil {
		if errors.As(err, new(*exec.ExitError)) {
			return nil, nil
		}
		return nil, err
	}
	return parseBSDMountOutput(string(out)), nil
}

// parseBSDMountOutput parses `mount -t nfs`'s stdout, whose lines read
// "server:/export on /mountpoint (nfs, ...)" on all the BSDs here, into the
// list of mount points.
//
// Split out from bsdNFSMounts so the format can be pinned directly, without
// exec(2) or a real mount(8) -- the exit-code/ExitError handling around it is
// tested separately, against a stub "mount" on PATH.
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
