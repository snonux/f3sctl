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
// setErr scripts a failure for one requested state; switchErr scripts the
// nastier one where the relay DID switch and the call still failed (a
// read-back cut short). onSet runs at the start of every Set, outside a.mu.
//
// Like the real execAC -- whose HTTP request fails on a done context -- Set
// switches nothing and returns ctx.Err() when its context is already done.
// That is what makes the cycle tests below notice a cut or restore that is
// not run on a detached context.
type fakeAC struct {
	mu        sync.Mutex
	seq       *sequence
	state     bool
	setErr    map[bool]error
	switchErr map[bool]error
	onSet     func(on bool)
}

func (a *fakeAC) Status(context.Context) (ACState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return ACState{On: a.state}, nil
}

func (a *fakeAC) Set(ctx context.Context, on bool) (ACState, error) {
	if a.onSet != nil {
		a.onSet(on)
	}
	a.mu.Lock()
	if err := ctx.Err(); err != nil {
		st := ACState{On: a.state}
		a.mu.Unlock()
		return st, err // the request never left: nothing switched, nothing recorded
	}
	err := a.setErr[on]
	if err == nil {
		a.state = on
		err = a.switchErr[on]
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
	if !strings.Contains(err.Error(), "interrupted") || !strings.Contains(err.Error(), "f3sctl power all on") {
		t.Errorf("err = %v, want it to say the cycle was interrupted and name `f3sctl power all on`", err)
	}
	if !rig.ac.state {
		t.Errorf("AC left off after a cancelled cycle: %v", rig.seq.get())
	}
}

// cancelOnCut returns a fakeAC hook that cancels the run's context the moment
// AC is cut -- the signal arriving while the cut is still in flight.
func cancelOnCut(cancel context.CancelFunc) func(bool) {
	return func(on bool) {
		if !on {
			cancel()
		}
	}
}

// assertACBackWithoutWake checks the end state every interrupted or failed
// cut must reach: the plug was cut and then switched back on, and no magic
// packet went out.
func assertACBackWithoutWake(t *testing.T, rig *cycleRig) {
	t.Helper()
	steps := rig.seq.get()
	if !rig.ac.state || indexOf(steps, "ac:true") < indexOf(steps, "ac:false") {
		t.Errorf("AC not switched back on after the cut: %v", steps)
	}
	for _, s := range steps {
		if strings.HasPrefix(s, "wake:") {
			t.Errorf("woke a host after an interrupted cycle: %v", steps)
		}
	}
}

// TestCycleAllRestoresACWhenCancelledJustAfterTheCut: the cut succeeds with
// the cancel already pending, so the dwell must return at once and the
// restore still run -- and the error must say what state the rack is in.
func TestCycleAllRestoresACWhenCancelledJustAfterTheCut(t *testing.T) {
	rig := newCycleRig(t, "f0", "f1", "f2", "f3")
	rig.eng.acOffDwell = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig.ac.onSet = cancelOnCut(cancel)

	err := rig.eng.CycleAll(ctx, &rig.log)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "power cycle interrupted") {
		t.Fatalf("err = %v, want a wrapped context.Canceled saying the cycle was interrupted", err)
	}
	assertACBackWithoutWake(t, rig)
}

// TestCycleAllRestoresACWhenTheCutFails is the non-cancelled variant: a cut
// that errors may still have switched the relay, so it is followed by a
// restore rather than trusting the error.
func TestCycleAllRestoresACWhenTheCutFails(t *testing.T) {
	rig := newCycleRig(t, "f0", "f1", "f2", "f3")
	boom := errors.New("read-back timed out")
	rig.ac.switchErr = map[bool]error{false: boom}

	err := rig.eng.CycleAll(context.Background(), &rig.log)
	if !errors.Is(err, boom) || errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cut failure and no cancellation", err)
	}
	assertACBackWithoutWake(t, rig)
}

// TestCycleAllReportsACutAndRestoreFailure covers the worst branch of
// cutAC: the cut fails (the relay having switched anyway) and the restore
// that follows fails too. The error must say AC is still off, name the fix,
// and keep both causes reachable with errors.Is.
func TestCycleAllReportsACutAndRestoreFailure(t *testing.T) {
	rig := newCycleRig(t, "f0", "f1", "f2", "f3")
	cutErr := errors.New("read-back timed out")
	restoreErr := errors.New("plug unreachable")
	rig.ac.switchErr = map[bool]error{false: cutErr}
	rig.ac.setErr = map[bool]error{true: restoreErr}

	err := rig.eng.CycleAll(context.Background(), &rig.log)
	if err == nil || !strings.Contains(err.Error(), "AC is still OFF") ||
		!strings.Contains(err.Error(), "f3sctl ac on") {
		t.Fatalf("err = %v, want a loud AC-still-off error naming `f3sctl ac on`", err)
	}
	if !errors.Is(err, cutErr) || !errors.Is(err, restoreErr) {
		t.Errorf("err = %v, want both the cut and the restore failure wrapped", err)
	}
	for _, s := range rig.seq.get() {
		if strings.HasPrefix(s, "wake:") {
			t.Errorf("woke a host with AC off: %v", rig.seq.get())
		}
	}
}

// TestCycleACCancelledBeforeTheCutLeavesACOn: a cancel between the shutdown
// and the cut must not cut, and must not be misreported as a busy rack (the
// dark check's probes, cut short, read as "unknown", which counts as busy).
func TestCycleACCancelledBeforeTheCutLeavesACOn(t *testing.T) {
	rig := newCycleRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := rig.eng.cycleAC(ctx, &rig.log)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v, want a wrapped context.Canceled, not a refusal", err)
	}
	if !strings.Contains(err.Error(), "AC left ON") {
		t.Errorf("err = %v, want it to say AC was left on", err)
	}
	if steps := rig.seq.get(); len(steps) != 0 {
		t.Errorf("plug touched after the run was cancelled: %v", steps)
	}
}
