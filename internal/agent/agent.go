// Package agent implements the restricted host-side mode of f3sctl.
//
// On every f-host (f0-f3) sshd is configured with:
//
//	Match User f3sctl
//	    ForceCommand /usr/local/bin/f3sctl agent
//
// so the f3sctl account can run this and nothing else, whatever the client
// asks for. The requested verb arrives in $SSH_ORIGINAL_COMMAND.
//
// The OpenBSD gateways deliberately do NOT run f3sctl. They only ever needed
// to set and clear the Gogios mute marker, and putting a whole homelab power
// tool on the two internet-facing hosts to touch one file is more surface than
// that deserves. They run conf/frontends/scripts/f3s-gogios-mute behind the
// same ForceCommand arrangement instead, speaking the same bare verbs, so
// nothing on this side has to care which is at the far end.
//
// Two rules keep that surface honest:
//
//   - a request must be exactly one word from a fixed allowlist. Nothing is
//     interpolated, nothing reaches a shell, and no verb takes an argument, so
//     there is nothing an attacker holding the key can vary.
//   - the account is unprivileged. Verbs needing root re-enter this binary
//     through doas with an exact-argv allowlist (see RunPrivileged), instead of
//     the account being given general root.
package agent

import (
	"fmt"
	"os"
	"strings"
)

// verb is one allowlisted operation.
type verb struct {
	name string
	// privileged verbs are re-entered as root via doas rather than run
	// directly, because the f3sctl account has no privileges of its own.
	privileged bool
	run        func() error
}

// verbs is the complete surface reachable through the forced command.
//
// It is deliberately a closed list rather than anything derived at runtime:
// this is the security boundary, and it should be readable in one screen.
func verbs() []verb {
	return []verb{
		{name: "probe", privileged: true, run: runProbe},
		{name: "zusb-status", run: runZusbStatus},
		{name: "zusb-unload", privileged: true, run: runZusbUnload},
		{name: "poweroff", privileged: true, run: runPoweroff},
	}
}

// Run dispatches an agent request.
//
// args comes from the command line when f3sctl agent is invoked directly; when
// invoked as a forced command args is empty and the request is in
// $SSH_ORIGINAL_COMMAND instead.
func Run(args []string) error {
	name, err := requestedVerb(args)
	if err != nil {
		return err
	}

	for _, v := range verbs() {
		if v.name != name {
			continue
		}
		if v.privileged && os.Geteuid() != 0 {
			return reexecAsRoot(v.name)
		}
		return v.run()
	}
	return fmt.Errorf("unknown verb %q", name)
}

// RunPrivileged is the entry point doas invokes for verbs that need root.
//
// It refuses to do anything unless it really is root, so that a mistake in
// doas.conf surfaces as a clear error rather than as a verb silently failing
// half way through.
func RunPrivileged(args []string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("agent-root must run as root (check the doas rules for the f3sctl user)")
	}

	name, err := requestedVerb(args)
	if err != nil {
		return err
	}

	for _, v := range verbs() {
		if v.name == name {
			if !v.privileged {
				return fmt.Errorf("verb %q does not need root", name)
			}
			return v.run()
		}
	}
	return fmt.Errorf("unknown verb %q", name)
}

// requestedVerb extracts the single requested verb from argv, or from
// $SSH_ORIGINAL_COMMAND when argv is empty.
//
// Requiring exactly one bare word is the whole check. A client that sends
// "poweroff; rm -rf /" or "poweroff --now" is rejected outright rather than
// having its extra words stripped, because silently ignoring part of a request
// is how allowlists develop holes.
func requestedVerb(args []string) (string, error) {
	if len(args) == 0 {
		args = strings.Fields(os.Getenv("SSH_ORIGINAL_COMMAND"))
	}

	switch len(args) {
	case 0:
		return "", fmt.Errorf("no verb requested; expected one of: %s", verbNames())
	case 1:
		return args[0], nil
	default:
		return "", fmt.Errorf("expected exactly one verb, got %d words; expected one of: %s",
			len(args), verbNames())
	}
}

func verbNames() string {
	var names []string
	for _, v := range verbs() {
		names = append(names, v.name)
	}
	return strings.Join(names, ", ")
}
