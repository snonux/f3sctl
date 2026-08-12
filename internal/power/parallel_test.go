package power

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/powertest"
)

// offTestRig is the common setup for a whole-rack shutdown against fakes: a
// fake plug, an engine over the f-hosts, and liveness that flips a host
// silent the moment its (fake) poweroff succeeds, so awaitPowerDown confirms
// without a real ping.
type offTestRig struct {
	eng   *Engine
	power *fakePower
	log   bytes.Buffer
}

func newOffTestRig(t *testing.T, up ...string) *offTestRig {
	t.Helper()

	shelly := powertest.NewFakeShelly(t, true)
	eng := testEngine(t, shelly, up...)
	eng.downProbeInterval = time.Millisecond

	var mu sync.Mutex
	live := map[string]bool{}
	for _, name := range up {
		live[hostIP(t, eng, name)] = true
	}
	eng.isUp = func(_ context.Context, ip string) (bool, bool) {
		mu.Lock()
		defer mu.Unlock()
		return live[ip], true
	}

	// The zusb pre-flight speaks SSH to every host; without a fake it fails
	// before the shutdown these tests are about even begins.
	eng.zusb = &fakeZusb{}

	rig := &offTestRig{eng: eng}
	rig.power = &fakePower{onPowerOff: func(h inventory.Host) {
		mu.Lock()
		live[h.IP] = false
		mu.Unlock()
	}}
	eng.power = rig.power
	return rig
}

// TestOffQuiescesCARPOnBothMembersOnly pins who is asked to stop its
// failover daemons: the two hosts the inventory names as the pair, and
// nobody else. f2 and f3 have no CARP configuration, and an SSH round trip to
// each of them for a verb that would do nothing is pure latency in front of
// every rack shutdown.
func TestOffQuiescesCARPOnBothMembersOnly(t *testing.T) {
	rig := newOffTestRig(t, "f0", "f1", "f2", "f3")

	if err := rig.eng.OffAll(context.Background(), &rig.log); err != nil {
		t.Fatalf("power all off: %v", err)
	}

	want := []string{carpQuiesceVerb + ":f0", carpQuiesceVerb + ":f1"}
	if got := rig.power.verbCalls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("agent verbs = %v, want exactly %v", got, want)
	}
}

// TestOffQuiescesCARPBeforeTheFirstPoweroff pins the ordering the safety
// argument rests on: the failover daemons are stopped while every host is
// still up. Quiescing after the first poweroff would leave precisely the
// window this exists to close -- a host taking the VIP, and carpcontrol.sh's
// "start NFS now", on its way down.
func TestOffQuiescesCARPBeforeTheFirstPoweroff(t *testing.T) {
	rig := newOffTestRig(t, "f0", "f1", "f2", "f3")

	var mu sync.Mutex
	var quiescedLate bool
	rig.power.onPowerOffEnd = func(inventory.Host) {
		mu.Lock()
		defer mu.Unlock()
		if len(rig.power.verbCalls()) < 2 {
			quiescedLate = true
		}
	}

	if err := rig.eng.OffAll(context.Background(), &rig.log); err != nil {
		t.Fatalf("power all off: %v", err)
	}
	if quiescedLate {
		t.Error("a host was powered off before both CARP members had been quiesced")
	}
}

// TestOffShutsTheBatchDownInParallel is the point of the whole change: with
// the failover daemons stopped, every host except the storage master goes down
// at once rather than one after another.
//
// It is proved by making each host's PowerOff block until all three have
// entered it. A sequential implementation deadlocks against that barrier and
// fails on the timeout; nothing else about the fake changes.
func TestOffShutsTheBatchDownInParallel(t *testing.T) {
	rig := newOffTestRig(t, "f0", "f1", "f2", "f3")

	const batchSize = 3 // f1, f2, f3 -- everything but the storage master
	entered := make(chan string, batchSize)
	release := make(chan struct{})
	rig.power.onPowerOffStart = func(h inventory.Host) {
		if h.Name == inventory.StorageMaster {
			return
		}
		entered <- h.Name
		<-release
	}

	done := make(chan error, 1)
	go func() { done <- rig.eng.OffAll(context.Background(), &rig.log) }()

	for i := 0; i < batchSize; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatalf("only %d of %d hosts had started shutting down; the batch is not parallel",
				i, batchSize)
		}
	}
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("power all off: %v", err)
	}
}

// TestOffPowersTheStorageMasterOffLast pins the half of the old ordering that
// survives, and that CARP has nothing to do with: every other host's guests
// are still writing to NFS served from the VIP while they stop, so the master
// may not go until the batch has finished.
func TestOffPowersTheStorageMasterOffLast(t *testing.T) {
	rig := newOffTestRig(t, "f0", "f1", "f2", "f3")

	var mu sync.Mutex
	inFlight := 0
	var masterOverlapped bool
	rig.power.onPowerOffStart = func(h inventory.Host) {
		mu.Lock()
		defer mu.Unlock()
		if h.Name == inventory.StorageMaster && inFlight > 0 {
			masterOverlapped = true
		}
		inFlight++
	}
	rig.power.onPowerOffEnd = func(inventory.Host) {
		mu.Lock()
		defer mu.Unlock()
		inFlight--
	}

	if err := rig.eng.OffAll(context.Background(), &rig.log); err != nil {
		t.Fatalf("power all off: %v", err)
	}

	if masterOverlapped {
		t.Error("the storage master was powered off while another host was still shutting down")
	}
	if got := rig.power.calls(); got[len(got)-1] != inventory.StorageMaster {
		t.Errorf("last poweroff was %s, want the storage master %s",
			got[len(got)-1], inventory.StorageMaster)
	}
}

