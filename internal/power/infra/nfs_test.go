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
// bsdNFSMounts hard-codes the "mount" command name (matching mount(8)'s real
// invocation), so it is driven here by putting a stub executable named
// "mount" first on PATH -- the same technique the ping tests use, just via
// PATH instead of an explicit argument, because bsdNFSMounts (unlike Ping)
// takes no injectable binary path.
func TestBSDNFSMountsToldApartFromAnUnrunnableMount(t *testing.T) {
	t.Run("mount ran and exited non-zero: empty, not an error", func(t *testing.T) {
		withStubMount(t, "#!/bin/sh\nexit 1\n")
		got, err := bsdNFSMounts(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("bsdNFSMounts = %v, want none", got)
		}
	})

	t.Run("mount ran and printed a mount: parsed", func(t *testing.T) {
		withStubMount(t, "#!/bin/sh\necho '192.168.1.138:/tank/media on /mnt/media (nfs)'\n")
		got, err := bsdNFSMounts(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"/mnt/media"}
		if !equalStrings(got, want) {
			t.Errorf("bsdNFSMounts = %v, want %v", got, want)
		}
	})

	t.Run("mount could not be started at all: reported, not empty", func(t *testing.T) {
		// PATH points only at an empty directory, so exec.CommandContext
		// cannot find "mount" and the failure is a *fs.PathError / exec.Error,
		// never an *exec.ExitError.
		t.Setenv("PATH", t.TempDir())
		_, err := bsdNFSMounts(context.Background())
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

// withStubMount points PATH at a directory containing only an executable
// named "mount" running script, so bsdNFSMounts's hard-coded
// exec.CommandContext(ctx, "mount", ...) resolves to it instead of the real
// mount(8).
func withStubMount(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mount"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing the mount stub: %v", err)
	}
	t.Setenv("PATH", dir)
}
