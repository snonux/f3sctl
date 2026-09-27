package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
	"github.com/snonux/f3sctl/internal/powertest"
)

// fakePlugEngine is a plugEngine that records every call by method name, so a
// test can pin both that a plug was (or was not) switched and that the fans
// plug never reaches the AC methods or the other way round.
type fakePlugEngine struct {
	calls []string
	on    bool   // what a status read, or a successful set, reports
	ip    string // what a status read reports
	err   error  // returned by every method when set
	// onSet, when set, runs at the start of FansSet and ACSet: the moment the
	// plug is being written.
	onSet func()
}

func (f *fakePlugEngine) FansStatus(context.Context) (power.FansState, error) {
	f.calls = append(f.calls, "FansStatus")
	return power.FansState{On: f.on, IP: f.ip}, f.err
}

func (f *fakePlugEngine) FansSet(_ context.Context, on bool) (power.FansState, error) {
	if f.onSet != nil {
		f.onSet()
	}
	f.calls = append(f.calls, fmt.Sprintf("FansSet(%t)", on))
	if f.err != nil {
		return power.FansState{}, f.err
	}
	f.on = on
	return power.FansState{On: on, IP: f.ip}, nil
}

func (f *fakePlugEngine) ACStatus(context.Context) (power.ACState, error) {
	f.calls = append(f.calls, "ACStatus")
	return power.ACState{On: f.on, IP: f.ip}, f.err
}

func (f *fakePlugEngine) ACSet(_ context.Context, on bool) (power.ACState, error) {
	if f.onSet != nil {
		f.onSet()
	}
	f.calls = append(f.calls, fmt.Sprintf("ACSet(%t)", on))
	if f.err != nil {
		return power.ACState{}, f.err
	}
	f.on = on
	return power.ACState{On: on, IP: f.ip}, nil
}

// fakeJobs is a jobGuard that answers from a script: calls before busyFrom
// find the node idle and run fn, later ones refuse with err. It counts calls
// and records whether fn is running inside it, so a test can pin that the
// plug write happens under the guard.
type fakeJobs struct {
	busyFrom int
	err      error
	calls    int
	inside   bool
}

func (f *fakeJobs) whileIdle(_ context.Context, fn func() error) error {
	f.calls++
	if f.calls > f.busyFrom {
		return f.err
	}
	f.inside = true
	defer func() { f.inside = false }()
	return fn()
}

// errTestJobRunning is what the fake guard refuses with: the production
// guard's error shape, a wrapped coordination.ErrJobRunning.
var errTestJobRunning = fmt.Errorf("%w: \"all-cycle\" (job 42 on pi1)", coordination.ErrJobRunning)

// idleJobs is a job guard that never finds a job running.
func idleJobs() *fakeJobs { return &fakeJobs{busyFrom: math.MaxInt} }

// busyJobs is a job guard that finds the node idle for its first n calls and
// a job running from then on: busyJobs(0) is a job already running,
// busyJobs(1) one that started during the liveness probe.
func busyJobs(n int) *fakeJobs { return &fakeJobs{busyFrom: n, err: errTestJobRunning} }

// plugCase names one plug together with the wording and engine methods the
// CLI printed and called before fans and ac shared one implementation. The
// strings are spelled out rather than read back from the plug values so that a
// change to either plug's wording or wiring fails here.
type plugCase struct {
	plug        plug
	label       string
	statusCall  string
	setCallFmt  string // with %t for the requested state
	refusal     string // the busy refusal after "[f0 f3] may still be running; "
	interrupted string // what guardInterrupted names as untouched
}

func plugCases() []plugCase {
	return []plugCase{
		{
			plug: fansPlug, label: "rack fans", statusCall: "FansStatus", setCallFmt: "FansSet(%t)",
			refusal:     "refusing to switch the rack fans off. Use --force if you mean it",
			interrupted: "the rack fans left untouched",
		},
		{
			plug: acPlug, label: "f-host AC", statusCall: "ACStatus", setCallFmt: "ACSet(%t)",
			refusal: "refusing to cut f-host AC. Shut the hosts down first " +
				"(f3sctl power off / power all off), or use --force if you mean a hard cut",
			interrupted: "f-host AC left untouched",
		},
	}
}

// TestParsePlugArgsValidatesSpellings pins the one grammar both plug nouns
// share: exactly one of status/on/off, nothing else.
func TestParsePlugArgsValidatesSpellings(t *testing.T) {
	for _, verb := range []string{"status", "on", "off"} {
		if got, ok := parsePlugArgs([]string{verb}); !ok || got != verb {
			t.Errorf("parsePlugArgs([%s]) = %q, %v; want %q, true", verb, got, ok, verb)
		}
	}

	bad := [][]string{
		nil,
		{},
		{"of"},
		{"mute"},
		{"--force"},
		{"on", "f0"},
		{"off", "now"},
		{"status", "status"},
	}
	for _, args := range bad {
		if got, ok := parsePlugArgs(args); ok {
			t.Errorf("parsePlugArgs(%q) = %q, true; want a rejection", args, got)
		}
	}
}

