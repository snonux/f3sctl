package power

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/inventory"
)

// fakeAC fakes ACBackend, recording each Set into a shared sequence so a test
// can check where the plug was switched relative to the poweroffs and wakes.
// setErr scripts a failure for one requested state.
type fakeAC struct {
	mu     sync.Mutex
	seq    *sequence
	state  bool
	setErr map[bool]error
}

func (a *fakeAC) Status(context.Context) (ACState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return ACState{On: a.state}, nil
}

func (a *fakeAC) Set(_ context.Context, on bool) (ACState, error) {
	a.mu.Lock()
	err := a.setErr[on]
	if err == nil {
		a.state = on
	}
	st := ACState{On: a.state}
	a.mu.Unlock()

	a.seq.add("ac:" + strconv.FormatBool(on))
	return st, err
}

// cycleRig is newOffTestRig plus a fake AC plug and fake fans sharing one
// sequence with the fake power backend, and the cycle's waits collapsed.
type cycleRig struct {
	*offTestRig
	seq *sequence
	ac  *fakeAC
}

func newCycleRig(t *testing.T, up ...string) *cycleRig {
	t.Helper()
	rig := newOffTestRig(t, up...)
	seq := &sequence{}
	rig.power.seq = seq
	ac := &fakeAC{seq: seq, state: true}
	rig.eng.ac = ac
	rig.eng.fans = &fakeFans{seq: seq, state: true}
	rig.eng.acOffDwell = time.Millisecond
	rig.eng.acSettleWait = time.Millisecond
	return &cycleRig{offTestRig: rig, seq: seq, ac: ac}
}

// indexOf returns where step first appears in steps, or -1.
func indexOf(steps []string, step string) int {
	for i, s := range steps {
		if s == step {
			return i
		}
	}
	return -1
}

// TestCycleAllOrdersOffACOffACOnThenWake pins the whole point of the cycle:
// every host is powered off before AC is cut, AC comes back before any magic
// packet goes out, and every f-host -- f3 included -- is woken afterwards.
func TestCycleAllOrdersOffACOffACOnThenWake(t *testing.T) {
	rig := newCycleRig(t, "f0", "f1", "f2", "f3")

	if err := rig.eng.CycleAll(context.Background(), &rig.log); err != nil {
		t.Fatalf("power all cycle: %v\n%s", err, rig.log.String())
	}

	steps := rig.seq.get()
	acOff, acOn := indexOf(steps, "ac:false"), indexOf(steps, "ac:true")
	if acOff < 0 || acOn < acOff {
		t.Fatalf("sequence = %v, want ac:false then ac:true", steps)
	}
	for _, h := range []string{"f0", "f1", "f2", "f3"} {
		if i := indexOf(steps, "poweroff:"+h); i < 0 || i > acOff {
			t.Errorf("poweroff:%s at %d, want it before AC is cut (at %d): %v", h, i, acOff, steps)
		}
		if i := indexOf(steps, "wake:"+h); i < acOn {
			t.Errorf("wake:%s at %d, want it after AC is restored (at %d): %v", h, i, acOn, steps)
		}
	}
	if !rig.ac.state {
		t.Error("AC plug left off at the end of a successful cycle")
	}
}

// TestCycleAllLeavesACAloneWhenTheShutdownFails pins the safety half: a host
// that did not complete its shutdown may still be running, so the run must
// stop before the plug is touched -- that is what distinguishes a cycle from
// `ac off --force` -- and must not wake anything either.
func TestCycleAllLeavesACAloneWhenTheShutdownFails(t *testing.T) {
	rig := newCycleRig(t, "f0", "f1", "f2", "f3")
	rig.power.powerOffErr = map[string]error{"f2": errors.New("ssh: connection refused")}

	err := rig.eng.CycleAll(context.Background(), &rig.log)
	if err == nil || !strings.Contains(err.Error(), "before touching AC") {
		t.Fatalf("err = %v, want the cycle to stop before touching AC", err)
	}
	for _, s := range rig.seq.get() {
		if strings.HasPrefix(s, "ac:") || strings.HasPrefix(s, "wake:") {
			t.Errorf("sequence has %q after a failed shutdown: %v", s, rig.seq.get())
		}
	}
}

// TestCycleAllRefusesToCutACUnderAHostStillAnswering covers the strict dark
// check: a host the shutdown believed gone but which still answers (here one
// whose poweroff "succeeded" without it ever going silent is simulated by
// keeping f3 live after the off) keeps AC on.
func TestCycleAllRefusesToCutACUnderAHostStillAnswering(t *testing.T) {
	rig := newCycleRig(t, "f0", "f1", "f2")
	f3 := hostIP(t, rig.eng, "f3")
	base := rig.eng.isUp
	rig.eng.isUp = func(ctx context.Context, ip string) (bool, bool) {
		if ip == f3 {
			// Silent to the single-ping skip in the shutdown pre-flight,
			// then back: the stricter AC check must still catch it.
			return rig.power.calls() != nil, true
		}
		return base(ctx, ip)
	}

	err := rig.eng.CycleAll(context.Background(), &rig.log)
	if err == nil || !strings.Contains(err.Error(), "refusing to cut f-host AC") {
		t.Fatalf("err = %v, want the AC cut refused", err)
	}
	if i := indexOf(rig.seq.get(), "ac:false"); i >= 0 {
		t.Errorf("AC was cut with f3 still answering: %v", rig.seq.get())
	}
}

// TestCycleAllReportsAnACRestoreFailureLoudly pins the one unrecoverable
// outcome: AC cut and not restored. The error must say AC is still off and
// name the fix, and no magic packet may go out to hosts with no mains.
func TestCycleAllReportsAnACRestoreFailureLoudly(t *testing.T) {
	rig := newCycleRig(t, "f0", "f1", "f2", "f3")
	rig.ac.setErr = map[bool]error{true: errors.New("plug unreachable")}

	err := rig.eng.CycleAll(context.Background(), &rig.log)
	if err == nil || !strings.Contains(err.Error(), "AC is still OFF") ||
		!strings.Contains(err.Error(), "f3sctl ac on") {
		t.Fatalf("err = %v, want a loud AC-still-off error naming `f3sctl ac on`", err)
	}
	for _, s := range rig.seq.get() {
		if strings.HasPrefix(s, "wake:") {
			t.Errorf("woke a host with AC off: %v", rig.seq.get())
		}
	}
}

// TestCycleAllRestoresACEvenWhenCancelledMidDwell pins that tearing a run
// down while AC is off still brings AC back: a rack without mains is the one
// state nothing remote can recover from.
func TestCycleAllRestoresACEvenWhenCancelledMidDwell(t *testing.T) {
	rig := newCycleRig(t, "f0", "f1", "f2", "f3")
	rig.eng.acOffDwell = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	rig.power.onPowerOffEnd = func(h inventory.Host) {
		if h.Name == inventory.StorageMaster {
			// The master goes last; cancel shortly after, once the run is
			// inside the dwell.
			go func() { time.Sleep(200 * time.Millisecond); cancel() }()
		}
	}

	err := rig.eng.CycleAll(ctx, &rig.log)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !rig.ac.state {
		t.Errorf("AC left off after a cancelled cycle: %v", rig.seq.get())
	}
}
