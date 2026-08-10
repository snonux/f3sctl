package power

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/inventory"
)

// This file exercises off()'s and on()'s ordering and refusal logic purely
// against fake PowerBackend/FansBackend/NFSChecker/ZusbChecker
// implementations -- no ssh(1), no exec(2), no packet on the wire. Before
// backends.go these stages of a shutdown or a wake could only be driven with
// a non-empty host list by actually SSHing somewhere (see the "What is NOT
// seamed is e.ssh" comment on testEngine, above); wiring the seams this
// refactor adds is what closes that gap.
//
// off()'s tests still send the fan plug through fakeShelly (an httptest
// server), which predates this refactor and already exercises FansBackend's
// real adapter end to end for the one call off() makes; duplicating it behind
// a fake here would test less, not more. on()'s tests use fakeFans instead,
// because they need a second, failing call as well (see fakeFans' own doc).

// sequence records the order calls land in across more than one fake, so a
// test can assert that the zusb pre-flight runs to completion before any
// host is powered off, not merely that both happened.
type sequence struct {
	mu  sync.Mutex
	log []string
}

func (s *sequence) add(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, step)
}

func (s *sequence) get() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.log...)
}

// fakePower fakes PowerBackend: it records every call and lets a test script
// a failure for a named host without any SSH client existing at all.
type fakePower struct {
	mu sync.Mutex
	// seq, if set, gets a "poweroff:<host>" and "wake:<host>" entry for every
	// PowerOff/Wake call, so a test can compare it against a fakeZusb or
	// fakeFans sharing the same sequence.
	seq *sequence

	powerOffErr map[string]error
	// onPowerOff runs after a successful scripted PowerOff, before it
	// returns. Used to flip a host's fake liveness to "off" so
	// awaitPowerDown can confirm it went silent without a real ping.
	onPowerOff func(h inventory.Host)
	// wakeErr scripts a Wake failure for a named host, the same way
	// powerOffErr does for PowerOff -- used by on()'s abort tests to prove a
	// magic packet that fails mid-sequence stops the rest of the hosts from
	// being woken.
	wakeErr map[string]error

	poweroffs []string
	wakes     []string
}

func (f *fakePower) Wake(h inventory.Host) error {
	f.mu.Lock()
	f.wakes = append(f.wakes, h.Name)
	err := f.wakeErr[h.Name]
	f.mu.Unlock()

	if f.seq != nil {
		f.seq.add("wake:" + h.Name)
	}
	return err
}

// wakeCalls returns the hosts Wake was called for, in order.
func (f *fakePower) wakeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.wakes...)
}

func (f *fakePower) AgentVerb(context.Context, inventory.Host, string) (string, error) {
	return "", nil
}

func (f *fakePower) PowerOff(_ context.Context, h inventory.Host) (out, diag string, err error) {
	f.mu.Lock()
	f.poweroffs = append(f.poweroffs, h.Name)
	err = f.powerOffErr[h.Name]
	f.mu.Unlock()

	if f.seq != nil {
		f.seq.add("poweroff:" + h.Name)
	}
	if err == nil && f.onPowerOff != nil {
		f.onPowerOff(h)
	}
	return "", "", err
}

func (f *fakePower) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.poweroffs...)
}

// fakeFans fakes FansBackend. Set can be scripted to fail once, the same
// shape as fakePower.powerOffErr, so on()'s abort test can prove a plug that
// refuses to switch on stops the wake sequence before a single magic packet
// goes out -- without a Shelly plug or an httptest server involved at all.
//
// off()'s tests reuse fakeShelly (an httptest server) instead, because that
// already exercises FansBackend's real HTTP adapter end to end for the one
// call off() makes; on()'s tests need a second, failing call as well, which a
// plain Go fake scripts far more directly than a second HTTP fixture would.
type fakeFans struct {
	mu  sync.Mutex
	seq *sequence

	setErr error

	state bool
	calls []bool // the requested state of every Set call, in order
}

func (f *fakeFans) Status(context.Context) (FansState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return FansState{On: f.state}, nil
}

func (f *fakeFans) Set(_ context.Context, on bool) (FansState, error) {
	f.mu.Lock()
	f.calls = append(f.calls, on)
	err := f.setErr
	if err == nil {
		f.state = on
	}
	st := FansState{On: f.state}
	f.mu.Unlock()

	if f.seq != nil {
		f.seq.add("fans:" + strconv.FormatBool(on))
	}
	return st, err
}

// setCalls returns the state requested by each Set call, in order.
func (f *fakeFans) setCalls() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.calls...)
}

