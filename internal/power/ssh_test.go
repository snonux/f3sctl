package power

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/inventory"
)

// fakeSSHRunner returns a runner whose ssh(1) is script, found first on PATH,
// with a readable identity so the runner gets as far as exec.
func fakeSSHRunner(t *testing.T, script string) *runner {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("writing the fake ssh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	key := filepath.Join(dir, "id")
	if err := os.WriteFile(key, []byte("key\n"), 0o600); err != nil {
		t.Fatalf("writing the fake identity: %v", err)
	}
	cfg := config.Default()
	cfg.SSHIdentity = []string{key}
	return newRunner(cfg)
}

var sshTestHost = inventory.Host{Name: "f0", IP: "192.0.2.1", SSHPort: 22, SSHUser: "f3sctl"}

// TestAgentVerbWrapsTheCancellation pins that a verb killed by the caller's
// cancel (Ctrl-C mid-shutdown) is recognisable with errors.Is: the error used
// to be flattened with %s, so a caller could not tell "the operator
// interrupted" from "the host refused".
func TestAgentVerbWrapsTheCancellation(t *testing.T) {
	r := fakeSSHRunner(t, "exec sleep 10")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	_, err := r.agentVerb(ctx, sshTestHost, "poweroff")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a wrapped context.Canceled", err)
	}
	if !strings.HasPrefix(err.Error(), "f0: poweroff: ") {
		t.Errorf("err = %v, want it to still name the host and verb", err)
	}
}

// TestAgentVerbRemoteFailureIsNotACancellation is the negative: a verb that
// fails on its own reports the remote's stderr and no context error.
func TestAgentVerbRemoteFailureIsNotACancellation(t *testing.T) {
	r := fakeSSHRunner(t, "echo 'permission denied' >&2; exit 1")

	_, err := r.agentVerb(context.Background(), sshTestHost, "poweroff")
	if err == nil || err.Error() != "f0: poweroff: permission denied" {
		t.Fatalf("err = %v, want exactly the remote's stderr", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want no context error in a plain remote failure", err)
	}
}