// TestPlugOffRefusesWhileAHostMayBeRunning pins the busy-rack refusal for both
// plugs, word for word: it names the hosts, and switches nothing.
func TestPlugOffRefusesWhileAHostMayBeRunning(t *testing.T) {
	for _, tc := range plugCases() {
		t.Run(tc.plug.noun, func(t *testing.T) {
			eng := &fakePlugEngine{on: true}
			live := hostsUp("f0", "f3")
			var out bytes.Buffer

			err := plugOff(context.Background(), eng, tc.plug, false, live.hosts, idleJobs(), &out)
			want := "[f0 f3] may still be running; " + tc.refusal
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if len(eng.calls) != 0 || out.Len() != 0 {
				t.Errorf("calls = %v, output = %q; want none: the plug must be untouched", eng.calls, out.String())
			}
			if live.calls != 1 {
				t.Errorf("liveness consulted %d times, want once", live.calls)
			}
		})
	}
}

// TestPlugOffInterruptedGuardSwitchesNothing: a cancel landing during the
// probe makes the hosts read as running. That must come back as an
// interruption naming the plug left alone, never as the --force refusal.
func TestPlugOffInterruptedGuardSwitchesNothing(t *testing.T) {
	for _, tc := range plugCases() {
		t.Run(tc.plug.noun, func(t *testing.T) {
			eng := &fakePlugEngine{on: true}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			probe := func(context.Context) []string { cancel(); return []string{"f0"} }

			err := plugOff(ctx, eng, tc.plug, false, probe, idleJobs(), &bytes.Buffer{})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want a wrapped context.Canceled", err)
			}
			if strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), tc.interrupted) {
				t.Errorf("err = %v, want an interruption naming %q and no refusal", err, tc.interrupted)
			}
			if len(eng.calls) != 0 {
				t.Errorf("calls = %v, want none", eng.calls)
			}
		})
	}
}

// TestPlugOffForceSkipsTheGuard pins that --force switches the plug off
// without consulting liveness at all, and that the idle-rack path switches it
// off after one consultation.
func TestPlugOffForceSkipsTheGuard(t *testing.T) {
	for _, tc := range plugCases() {
		for _, force := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/force=%t", tc.plug.noun, force), func(t *testing.T) {
				eng := &fakePlugEngine{on: true}
				live := hostsUp("f0")
				if !force {
					live = hostsUp()
				}
				var out bytes.Buffer
				jobs := idleJobs()
				if force {
					// --force skips the job guard too: a busy one must not matter.
					jobs = busyJobs(0)
				}

				if err := plugOff(context.Background(), eng, tc.plug, force, live.hosts, jobs, &out); err != nil {
					t.Fatalf("plugOff: %v", err)
				}
				if want := []string{fmt.Sprintf(tc.setCallFmt, false)}; !slices.Equal(eng.calls, want) {
					t.Errorf("calls = %v, want %v", eng.calls, want)
				}
				if want := tc.label + ": off\n"; out.String() != want {
					t.Errorf("output = %q, want %q", out.String(), want)
				}
				if wantCalls := map[bool]int{true: 0, false: 1}[force]; live.calls != wantCalls {
					t.Errorf("liveness consulted %d times, want %d", live.calls, wantCalls)
				}
				if wantCalls := map[bool]int{true: 0, false: 2}[force]; jobs.calls != wantCalls {
					t.Errorf("job guard consulted %d times, want %d", jobs.calls, wantCalls)
				}
			})
		}
	}
}

// TestPlugOffRefusesWhileAJobRuns pins the job guard for both plugs: a job
// already running is refused up front -- before the half-minute liveness
// probe, which is not consulted -- the refusal says so and offers --force,
// and the plug is untouched.
func TestPlugOffRefusesWhileAJobRuns(t *testing.T) {
	for _, tc := range plugCases() {
		t.Run(tc.plug.noun, func(t *testing.T) {
			eng := &fakePlugEngine{on: true}
			live := hostsUp()
			var out bytes.Buffer

			err := plugOff(context.Background(), eng, tc.plug, false, live.hosts, busyJobs(0), &out)
			if !errors.Is(err, coordination.ErrJobRunning) {
				t.Fatalf("err = %v, want a wrapped coordination.ErrJobRunning", err)
			}
			for _, want := range []string{"all-cycle", tc.interrupted, "--force"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to mention %q", err, want)
				}
			}
			if len(eng.calls) != 0 || out.Len() != 0 {
				t.Errorf("calls = %v, output = %q; want none: the plug must be untouched", eng.calls, out.String())
			}
			if live.calls != 0 {
				t.Errorf("liveness consulted %d times, want none: a running job refuses first", live.calls)
			}
		})
	}
}