// fakeZusb fakes ZusbChecker. Every host reports "unloaded" unless named in
// loaded; Unload can be scripted to fail per host.
type fakeZusb struct {
	mu  sync.Mutex
	seq *sequence

	loaded    map[string]bool
	unloadErr map[string]error

	statusCalls []string
	unloadCalls []string
}

func (z *fakeZusb) Status(_ context.Context, h inventory.Host) (string, error) {
	z.mu.Lock()
	z.statusCalls = append(z.statusCalls, h.Name)
	loaded := z.loaded[h.Name]
	z.mu.Unlock()

	if z.seq != nil {
		z.seq.add("zusb-status:" + h.Name)
	}
	if loaded {
		return "loaded", nil
	}
	return "unloaded", nil
}

func (z *fakeZusb) Unload(_ context.Context, h inventory.Host) (string, error) {
	z.mu.Lock()
	z.unloadCalls = append(z.unloadCalls, h.Name)
	err := z.unloadErr[h.Name]
	z.mu.Unlock()

	if z.seq != nil {
		z.seq.add("zusb-unload:" + h.Name)
	}
	return "", err
}

// fakeNFS fakes NFSChecker's Unmount half. Mounts is implemented only so
// fakeNFS satisfies the interface -- checkLocalNFS lists what is mounted
// through Engine.localMounts/nfsMounts, the pre-existing seam (see
// NFSChecker's doc in backends.go), not through here.
type fakeNFS struct {
	mu         sync.Mutex
	unmountErr map[string]error
	calls      []string
}

func (n *fakeNFS) Mounts(context.Context) ([]string, error) { return nil, nil }

func (n *fakeNFS) Unmount(_ context.Context, mountpoint string) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, mountpoint)
	if err, ok := n.unmountErr[mountpoint]; ok {
		return "device or resource busy", err
	}
	return "", nil
}

// TestOffRunsThroughFakeBackendsEndToEnd drives a full shutdown -- NFS
// pre-flight, zusb pre-flight, poweroff, confirmation, fans off -- with none
// of PowerBackend, ZusbChecker or the fan plug's mechanism real. This is the
// path testEngine's own doc says was previously unreachable without a real
// SSH client.
func TestOffRunsThroughFakeBackendsEndToEnd(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly, "f1")
	eng.downProbeInterval = time.Millisecond

	f1IP := hostIP(t, eng, "f1")
	var mu sync.Mutex
	live := map[string]bool{f1IP: true}
	eng.isUp = func(_ context.Context, ip string) (bool, bool) {
		mu.Lock()
		defer mu.Unlock()
		return live[ip], true
	}

	zusb := &fakeZusb{}
	eng.zusb = zusb

	power := &fakePower{
		onPowerOff: func(h inventory.Host) {
			mu.Lock()
			live[h.IP] = false
			mu.Unlock()
		},
	}
	eng.power = power

	var log bytes.Buffer
	if err := eng.Off(context.Background(), &log); err != nil {
		t.Fatalf("power off: %v", err)
	}

	if got := power.calls(); len(got) != 1 || got[0] != "f1" {
		t.Errorf("PowerOff calls = %v, want exactly [f1]", got)
	}
	if got := zusb.statusCalls; len(got) != 1 || got[0] != "f1" {
		t.Errorf("zusb-status calls = %v, want exactly [f1]", got)
	}
	if len(zusb.unloadCalls) != 0 {
		t.Errorf("zusb-unload calls = %v, want none: nothing reported the pool loaded", zusb.unloadCalls)
	}
	if got := shelly.setCalls(); len(got) != 1 || got[0] {
		t.Fatalf("Switch.Set calls = %v, want exactly one with on=false: the rack went idle", got)
	}
}

