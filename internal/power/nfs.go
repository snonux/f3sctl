package power

import (
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/snonux/f3sctl/internal/power/infra"
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
//
// The listing goes through Engine.localMounts rather than straight to
// localNFSMounts, because the unmounting below is real: a test that drives a
// whole shutdown has to be able to say what is mounted instead of finding out
// from — and acting on — the mount table of the machine running it.
func (e *Engine) checkLocalNFS(ctx context.Context, log io.Writer) error {
	mounts, err := e.localMounts(ctx)
	if err != nil {
		return fmt.Errorf("listing local NFS mounts: %w", err)
	}

	if len(mounts) == 0 {
		fmt.Fprintln(log, "  No NFS filesystems mounted locally.")
		return nil
	}

	var stuck []string
	for _, mp := range mounts {
		out, err := e.nfsBackend().Unmount(ctx, mp)
		if err == nil {
			fmt.Fprintf(log, "  Unmounted %s\n", mp)
			continue
		}

		// It may have gone away between listing and unmounting. Re-checked
		// through the same seam as the initial listing (e.localMounts, not the
		// bare package function) so a test driving the "could not unmount"
		// branch controls this re-check too, rather than racing the real mount
		// table of whatever machine runs the test.
		still, lerr := e.localMounts(ctx)
		if lerr == nil && !slices.Contains(still, mp) {
			fmt.Fprintf(log, "  %s was already unmounted\n", mp)
			continue
		}

		fmt.Fprintf(log, "  ! Could not unmount %s: %s\n", mp, out)
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
//
// The actual parsing -- /proc/mounts on Linux, `mount -t nfs` on the BSDs --
// is platform detail with no policy of its own, so it lives in
// internal/power/infra (see infra.NFSMounts) rather than here. This stays a
// thin wrapper so the rest of the package, and the doc comments on New's
// nfsMounts field and on execNFS.Mounts, keep pointing at a name that lives
// in internal/power.
func localNFSMounts(ctx context.Context) ([]string, error) {
	return infra.NFSMounts(ctx)
}