// TestPlugOffRefusesAJobStartedDuringTheProbe: the guard is asked again after
// the probe, and its answer beats the probe's verdict. A job waking the rack
// meanwhile makes the probe hear hosts, and "may still be running ... use
// --force" would send the operator the wrong way.
func TestPlugOffRefusesAJobStartedDuringTheProbe(t *testing.T) {
	for _, tc := range plugCases() {
		t.Run(tc.plug.noun, func(t *testing.T) {
			eng := &fakePlugEngine{on: true}
			jobs := busyJobs(1)

			err := plugOff(context.Background(), eng, tc.plug, false, hostsUp("f0").hosts, jobs, &bytes.Buffer{})
			if !errors.Is(err, coordination.ErrJobRunning) || strings.Contains(err.Error(), "may still be running") {
				t.Fatalf("err = %v, want the job refusal, not the liveness one", err)
			}
			if len(eng.calls) != 0 || jobs.calls != 2 {
				t.Errorf("calls = %v, job guard consulted %d times; want no switch after two checks", eng.calls, jobs.calls)
			}
		})
	}
}

// TestPlugOffSwitchesInsideTheJobGuard pins that the write itself runs under
// the guard -- holding the node's job lock in production -- rather than after
// a check that a job could start behind.
func TestPlugOffSwitchesInsideTheJobGuard(t *testing.T) {
	for _, tc := range plugCases() {
		t.Run(tc.plug.noun, func(t *testing.T) {
			jobs := idleJobs()
			underGuard := false
			eng := &fakePlugEngine{on: true, onSet: func() { underGuard = jobs.inside }}

			if err := plugOff(context.Background(), eng, tc.plug, false, hostsUp().hosts, jobs, &bytes.Buffer{}); err != nil {
				t.Fatalf("plugOff: %v", err)
			}
			if !underGuard {
				t.Error("the plug was written outside the job guard")
			}
		})
	}
}

// TestPlugOffRefusesWhenTheJobStateIsUnreadable: not knowing whether a job
// runs is not the same as idle. The error says so, is not dressed up as a
// running job, and switches nothing.
func TestPlugOffRefusesWhenTheJobStateIsUnreadable(t *testing.T) {
	for _, tc := range plugCases() {
		t.Run(tc.plug.noun, func(t *testing.T) {
			eng := &fakePlugEngine{on: true}
			jobs := &fakeJobs{err: fmt.Errorf("opening the job lock: %w", fs.ErrPermission)}

			err := plugOff(context.Background(), eng, tc.plug, false, hostsUp().hosts, jobs, &bytes.Buffer{})
			if !errors.Is(err, fs.ErrPermission) || errors.Is(err, coordination.ErrJobRunning) {
				t.Fatalf("err = %v, want the permission error, not ErrJobRunning", err)
			}
			if !strings.Contains(err.Error(), "cannot tell whether a power job is running") {
				t.Errorf("err = %q, want it to say the job state could not be read", err)
			}
			if len(eng.calls) != 0 {
				t.Errorf("calls = %v, want none", eng.calls)
			}
		})
	}
}

// TestPlugOffReportsASwitchFailure pins that a plug that cannot be switched is
// an error with nothing printed, not a claimed success.
func TestPlugOffReportsASwitchFailure(t *testing.T) {
	boom := errors.New("reaching the Shelly plug: boom")
	for _, tc := range plugCases() {
		t.Run(tc.plug.noun, func(t *testing.T) {
			eng := &fakePlugEngine{on: true, err: boom}
			var out bytes.Buffer

			err := plugOff(context.Background(), eng, tc.plug, false, hostsUp().hosts, idleJobs(), &out)
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want %v", err, boom)
			}
			if out.Len() != 0 {
				t.Errorf("output = %q, want nothing", out.String())
			}
		})
	}
}