// TestOffExportsZusbBeforeAnyHostIsPoweredOff pins the pre-flight ordering
// off()'s own doc promises: zusb is exported everywhere it is held before
// the first poweroff is sent, not interleaved with it.
func TestOffExportsZusbBeforeAnyHostIsPoweredOff(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly, "f0", "f1", "f2")
	eng.downProbeInterval = time.Millisecond

	var mu sync.Mutex
	live := map[string]bool{}
	for _, name := range []string{"f0", "f1", "f2"} {
		live[hostIP(t, eng, name)] = true
	}
	eng.isUp = func(_ context.Context, ip string) (bool, bool) {
		mu.Lock()
		defer mu.Unlock()
		return live[ip], true
	}

	seq := &sequence{}
	zusb := &fakeZusb{seq: seq, loaded: map[string]bool{"f1": true}}
	eng.zusb = zusb
	power := &fakePower{
		seq: seq,
		onPowerOff: func(h inventory.Host) {
			mu.Lock()
			live[h.IP] = false
			mu.Unlock()
		},
	}
	eng.power = power

	var log bytes.Buffer
	if err := eng.Off(context.Background(), &log); err != nil {
		t.Fatalf("power off: %v", err)
	}

	if got := zusb.unloadCalls; len(got) != 1 || got[0] != "f1" {
		t.Fatalf("zusb-unload calls = %v, want exactly [f1]: only f1 reported the pool loaded", got)
	}

	got := seq.get()
	firstPoweroff := len(got)
	for i, step := range got {
		if strings.HasPrefix(step, "poweroff:") {
			firstPoweroff = i
			break
		}
	}
	for i, step := range got {
		if i >= firstPoweroff {
			break
		}
		if strings.HasPrefix(step, "poweroff:") {
			t.Fatalf("sequence = %v, want no poweroff before the zusb pre-flight finished", got)
		}
	}
	for _, step := range got[firstPoweroff:] {
		if strings.HasPrefix(step, "zusb-") {
			t.Fatalf("sequence = %v, want no zusb call after the first poweroff", got)
		}
	}
}

// TestOffAbortsWhenNFSWontUnmount is the negative test for the cheapest
// possible place to stop: a local NFS mount that will not let go must abort
// the whole shutdown before the zusb pre-flight or any host is touched.
func TestOffAbortsWhenNFSWontUnmount(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly, "f0")
	eng.nfsMounts = func(context.Context) ([]string, error) {
		return []string{"/mnt/nfs"}, nil
	}
	eng.nfs = &fakeNFS{unmountErr: map[string]error{"/mnt/nfs": errors.New("device is busy")}}

	zusb := &fakeZusb{}
	eng.zusb = zusb
	power := &fakePower{}
	eng.power = power

	var log bytes.Buffer
	err := eng.Off(context.Background(), &log)
	if err == nil {
		t.Fatal("power off succeeded, want the stuck NFS mount to abort it")
	}
	if !strings.Contains(err.Error(), "/mnt/nfs") {
		t.Errorf("error = %v, want it to name the stuck mount", err)
	}
	if len(zusb.statusCalls) != 0 {
		t.Errorf("zusb pre-flight ran = %v, want none: the run must abort before it", zusb.statusCalls)
	}
	if got := power.calls(); len(got) != 0 {
		t.Errorf("PowerOff calls = %v, want none: the rack was never reached", got)
	}
	if got := shelly.setCalls(); len(got) != 0 {
		t.Errorf("Switch.Set calls = %v, want none: the rack was never reached", got)
	}
}

// TestOffAbortsWhenZusbExportFails is the negative test for the second
// pre-flight stage: a pool that will not export must abort before any
// poweroff is sent, so the rack is never touched with the pool still held by
// a host about to lose USB power mid-write.
func TestOffAbortsWhenZusbExportFails(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly, "f0")
	eng.zusb = &fakeZusb{
		loaded:    map[string]bool{"f0": true},
		unloadErr: map[string]error{"f0": errors.New("zusb-unload: zpool export failed")},
	}
	power := &fakePower{}
	eng.power = power

	var log bytes.Buffer
	err := eng.Off(context.Background(), &log)
	if err == nil {
		t.Fatal("power off succeeded, want the failed zusb export to abort it")
	}
	if !strings.Contains(err.Error(), "f0") {
		t.Errorf("error = %v, want it to name the host", err)
	}
	if got := power.calls(); len(got) != 0 {
		t.Errorf("PowerOff calls = %v, want none: aborted before any poweroff", got)
	}
	if got := shelly.setCalls(); len(got) != 0 {
		t.Errorf("Switch.Set calls = %v, want none: the rack was never touched", got)
	}
}

// TestOffReportsPowerOffFailureAndLeavesFansOn is the negative test for the
// last mechanism stage: a host that refuses the poweroff itself must fail the
// run and, just as importantly, must never reach the fans-off step.
func TestOffReportsPowerOffFailureAndLeavesFansOn(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly, "f0")
	eng.zusb = &fakeZusb{}
	eng.power = &fakePower{
		powerOffErr: map[string]error{"f0": errors.New("ssh: connect to host 192.168.1.130 port 22: Operation timed out")},
	}

	var log bytes.Buffer
	err := eng.Off(context.Background(), &log)
	if err == nil {
		t.Fatal("power off succeeded, want the PowerOff failure on f0 to fail the run")
	}
	if !strings.Contains(err.Error(), "f0") {
		t.Errorf("error = %v, want it to name f0", err)
	}
	if !strings.Contains(err.Error(), fansLeftOn) {
		t.Errorf("error = %v, want it to say %q", err, fansLeftOn)
	}
	if got := shelly.setCalls(); len(got) != 0 {
		t.Fatalf("Switch.Set calls = %v, want none: a failed shutdown must not touch the fans", got)
	}
}

