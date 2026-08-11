package infra

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseProcMounts pins how /proc/mounts is read on Linux: the second
// field is the mount point, the third is the filesystem type, and only nfs/
// nfs4 entries count.
//
// This is the piece linuxNFSMounts previously had no direct test for -- it
// was only exercised indirectly, against whatever this test box's real
// /proc/mounts happens to contain, which on a box with no NFS mounted never
// drives the "found one" branch at all.
func TestParseProcMounts(t *testing.T) {
	const sample = `sysfs /sys sysfs rw,nosuid,nodev,noexec 0 0
proc /proc proc rw,nosuid,nodev,noexec 0 0
192.168.1.138:/tank/media /mnt/media nfs4 rw,relatime 0 0
192.168.1.138:/tank/backup /mnt/backup nfs rw,relatime 0 0
tmpfs /run tmpfs rw,nosuid 0 0
`
	got, err := parseProcMounts(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"/mnt/media", "/mnt/backup"}
	if !equalStrings(got, want) {
		t.Errorf("parseProcMounts = %v, want %v", got, want)
	}
}

func TestParseProcMountsWithNoNFS(t *testing.T) {
	const sample = "sysfs /sys sysfs rw 0 0\ntmpfs /run tmpfs rw 0 0\n"
	got, err := parseProcMounts(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("parseProcMounts = %v, want none", got)
	}
}

// TestParseBSDMountOutput pins the "server:/export on /mountpoint (nfs, ...)"
// shape every BSD here prints for `mount -t nfs`.
func TestParseBSDMountOutput(t *testing.T) {
	const sample = "192.168.1.138:/tank/media on /mnt/media (nfs, nfsv3, tcp)\n" +
		"192.168.1.138:/tank/backup on /mnt/backup (nfs, nfsv3, tcp)\n"

	got := parseBSDMountOutput(sample)
	want := []string{"/mnt/media", "/mnt/backup"}
	if !equalStrings(got, want) {
		t.Errorf("parseBSDMountOutput = %v, want %v", got, want)
	}
}

func TestParseBSDMountOutputEmpty(t *testing.T) {
	if got := parseBSDMountOutput(""); len(got) != 0 {
		t.Errorf("parseBSDMountOutput(\"\") = %v, want none", got)
	}
}

// TestBSDNFSMountsToldApartFromAnUnrunnableMount pins the fix this codebase
// already carries a scar for: a non-zero exit from mount(8) (it ran, and
// said "nothing of this type is mounted") must be treated as an empty list,
// but mount(8) not being runnable at all must be reported as an error rather
// than silently read the same way -- an empty list there would let a caller
// proceed with an NFS share that is, for all this machine actually knows,
// still mounted.
//
// bsdNFSMounts takes bin as an explicit argument (the same seam Ping uses
// for ping(8)), so each case drives it directly with a stub binary path
// rather than manipulating PATH -- which would also race MountPath's
// process-wide sync.OnceValue cache across subtests.
func TestBSDNFSMountsToldApartFromAnUnrunnableMount(t *testing.T) {
	t.Run("mount ran and exited non-zero: empty, not an error", func(t *testing.T) {
		bin := writeStubMount(t, "#!/bin/sh\nexit 1\n")
		got, err := bsdNFSMounts(context.Background(), bin)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("bsdNFSMounts = %v, want none", got)
		}
	})

	t.Run("mount ran and printed a mount: parsed", func(t *testing.T) {
		bin := writeStubMount(t, "#!/bin/sh\necho '192.168.1.138:/tank/media on /mnt/media (nfs)'\n")
		got, err := bsdNFSMounts(context.Background(), bin)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"/mnt/media"}
		if !equalStrings(got, want) {
			t.Errorf("bsdNFSMounts = %v, want %v", got, want)
		}
	})

	t.Run("mount path empty: reported, not empty list", func(t *testing.T) {
		// The empty string is what MountPath returns when mount(8) could not
		// be found anywhere -- the CGI-PATH incident this whole seam exists
		// for (see mountCandidates). bsdNFSMounts must refuse to guess.
		_, err := bsdNFSMounts(context.Background(), "")
		if err == nil {
			t.Fatal("bsdNFSMounts = nil error, want one: mount(8) could not be resolved at all")
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("bsdNFSMounts returned an *exec.ExitError (%v); "+
				"an unresolved binary must not be classified as one", err)
		}
	})

	t.Run("mount path does not exist: reported, not empty list", func(t *testing.T) {
		// A resolved-but-wrong path (e.g. a stale cache entry) must fail the
		// same way as an empty one, not silently read as "nothing mounted".
		_, err := bsdNFSMounts(context.Background(), filepath.Join(t.TempDir(), "no-such-mount"))
		if err == nil {
			t.Fatal("bsdNFSMounts = nil error, want one: mount(8) could not be run at all")
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("bsdNFSMounts returned an *exec.ExitError (%v); "+
				"a start failure must not be classified as one", err)
		}
	})
}

// writeStubMount writes an executable script and returns its absolute path,
// for bsdNFSMounts's injectable bin argument to resolve to instead of a real
// mount(8).
func writeStubMount(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "mount")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the mount stub: %v", err)
	}
	return p
}
