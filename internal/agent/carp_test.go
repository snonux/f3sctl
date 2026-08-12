package agent

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// fakeCommands records what the verb ran and answers each call from a table
// keyed by "name arg arg".
type fakeCommands struct {
	ran  []string
	fail map[string]error
	out  map[string]string
}

func (f *fakeCommands) run(name string, args ...string) ([]byte, error) {
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.ran = append(f.ran, key)
	return []byte(f.out[key]), f.fail[key]
}

// install wires the fake in for the duration of one test.
func (f *fakeCommands) install(t *testing.T, carpPresent bool) {
	t.Helper()
	prevRun, prevStat := runCommand, fileExists
	runCommand = f.run
	fileExists = func(string) bool { return carpPresent }
	t.Cleanup(func() { runCommand, fileExists = prevRun, prevStat })
}

// TestCARPQuiesceStopsDevdAndAutoFailback pins what the verb actually does on
// a CARP member. devd is the load-bearing one: with it running, a state change
// mid-shutdown runs carpcontrol.sh and starts NFS on a host that is going
// down, which is the 2026-08-08 wedge this whole verb exists to prevent.
func TestCARPQuiesceStopsDevdAndAutoFailback(t *testing.T) {
	f := &fakeCommands{}
	f.install(t, true)

	if err := runCARPQuiesce(); err != nil {
		t.Fatalf("runCARPQuiesce: %v", err)
	}

	want := []string{
		"service cron status",
		"service cron stop",
		"service devd status",
		"service devd stop",
	}
	if strings.Join(f.ran, "|") != strings.Join(want, "|") {
		t.Errorf("ran %v, want %v", f.ran, want)
	}
}

// TestCARPQuiesceIsANoOpWithoutTheCARPScript pins that f2 and f3 -- which are
// not in the pair -- are cheap no-ops rather than failures. The engine asks
// every host, so a failure here would abort a perfectly good shutdown.
func TestCARPQuiesceIsANoOpWithoutTheCARPScript(t *testing.T) {
	f := &fakeCommands{}
	f.install(t, false)

	if err := runCARPQuiesce(); err != nil {
		t.Fatalf("runCARPQuiesce on a non-CARP host: %v", err)
	}
	if len(f.ran) != 0 {
		t.Errorf("a non-CARP host ran %v, want nothing", f.ran)
	}
}

// TestCARPQuiesceSucceedsWhenDevdIsAlreadyStopped pins that "the thing I
// wanted stopped is stopped" is success. service(8) exits non-zero for a
// stopped daemon, and treating that as an error would send the whole rack
// down the slow sequential path for no reason at all.
func TestCARPQuiesceSucceedsWhenDevdIsAlreadyStopped(t *testing.T) {
	f := &fakeCommands{fail: map[string]error{
		"service devd status": fmt.Errorf("devd is not running"),
	}}
	f.install(t, true)

	if err := runCARPQuiesce(); err != nil {
		t.Fatalf("runCARPQuiesce with devd already stopped: %v", err)
	}
	for _, ran := range f.ran {
		if ran == "service devd stop" {
			t.Error("stopped devd although it was not running")
		}
	}
}

// TestCARPQuiesceFailsWhenDevdCannotBeStopped pins the direction of the one
// real failure: the caller reads it as "do not shut these hosts down in
// parallel", so it must not be swallowed.
func TestCARPQuiesceFailsWhenDevdCannotBeStopped(t *testing.T) {
	f := &fakeCommands{fail: map[string]error{
		"service devd stop": fmt.Errorf("exit status 1"),
	}}
	f.install(t, true)

	err := runCARPQuiesce()
	if err == nil {
		t.Fatal("a devd that could not be stopped must be an error")
	}
	if !strings.Contains(err.Error(), "devd") {
		t.Errorf("error %q does not name devd", err)
	}
}

// TestCARPQuiesceToleratesCronFailure pins the other direction: with devd
// stopped, a promotion changes no services, so a cron that refuses to stop is
// a warning rather than a reason to slow the shutdown down.
func TestCARPQuiesceToleratesCronFailure(t *testing.T) {
	f := &fakeCommands{fail: map[string]error{
		"service cron stop": fmt.Errorf("exit status 1"),
	}}
	f.install(t, true)

	if err := runCARPQuiesce(); err != nil {
		t.Fatalf("runCARPQuiesce: %v", err)
	}
	if !slices.Contains(f.ran, "service devd stop") {
		t.Errorf("ran %v, want the devd stop to have gone ahead anyway", f.ran)
	}
}

// TestCARPQuiesceNeverWritesTheAutoFailbackBlockFile pins a trap worth a test
// of its own: `carp auto-failback disable` is the documented way to hold
// failback off, but it works by creating a file on the NFS dataset, which
// outlives the reboot. A shutdown that used it would disable auto-failback
// permanently.
func TestCARPQuiesceNeverWritesTheAutoFailbackBlockFile(t *testing.T) {
	f := &fakeCommands{}
	f.install(t, true)

	if err := runCARPQuiesce(); err != nil {
		t.Fatalf("runCARPQuiesce: %v", err)
	}
	for _, ran := range f.ran {
		if strings.Contains(ran, "auto-failback") {
			t.Errorf("ran %q: the block file it creates survives the reboot", ran)
		}
	}
}
