package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWriteReplacesTheFile pins the happy path: the content lands, with mode
// 0600, and no temp file is left behind.
func TestWriteReplacesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	for _, body := range []string{"one", "two"} {
		if err := Write(path, []byte(body), "state", time.Minute); err != nil {
			t.Fatalf("Write(%q): %v", body, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "two" {
		t.Fatalf("ReadFile = %q, %v; want the last write", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 0600", perm)
	}
	assertNoTemps(t, filepath.Dir(path))
}

// TestWriteCleansUpWhenTheRenameFails is the negative path: a rename that
// cannot succeed (a non-empty directory occupies path) returns an error that
// names desc and leaves no temp file.
func TestWriteCleansUpWhenTheRenameFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o700); err != nil {
		t.Fatalf("seeding the blocking dir: %v", err)
	}
	err := Write(path, []byte("x"), "job state", time.Minute)
	if err == nil || !strings.Contains(err.Error(), "renaming the job state into place") {
		t.Fatalf("err = %v, want the rename failure naming desc", err)
	}
	assertNoTemps(t, dir)
}

// TestWriteSweepsOnlyStaleTemps pins the orphan sweep: an old temp sibling
// is removed, a fresh one (possibly a live writer's) and unrelated files are
// kept.
func TestWriteSweepsOnlyStaleTemps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	stale := seed(t, dir, "state.json.111.tmp", time.Hour)
	fresh := seed(t, dir, "state.json.222.tmp", time.Second)
	other := seed(t, dir, "other.json.333.tmp", time.Hour)

	if err := Write(path, []byte("x"), "state", time.Minute); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale temp survived: %v", err)
	}
	for _, keep := range []string{fresh, other} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s removed, want it kept: %v", filepath.Base(keep), err)
		}
	}
}

func seed(t *testing.T, dir, name string, age time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", name, err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatalf("backdating %s: %v", name, err)
	}
	return p
}

func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*"+tempSuffix))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

// fakeFile is a syncWriteCloser whose steps can each be made to fail; it
// records what was called so a test can check Close always runs.
type fakeFile struct {
	writeErr, syncErr, closeErr error
	synced, closed              bool
}

func (f *fakeFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}

func (f *fakeFile) Sync() error {
	f.synced = true
	return f.syncErr
}

func (f *fakeFile) Close() error {
	f.closed = true
	return f.closeErr
}

// TestWriteAndSyncFailures pins writeAndSync's error handling: a failure at
// Write, Sync or Close is returned wrapped with the failing step named, a
// Write failure skips the Sync, and Close runs on every path so no file
// descriptor leaks.
func TestWriteAndSyncFailures(t *testing.T) {
	boom := errors.New("boom")
	cases := []struct {
		name       string
		f          *fakeFile
		wantStep   string
		wantSynced bool
	}{
		{"write", &fakeFile{writeErr: boom}, "writing", false},
		{"sync", &fakeFile{syncErr: boom}, "syncing", true},
		{"close", &fakeFile{closeErr: boom}, "closing", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := writeAndSync(tc.f, []byte("x"), "state")
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want it to wrap %v", err, boom)
			}
			if !strings.Contains(err.Error(), tc.wantStep) {
				t.Errorf("err = %v, want it to name the %q step", err, tc.wantStep)
			}
			if tc.f.synced != tc.wantSynced {
				t.Errorf("synced = %v, want %v", tc.f.synced, tc.wantSynced)
			}
			if !tc.f.closed {
				t.Error("Close was not called after the failure")
			}
		})
	}

	// Happy path: all three steps run and no error is returned.
	ok := &fakeFile{}
	if err := writeAndSync(ok, []byte("x"), "state"); err != nil || !ok.synced || !ok.closed {
		t.Errorf("writeAndSync(ok) = %v, synced=%v closed=%v; want nil, true, true", err, ok.synced, ok.closed)
	}
}