// TestOnRunsThroughFakeBackendsEndToEnd drives a full wake -- fans on, then a
// magic packet to every PowerGroup host -- with neither the fan plug nor
// PowerBackend's Wake mechanism real. This is on()'s half of the gap
// TestOffRunsThroughFakeBackendsEndToEnd closes for off(): before backends.go
// this ordering could only be observed against a real Shelly plug and real
// NICs on the wire.
func TestOnRunsThroughFakeBackendsEndToEnd(t *testing.T) {
	shelly := newFakeShelly(t, false)
	eng := testEngine(t, shelly)

	seq := &sequence{}
	fans := &fakeFans{seq: seq}
	eng.fans = fans
	power := &fakePower{seq: seq}
	eng.power = power

	var log bytes.Buffer
	if err := eng.On(context.Background(), &log); err != nil {
		t.Fatalf("power on: %v", err)
	}

	if got := fans.setCalls(); len(got) != 1 || !got[0] {
		t.Fatalf("fans Set calls = %v, want exactly one with on=true", got)
	}
	if got := power.wakeCalls(); len(got) != 3 || got[0] != "f0" || got[1] != "f1" || got[2] != "f2" {
		t.Fatalf("Wake calls = %v, want exactly [f0 f1 f2], PowerGroup order", got)
	}

	// The fans must be switched on before the first magic packet goes out --
	// On's own doc says waking hosts with no cooling running is worse than
	// leaving them off.
	got := seq.get()
	if len(got) == 0 || got[0] != "fans:true" {
		t.Fatalf("sequence = %v, want it to start with fans:true", got)
	}
	for _, step := range got {
		if strings.HasPrefix(step, "wake:") {
			break
		}
		if step == "fans:true" {
			continue
		}
		t.Fatalf("sequence = %v, want nothing but fans:true before the first wake", got)
	}
}

// TestOnAbortsWhenFansWontSwitchOn is the negative test for on()'s first
// stage: a plug that refuses to switch on must stop the whole wake before a
// single magic packet is sent, the same way a stuck NFS mount stops off()
// before the zusb pre-flight.
func TestOnAbortsWhenFansWontSwitchOn(t *testing.T) {
	shelly := newFakeShelly(t, false)
	eng := testEngine(t, shelly)

	eng.fans = &fakeFans{setErr: errors.New("shelly plug did not change state")}
	power := &fakePower{}
	eng.power = power

	var log bytes.Buffer
	err := eng.On(context.Background(), &log)
	if err == nil {
		t.Fatal("power on succeeded, want the failed fan switch to abort it")
	}
	if !strings.Contains(err.Error(), "fans off") {
		t.Errorf("error = %v, want it to say why: refusing to wake hosts with the fans off", err)
	}
	if got := power.wakeCalls(); len(got) != 0 {
		t.Errorf("Wake calls = %v, want none: no host is woken with no cooling running", got)
	}
}

// TestOnAbortsWhenWakeFails is the negative test for on()'s second stage: a
// host that refuses its magic packet must stop the sequence before any host
// after it in the list is woken, and must surface the failure rather than
// carry on quietly.
func TestOnAbortsWhenWakeFails(t *testing.T) {
	shelly := newFakeShelly(t, false)
	eng := testEngine(t, shelly)

	eng.fans = &fakeFans{}
	power := &fakePower{
		wakeErr: map[string]error{"f1": errors.New("sending magic packet to f1: network is unreachable")},
	}
	eng.power = power

	var log bytes.Buffer
	err := eng.On(context.Background(), &log)
	if err == nil {
		t.Fatal("power on succeeded, want the failed Wake on f1 to abort it")
	}
	if !strings.Contains(err.Error(), "f1") {
		t.Errorf("error = %v, want it to name f1", err)
	}
	if got := power.wakeCalls(); len(got) != 2 || got[0] != "f0" || got[1] != "f1" {
		t.Fatalf("Wake calls = %v, want exactly [f0 f1]: f2 must never be reached "+
			"once f1's magic packet failed", got)
	}
}