// TestPlugVerbReachesOnlyItsOwnPlug drives status and on for each plug and
// pins which engine method ran and what was printed: a plug value wired to
// the other plug's methods would switch the wrong Shelly.
func TestPlugVerbReachesOnlyItsOwnPlug(t *testing.T) {
	for _, tc := range plugCases() {
		t.Run(tc.plug.noun+"/status", func(t *testing.T) {
			eng := &fakePlugEngine{on: true, ip: "192.0.2.9"}
			var out bytes.Buffer
			if err := plugVerb(context.Background(), eng, tc.plug, "status", false, nil, nil, &out); err != nil {
				t.Fatalf("status: %v", err)
			}
			if !slices.Equal(eng.calls, []string{tc.statusCall}) {
				t.Errorf("calls = %v, want [%s]", eng.calls, tc.statusCall)
			}
			if want := tc.label + ": on (192.0.2.9)\n"; out.String() != want {
				t.Errorf("output = %q, want %q", out.String(), want)
			}
		})
		t.Run(tc.plug.noun+"/on", func(t *testing.T) {
			eng := &fakePlugEngine{}
			live := hostsUp("f0")
			// A running job does not gate on either: it is the recovery path
			// for a job whose process died.
			jobs := busyJobs(0)
			var out bytes.Buffer
			if err := plugVerb(context.Background(), eng, tc.plug, "on", false, live.hosts, jobs, &out); err != nil {
				t.Fatalf("on: %v", err)
			}
			if want := []string{fmt.Sprintf(tc.setCallFmt, true)}; !slices.Equal(eng.calls, want) {
				t.Errorf("calls = %v, want %v", eng.calls, want)
			}
			if want := tc.label + ": on\n"; out.String() != want {
				t.Errorf("output = %q, want %q", out.String(), want)
			}
			if live.calls != 0 || jobs.calls != 0 {
				t.Errorf("liveness consulted %d times, job guard %d times; want none: on is never gated",
					live.calls, jobs.calls)
			}
		})
	}
}

// TestPlugProbesCountTheRightHosts pins which liveness probe each plug's off
// guard uses in production, against a real engine whose inventory holds only
// f3 (standalone: outside the fan-cooled power group, but on shelly2's mains).
//
// The context is cancelled up front, so ping(8) never runs and every probed
// host reads as unknown, i.e. running. The fan plug must judge only the power
// group -- empty here, which fails safe as "no hosts configured" -- and the AC
// plug every f-host, so f3 must keep `ac off` from cutting mains. Swapping the
// two probes flips both answers.
func TestPlugProbesCountTheRightHosts(t *testing.T) {
	shelly := powertest.NewFakeShelly(t, true)
	cfg := testConfig(t, shelly)
	cfg.Inventory.Hosts = []inventory.Host{
		{Name: "f3", Role: inventory.RoleF, IP: fHostIP, SSHPort: 22, SSHUser: "f3sctl", Standalone: true},
	}
	eng, err := power.New(cfg)
	if err != nil {
		t.Fatalf("power.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		plug plug
		want []string
	}{
		{fansPlug, []string{"the rack (no hosts configured)"}},
		{acPlug, []string{"f3"}},
	}
	for _, tt := range tests {
		t.Run(tt.plug.noun, func(t *testing.T) {
			if got := tt.plug.liveHosts(eng)(ctx); !slices.Equal(got, tt.want) {
				t.Errorf("%s probe = %q, want %q", tt.plug.noun, got, tt.want)
			}
		})
	}
}

// TestPlugNounsRejectBadArgs drives both nouns end to end through run: a
// missing verb is errUsage, and any other malformed spelling is an "unknown
// <noun> command" error with usage on stderr. Neither touches the plug.
func TestPlugNounsRejectBadArgs(t *testing.T) {
	for _, noun := range []string{"fans", "ac"} {
		for _, rest := range [][]string{{}, {"of"}, {"on", "f0"}, {"off", "now"}} {
			args := append([]string{noun}, rest...)
			t.Run(strings.Join(args, "_"), func(t *testing.T) {
				shelly := powertest.NewFakeShelly(t, true)
				cfg := testConfig(t, shelly)
				live := hostsUp("f0")

				out, errOut, err := runCLI(t, cfg, live, args...)
				switch {
				case len(rest) == 0 && !errors.Is(err, errUsage):
					t.Errorf("err = %v, want errUsage", err)
				case len(rest) > 0:
					want := fmt.Sprintf("unknown %s command %q", noun, strings.Join(rest, " "))
					if err == nil || err.Error() != want {
						t.Errorf("err = %v, want %q", err, want)
					}
				}
				if !strings.Contains(errOut, "f3sctl "+noun) || out != "" {
					t.Errorf("stdout = %q, stderr = %q; want usage on stderr only", out, errOut)
				}
				if got := shelly.SetCalls(); len(got) != 0 || live.calls != 0 {
					t.Errorf("Switch.Set calls = %v, liveness calls = %d; want none", got, live.calls)
				}
			})
		}
	}
}
