package power

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/inventory"
)

// runner executes one allowlisted agent verb on a remote host.
//
// f3sctl shells out to ssh(1) rather than using golang.org/x/crypto/ssh: the
// dependency buys nothing here (every target enforces its own ForceCommand, so
// the client cannot influence what runs) and shelling out keeps the binary
// dependency-free for the BSD cross-builds.
type runner struct {
	cfg config.Config

	// identity is resolved on first use, not at construction. Status probing
	// needs no SSH at all, and it must keep working on a host that has no
	// f3sctl key -- a laptop that can still see whether the rack is up is
	// more useful than one that refuses to look.
	once     sync.Once
	identity string
	idErr    error

	// warn receives diagnostics an agent verb wrote to stderr while still
	// exiting 0. Nil means "nowhere to put them", which is only right for a
	// caller that has no log; the Engine always wires this up.
	warn func(host, verb, msg string)
}

func newRunner(cfg config.Config) *runner {
	return &runner{cfg: cfg}
}

func (r *runner) resolveIdentity() (string, error) {
	r.once.Do(func() {
		r.identity, r.idErr = r.cfg.ResolveSSHIdentity()
	})
	return r.identity, r.idErr
}

// sshArgs builds the option list used for every connection.
//
// Host keys are deliberately not verified. f3sctl connects to hosts by IP on a
// trusted LAN, and the hosts are reinstalled often enough that a pinned
// known_hosts would fail closed at exactly the wrong moment — during a
// recovery. What protects this path is the other direction: the key is locked
// to a ForceCommand and pinned by source address, so a spoofed server learns
// nothing but the verb name.
func (r *runner) sshArgs(h inventory.Host, identity string) []string {
	return []string{
		"-i", identity,
		"-o", "IdentitiesOnly=yes",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=" + strconv.Itoa(int(r.cfg.SSHConnectTimeout.D().Seconds())),
		"-p", strconv.Itoa(h.SSHPort),
		h.SSHUser + "@" + h.IP,
	}
}

// agentVerb runs one agent verb on h and returns its trimmed stdout.
//
// Diagnostics the agent wrote to stderr while still succeeding are reported
// through r.warn rather than dropped -- see agentVerbFull.
func (r *runner) agentVerb(ctx context.Context, h inventory.Host, verb string) (string, error) {
	stdout, _, err := r.agentVerbFull(ctx, h, verb)
	return stdout, err
}

// agentVerbFull runs one agent verb on h and returns its trimmed stdout and
// stderr separately.
//
// The verb is passed as the SSH command, which the target's ForceCommand turns
// into $SSH_ORIGINAL_COMMAND for `f3sctl agent`. Verbs are single words with no
// arguments precisely so there is nothing here an attacker could vary.
//
// stderr is returned even when the command SUCCEEDS, and that is the whole
// point of this function existing. The agent's `poweroff` verb force-kills a
// bhyve guest that outlives the 240s timeout, warns about it on stderr -- "check
// etcd health on the next boot" -- and then exits 0, because a forced stop is
// still a completed shutdown. An earlier version only looked at stderr on
// failure, so that warning was discarded every single time it mattered, and a
// run that SIGKILLed all three k3s guests was indistinguishable in the log from
// a clean one.
func (r *runner) agentVerbFull(ctx context.Context, h inventory.Host, verb string) (string, string, error) {
	identity, err := r.resolveIdentity()
	if err != nil {
		return "", "", err
	}
	args := append(r.sshArgs(h, identity), verb)

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	outStr := strings.TrimSpace(stdout.String())
	errStr := strings.TrimSpace(stderr.String())

	if runErr != nil {
		msg := errStr
		if msg == "" {
			msg = runErr.Error()
		}
		return outStr, errStr, fmt.Errorf("%s: %s: %s", h.Name, verb, msg)
	}

	if errStr != "" && r.warn != nil {
		r.warn(h.Name, verb, errStr)
	}
	return outStr, errStr, nil
}