// TestNFSBackendMountsUsesTheSameSeamAsCheckLocalNFS is the regression test
// for oz0: execNFS.Mounts used to call the free function localNFSMounts
// directly instead of going through Engine.localMounts/nfsMounts, the seam
// checkLocalNFS itself is built on. A test that stubbed nfsMounts but not nfs
// would then have nfsBackend().Mounts() silently read the real mount table of
// whatever machine ran the test instead of the fake. Nothing in production
// calls nfsBackend().Mounts() today (see NFSChecker's doc in backends.go), so
// this is the only thing that would have caught the drift.
func TestNFSBackendMountsUsesTheSameSeamAsCheckLocalNFS(t *testing.T) {
	e := &Engine{
		nfsMounts: func(context.Context) ([]string, error) {
			return []string{"/fake/mount"}, nil
		},
	}

	got, err := e.nfsBackend().Mounts(context.Background())
	if err != nil {
		t.Fatalf("nfsBackend().Mounts: %v", err)
	}
	if len(got) != 1 || got[0] != "/fake/mount" {
		t.Fatalf("Mounts = %v, want [/fake/mount] from the stubbed nfsMounts field, "+
			"not whatever is really mounted on the test box", got)
	}
}

// trackedLiveness wires eng.isUp to a map seeded true for upHosts, and
// returns the onPowerOff callback fakePower needs to flip a host silent once
// its scripted PowerOff succeeds -- the mechanism awaitPowerDown depends on to
// confirm a shutdown actually completed, without a real ping. Driving
// Off/OffAll all the way through awaitPowerDown needs this exact wiring;
// TestOffRunsThroughFakeBackendsEndToEnd and
// TestOffExportsZusbBeforeAnyHostIsPoweredOff still inline their own copy of
// it rather than calling this -- worth consolidating onto this helper next
// time either of them is touched, but not redone here to keep this change
// small.
func trackedLiveness(t *testing.T, eng *Engine, upHosts ...string) func(inventory.Host) {
	t.Helper()
	var mu sync.Mutex
	live := map[string]bool{}
	for _, name := range upHosts {
		live[hostIP(t, eng, name)] = true
	}
	eng.isUp = func(_ context.Context, ip string) (bool, bool) {
		mu.Lock()
		defer mu.Unlock()
		return live[ip], true
	}
	return func(h inventory.Host) {
		mu.Lock()
		live[h.IP] = false
		mu.Unlock()
	}
}

// TestOffProceedsWhenNFSWasAlreadyUnmountedByTheRace is pz0's test:
// checkLocalNFS's post-failure re-check ("it may have gone away between
// listing and unmounting") must let the run continue when a second listing no
// longer shows the mountpoint, rather than treat the failed Unmount as fatal.
//
// nfsMounts is scripted to answer differently on its two calls: the initial
// listing (which finds the mount and drives the Unmount attempt) and the
// re-check after Unmount fails (which finds it already gone) both go through
// this same field, per Engine.localMounts' doc -- so the race and its
// resolution are both driven through the one seam checkLocalNFS actually
// uses, not two independent fakes that could disagree.
func TestOffProceedsWhenNFSWasAlreadyUnmountedByTheRace(t *testing.T) {
	shelly := newFakeShelly(t, true)
	eng := testEngine(t, shelly, "f0")

	var listCalls int
	eng.nfsMounts = func(context.Context) ([]string, error) {
		listCalls++
		if listCalls == 1 {
			return []string{"/mnt/nfs"}, nil
		}
		// The re-check: something else unmounted it in the meantime.
		return nil, nil
	}
	eng.nfs = &fakeNFS{unmountErr: map[string]error{"/mnt/nfs": errors.New("device is busy")}}

	zusb := &fakeZusb{}
	eng.zusb = zusb
	power := &fakePower{onPowerOff: trackedLiveness(t, eng, "f0")}
	eng.power = power

	var log bytes.Buffer
	if err := eng.Off(context.Background(), &log); err != nil {
		t.Fatalf("power off: %v", err)
	}

	if listCalls != 2 {
		t.Fatalf("nfsMounts called %d times, want 2: the initial listing and the post-failure re-check", listCalls)
	}
	if !strings.Contains(log.String(), "already unmounted") {
		t.Errorf("log = %q, want it to say /mnt/nfs was already unmounted", log.String())
	}
	if got := zusb.statusCalls; len(got) != 1 || got[0] != "f0" {
		t.Errorf("zusb-status calls = %v, want exactly [f0]: the run must proceed past the NFS check", got)
	}
	if got := power.calls(); len(got) != 1 || got[0] != "f0" {
		t.Errorf("PowerOff calls = %v, want exactly [f0]: the run must proceed to power off", got)
	}
}
