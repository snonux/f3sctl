// Package atomicfile replaces small state files atomically: a reader sees
// either the old file or the new one, never a torn mix, even with several
// writers -- goroutines in one process or, as on the Pis, separate CGI and
// job-run processes -- racing each other.
//
// It is the one implementation behind the Gogios report cache
// (internal/gogios) and the power job record (internal/coordination's
// job.json), so fsync, temp-file cleanup and the orphan sweep cannot drift
// apart between them. It is a stdlib-only leaf.
package atomicfile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tempInfix and tempSuffix frame the random part of a temp file's name:
// <base>.<random>.tmp, next to the file it will replace.
const (
	tempInfix  = "."
	tempSuffix = ".tmp"
)

// Write atomically replaces path with raw, creating path's directory (0700)
// if needed.
//
// It writes a uniquely named temp file in path's directory, fsyncs it, and
// renames it over path. Each writer gets its own temp file, so concurrent
// writers never share bytes; the last rename wins and a reader only ever sees
// a complete file. The fsync keeps a power loss from leaving a renamed but
// torn file; the rename itself may still be lost, which only means the older
// (or no) file. The temp file is removed on any failure.
//
// Temp files orphaned by writers killed between create and rename are swept
// first: any older than staleTempAge (see RemoveStaleTemps). Pick an age well
// past a live write's duration.
//
// desc names the file in errors ("creating the <desc> temp file"), so each
// caller keeps messages that say what was being written.
func Write(path string, raw []byte, desc string, staleTempAge time.Duration) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the %s dir: %w", desc, err)
	}
	RemoveStaleTemps(path, staleTempAge)

	// os.CreateTemp creates the file with mode 0600.
	f, err := os.CreateTemp(dir, filepath.Base(path)+tempInfix+"*"+tempSuffix)
	if err != nil {
		return fmt.Errorf("creating the %s temp file: %w", desc, err)
	}
	tmp := f.Name()
	// Remove the temp file on any failure; after a successful rename it no
	// longer exists under this name.
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()

	if err := writeAndSync(f, raw, desc); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("renaming the %s into place: %w", desc, err)
	}
	return nil
}

// syncWriteCloser is the slice of *os.File that writeAndSync needs; it is an
// interface so a test can make each step fail.
type syncWriteCloser interface {
	io.WriteCloser
	Sync() error
}

// writeAndSync writes raw to f, fsyncs it, and closes it. f is closed on
// every path.
func writeAndSync(f syncWriteCloser, raw []byte, desc string) error {
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing the %s temp file: %w", desc, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("syncing the %s temp file: %w", desc, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing the %s temp file: %w", desc, err)
	}
	return nil
}

// RemoveStaleTemps deletes Write's temp siblings of path older than maxAge.
// It is best-effort: a process killed between CreateTemp and Rename leaves
// its uniquely named temp file behind, and without this sweep those would
// accumulate forever. Fresh temp files are left alone, since they may belong
// to a writer that is still running. Errors are ignored; the next write
// retries.
func RemoveStaleTemps(path string, maxAge time.Duration) {
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := filepath.Base(path) + tempInfix
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, tempSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) <= maxAge {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}
