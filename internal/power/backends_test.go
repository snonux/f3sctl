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
)

// This file exercises off()'s ordering and refusal logic purely against fake
// PowerBackend/NFSChecker/ZusbChecker implementations -- no ssh(1), no
// exec(2), no packet on the wire. Before backends.go these three stages of a
// shutdown could only be driven with a non-empty host list by actually
// SSHing somewhere (see the "What is NOT seamed is e.ssh" comment on
// testEngine, above); wiring the seams this refactor adds is what closes
// that gap.
//
// The fan plug still goes through fakeShelly (an httptest server), which
// predates this refactor and already exercises FansBackend's real adapter
// end to end; duplicating it behind a fake here would test less, not more.

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
	// seq, if set, gets a "poweroff:<host>" entry for every PowerOff call, so
	// a test can compare it against a fakeZusb sharing the same sequence.
	seq *sequence

	powerOffErr map[string]error
	// onPowerOff runs after a successful scripted PowerOff, before it
	// returns. Used to flip a host's fake liveness to "off" so
	// awaitPowerDown can confirm it went silent without a real ping.
	onPowerOff func(h inventory.Host)

	poweroffs []string
}

func (f *fakePower) Wake(inventory.Host) error { return nil }

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