// TestClusterOffShutsF1AndF2TogetherThenF0 pins the same shape for a bare
// `power off`, which leaves f3 out: f1 and f2 go down together, f0 follows on
// its own.
//
// Worth its own test rather than trusting the four-host case: a batch of two
// is the smallest one that may still run in parallel, and it is the everyday
// command. An off-by-one in the "is there more than one host in the batch"
// guard would quietly serialise exactly this run and nothing else.
func TestClusterOffShutsF1AndF2TogetherThenF0(t *testing.T) {
	rig := newOffTestRig(t, "f0", "f1", "f2", "f3")

	const batchSize = 2 // f1 and f2; f3 is not in the cluster power group
	entered := make(chan string, batchSize)
	release := make(chan struct{})
	var mu sync.Mutex
	inFlight := 0
	var masterOverlapped bool

	rig.power.onPowerOffStart = func(h inventory.Host) {
		mu.Lock()
		if h.Name == inventory.StorageMaster && inFlight > 0 {
			masterOverlapped = true
		}
		inFlight++
		mu.Unlock()

		if h.Name == inventory.StorageMaster {
			return
		}
		entered <- h.Name
		<-release
	}
	rig.power.onPowerOffEnd = func(inventory.Host) {
		mu.Lock()
		defer mu.Unlock()
		inFlight--
	}

	done := make(chan error, 1)
	go func() { done <- rig.eng.Off(context.Background(), &rig.log) }()

	for i := 0; i < batchSize; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatalf("only %d of %d hosts had started shutting down; f1 and f2 are not parallel",
				i, batchSize)
		}
	}
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("power off: %v", err)
	}
	if masterOverlapped {
		t.Error("f0 was powered off while f1 or f2 was still shutting down")
	}

	got := rig.power.calls()
	if len(got) != 3 {
		t.Fatalf("powered off %v, want exactly f1, f2 and f0 -- f3 is not in the power group", got)
	}
	if got[len(got)-1] != inventory.StorageMaster {
		t.Errorf("last poweroff was %s, want the storage master %s",
			got[len(got)-1], inventory.StorageMaster)
	}
}

// TestOffFallsBackToSequentialWhenCARPCannotBeQuiesced pins the failure
// direction. A rack whose failover daemons could not be stopped is not a
// rack that cannot be shut down -- it is one that must go down the old way,
// one host at a time, master last. Doing it in parallel anyway would risk the
// 2026-08-08 wedge for the sake of a few minutes.
func TestOffFallsBackToSequentialWhenCARPCannotBeQuiesced(t *testing.T) {
	rig := newOffTestRig(t, "f0", "f1", "f2", "f3")
	rig.power.agentVerbErr = map[string]error{"f1": errors.New("devd would not stop")}

	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	rig.power.onPowerOffStart = func(inventory.Host) {
		mu.Lock()
		defer mu.Unlock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
	}
	rig.power.onPowerOffEnd = func(inventory.Host) {
		mu.Lock()
		defer mu.Unlock()
		inFlight--
	}

	if err := rig.eng.OffAll(context.Background(), &rig.log); err != nil {
		t.Fatalf("power all off: %v", err)
	}

	if maxInFlight != 1 {
		t.Errorf("%d hosts shut down at once, want 1: the failover daemons were not stopped", maxInFlight)
	}
	if got := rig.power.calls(); got[len(got)-1] != inventory.StorageMaster {
		t.Errorf("last poweroff was %s, want the storage master %s",
			got[len(got)-1], inventory.StorageMaster)
	}
	if !strings.Contains(rig.log.String(), "one at a time") {
		t.Error("the log does not say the run fell back to shutting hosts down one at a time")
	}
}

// TestOffReportsTimingPerHost pins the summary that answers "why did that
// take so long". With hosts going down together, wall time is whichever host
// was slowest, and a log without per-host timings cannot say which that was.
func TestOffReportsTimingPerHost(t *testing.T) {
	rig := newOffTestRig(t, "f0", "f1", "f2", "f3")

	if err := rig.eng.OffAll(context.Background(), &rig.log); err != nil {
		t.Fatalf("power all off: %v", err)
	}

	for _, want := range []string{
		"Timing (total",
		"pre-flight checks",
		"shutdown batch",
		"shutdown f1",
		"shutdown f0",
		"confirm power-down",
		"<- longest",
	} {
		if !strings.Contains(rig.log.String(), want) {
			t.Errorf("the timing summary does not mention %q:\n%s", want, rig.log.String())
		}
	}
}

// TestSplitStorageMasterKeepsOrder pins that the split is a partition, not a
// filter: nothing may be dropped, and the batch keeps the order it arrived
// in.
func TestSplitStorageMasterKeepsOrder(t *testing.T) {
	hosts := []inventory.Host{{Name: "f1"}, {Name: "f2"}, {Name: "f3"}, {Name: "f0"}}

	batch, master := splitStorageMaster(hosts)

	if got := strings.Join(hostNames(batch), ","); got != "f1,f2,f3" {
		t.Errorf("batch = %s, want f1,f2,f3", got)
	}
	if len(master) != 1 || master[0].Name != inventory.StorageMaster {
		t.Errorf("master = %v, want exactly the storage master", hostNames(master))
	}
}

// TestSplitStorageMasterWithoutTheMaster pins the `power f3 off` shape: a run
// that does not include f0 has no second wave at all.
func TestSplitStorageMasterWithoutTheMaster(t *testing.T) {
	batch, master := splitStorageMaster([]inventory.Host{{Name: "f3"}})

	if len(batch) != 1 || batch[0].Name != "f3" {
		t.Errorf("batch = %v, want [f3]", hostNames(batch))
	}
	if len(master) != 0 {
		t.Errorf("master = %v, want none", hostNames(master))
	}
}
