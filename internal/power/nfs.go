package power

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// checkLocalNFS unmounts every NFS filesystem mounted on the machine f3sctl is
// running on, and refuses to continue if any of them will not let go.
//
// The f3s NFS exports are served from the CARP VIP on f0/f1, tunnelled through
// stunnel. Powering those hosts off while a client still has the share mounted
// leaves the client with a hung or stale mount, and any write in flight is
// lost. Aborting the whole shutdown is the right trade: the cluster is still
// fully up at this point, so nothing has been half-done.
//
// This is a no-op on the Pis, which mount no NFS — but it is a real check
// there rather than an accident. Its bash predecessor read /proc/mounts
// unconditionally, so on NetBSD it silently found nothing and reported
// success regardless of what was mounted.
func (e *Engine) checkLocalNFS(ctx context.Context, log io.Writer) error {
	mounts, err := localNFSMounts(ctx)
	if err != nil {
		return fmt.Errorf("listing local NFS mounts: %w", err)
	}

	if len(mounts) == 0 {
		fmt.Fprintln(log, "  No NFS filesystems mounted locally.")
		return nil
	}

	var stuck []string
	for _, mp := range mounts {
		out, err := exec.CommandContext(ctx, "umount", mp).CombinedOutput()
		if err == nil {
			fmt.Fprintf(log, "  Unmounted %s\n", mp)
			continue
		}

		// It may have gone away between listing and unmounting.
		still, lerr := localNFSMounts(ctx)
		if lerr == nil && !contains(still, mp) {
			fmt.Fprintf(log, "  %s was already unmounted\n", mp)
			continue
		}

		fmt.Fprintf(log, "  ! Could not unmount %s: %s\n", mp, strings.TrimSpace(string(out)))
		stuck = append(stuck, mp)
	}

	if len(stuck) > 0 {
		return fmt.Errorf("NFS still mounted at %v; refusing to power hosts off. "+
			"Find what is holding them (fstat/lsof), or re-run with doas/sudo if this failed on permissions", stuck)
	}
	return nil
}

// localNFSMounts returns the mount points of every locally mounted NFS
// filesystem.
func localNFSMounts(ctx context.Context) ([]string, error) {
	if runtime.GOOS == "linux" {
		return linuxNFSMounts()
	}
	return bsdNFSMounts(ctx)
}

// linuxNFSMounts reads /proc/mounts, whose third field is the filesystem type
// (nfs or nfs4).
func linuxNFSMounts() ([]string, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 3 && strings.HasPrefix(fields[2], "nfs") {
			out = append(out, fields[1])
		}
	}
	return out, sc.Err()
}

// bsdNFSMounts parses `mount -t nfs`, whose lines read
// "server:/export on /mountpoint (nfs, ...)" on all the BSDs here.
//
// A non-zero exit means "nothing of that type is mounted" on some of them, so
// an error is treated as an empty list rather than a failure.
func bsdNFSMounts(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, "mount", "-t", "nfs").Output()
	if err != nil {
		return nil, nil
	}

	var mounts []string
	for _, line := range strings.Split(string(out), "\n") {
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
	return mounts, nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
