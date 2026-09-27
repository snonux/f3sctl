package powerapi

import (
	"context"
	"errors"
	"testing"

	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/power"
)

// TestSnapshotRoundTrip pins the surface's own Slot: a Snapshot stored with
// WithSnapshot reads back whole through SnapshotOf, and storing it keeps the
// shared vocabulary (Job, PeerBusy) the State already carried.
func TestSnapshotRoundTrip(t *testing.T) {
	job := &coordination.Job{ID: "j1", State: coordination.JobRunning}
	errPlug := errors.New("plug unreachable")
	snap := Snapshot{
		Hosts: []power.HostStatus{{Name: "f0", Role: "f", Ping: true}},
		Fans:  power.FansState{On: true, IP: "192.0.2.1"},
		ACErr: errPlug,
	}

	s := WithSnapshot(contract.State{Job: job, PeerBusy: true}, snap)

	got := SnapshotOf(s)
	if len(got.Hosts) != 1 || got.Hosts[0].Name != "f0" || got.Fans != snap.Fans || !errors.Is(got.ACErr, errPlug) {
		t.Errorf("SnapshotOf = %+v, want %+v", got, snap)
	}
	if s.Job != job || !s.PeerBusy {
		t.Errorf("WithSnapshot dropped the shared state: Job = %v, PeerBusy = %v", s.Job, s.PeerBusy)
	}
}

// TestNoSnapshotReadsAsTheZeroSnapshot is the negative case: a State the
// probe never ran for -- a SkipsProbe route's -- carries no snapshot, and
// every power predicate sees an empty fleet (no host up, so no host action
// is offered) rather than some other request's.
func TestNoSnapshotReadsAsTheZeroSnapshot(t *testing.T) {
	var s contract.State
	if got := SnapshotOf(s); got.Hosts != nil || got.Fans != (power.FansState{}) || got.FansErr != nil || got.ACErr != nil {
		t.Errorf("SnapshotOf an unprobed state = %+v, want the zero Snapshot", got)
	}
	if _, ok := Host(s, "f0"); ok {
		t.Error("Host found f0 in a state no probe ran for")
	}
}

// TestPlugReadBackStaysInTheHandlersCopy pins that a plug switch records its
// read-back in its own copy of the state: the response shows the plug on,
// while the State the handler was handed -- which serve(), or another
// handler, may still hold -- keeps the snapshot it was judged on.
func TestPlugReadBackStaysInTheHandlersCopy(t *testing.T) {
	plug := newFakePlug(t)
	plug.on = false
	sf := testSurface(t, plug, nil)
	before := WithSnapshot(contract.State{}, Snapshot{Fans: power.FansState{On: false}})

	e, _, err := sf.handleFansOn(context.Background(), before, contract.Request{})
	if err != nil {
		t.Fatalf("handleFansOn: %v", err)
	}
	if on, _ := e.Properties["on"].(bool); !on {
		t.Errorf("fans-on response on = %v, want the read-back's true", e.Properties["on"])
	}
	if SnapshotOf(before).Fans.On {
		t.Error("fans-on wrote its read-back into the caller's State rather than its own copy")
	}
}
