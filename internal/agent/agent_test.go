package agent

import (
	"strings"
	"testing"
)

// TestRequestedVerbRejectsAnythingButOneWord is the important test in this
// package. The agent sits behind an SSH forced command, so $SSH_ORIGINAL_COMMAND
// is entirely attacker-controlled: it is whatever the client asked for. The
// only thing standing between that string and the host is this parser, so it
// must accept exactly one bare word from the allowlist and reject everything
// else outright -- never trim, never take the first word and ignore the rest.
func TestRequestedVerbRejectsAnythingButOneWord(t *testing.T) {
	rejected := []string{
		"",                       // no verb at all
		"poweroff extra",         // trailing junk
		"poweroff; rm -rf /",     // command injection through a shell
		"poweroff && reboot",     //
		"poweroff|tee /etc/x",    //
		"zusb-unload --now",      // an argument we do not accept
		"probe /etc/master.pass", // a path argument
	}

	for _, cmd := range rejected {
		t.Setenv("SSH_ORIGINAL_COMMAND", cmd)
		if got, err := requestedVerb(nil); err == nil {
			t.Errorf("SSH_ORIGINAL_COMMAND=%q was accepted as verb %q; it must be rejected", cmd, got)
		}
	}
}

// TestRequestedVerbAcceptsBareWords confirms the legitimate case still works,
// including the extra whitespace ssh may leave around a command.
func TestRequestedVerbAcceptsBareWords(t *testing.T) {
	for _, name := range []string{"probe", "zusb-status", "zusb-unload", "poweroff"} {
		t.Setenv("SSH_ORIGINAL_COMMAND", "  "+name+" \n")
		got, err := requestedVerb(nil)
		if err != nil {
			t.Errorf("verb %q was rejected: %v", name, err)
			continue
		}
		if got != name {
			t.Errorf("verb %q parsed as %q", name, got)
		}
	}
}

// TestUnknownVerbIsRejected checks that a syntactically valid single word
// still has to be on the allowlist.
func TestUnknownVerbIsRejected(t *testing.T) {
	t.Setenv("SSH_ORIGINAL_COMMAND", "")
	err := Run([]string{"reboot"})
	if err == nil {
		t.Fatal("an unknown verb was accepted")
	}
	if !strings.Contains(err.Error(), "unknown verb") {
		t.Errorf("expected an unknown-verb error, got: %v", err)
	}
}

// TestArgvBeatsEnvironment confirms a directly-invoked agent uses its argv.
// The forced-command path passes no argv, so the environment is only consulted
// when there is none.
func TestArgvBeatsEnvironment(t *testing.T) {
	t.Setenv("SSH_ORIGINAL_COMMAND", "poweroff")
	got, err := requestedVerb([]string{"probe"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "probe" {
		t.Errorf("argv should win over SSH_ORIGINAL_COMMAND, got %q", got)
	}
}

// TestPrivilegedVerbsAreMarked pins which verbs need root. Getting this wrong
// in either direction is a real problem: a privileged verb left unmarked fails
// confusingly at runtime, and an unprivileged one marked privileged would
// widen the doas allowlist for no reason.
func TestPrivilegedVerbsAreMarked(t *testing.T) {
	want := map[string]bool{
		"probe":        true,
		"zusb-status":  false,
		"zusb-unload":  true,
		"carp-quiesce": true,
		"poweroff":     true,
	}

	got := map[string]bool{}
	for _, v := range verbs() {
		got[v.name] = v.privileged
	}

	if len(got) != len(want) {
		t.Fatalf("the verb list changed: got %v, expected the %d verbs in this test", got, len(want))
	}
	for name, wantPriv := range want {
		gotPriv, ok := got[name]
		if !ok {
			t.Errorf("verb %q is missing", name)
			continue
		}
		if gotPriv != wantPriv {
			t.Errorf("verb %q privileged = %v, want %v", name, gotPriv, wantPriv)
		}
	}
}

// TestRunPrivilegedRefusesWithoutRoot ensures a mistake in doas.conf surfaces
// as a clear error rather than as a verb half-failing.
func TestRunPrivilegedRefusesWithoutRoot(t *testing.T) {
	if err := RunPrivileged([]string{"poweroff"}); err == nil {
		t.Fatal("agent-root ran as a non-root user")
	}
}
