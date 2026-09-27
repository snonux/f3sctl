package powerapi

import (
	"context"

	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/power"
)

// Snapshot is the fleet state this surface judges every power and plug
// action on: one probe of each host and one read of each Shelly plug. This
// surface takes it itself (Surface.Probe, over its injected Prober) and keeps
// it in the request's contract.State under its own Slot, so the contract
// need not know any power type. The composition root decides only *whether*
// it is taken -- once per request, and not at all for a route declaring
// contract.Route.SkipsProbe.
type Snapshot struct {
	Hosts []power.HostStatus
	// Fans is the rack-fan plug; FansErr is set, and Fans zero, when it
	// could not be read.
	Fans    power.FansState
	FansErr error
	// AC is the f-host mains AC plug; ACErr is set, and AC zero, when it
	// could not be read.
	AC    power.ACState
	ACErr error
}

// snapshotSlot is where the Snapshot lives in a contract.State. Unexported:
// everything else goes through SnapshotOf and WithSnapshot.
var snapshotSlot = contract.NewSlot[Snapshot]("power snapshot")

// SnapshotOf returns the fleet snapshot s carries: the zero Snapshot (no
// hosts, both plugs read as off) when none was taken for this request.
func SnapshotOf(s contract.State) Snapshot { return snapshotSlot.Get(s) }

// WithSnapshot returns s carrying snap as its fleet snapshot.
//
// The copy is shallow, as every contract.State copy is: snap.Hosts is shared
// with whoever built snap, not cloned. To change a snapshot, build a new one
// and store it here -- never write into the Hosts of one already stored.
func WithSnapshot(s contract.State, snap Snapshot) contract.State {
	return snapshotSlot.With(s, snap)
}

// Prober is the slice of the power engine the Snapshot is read from: the
// fleet probe and the two plug reads. Satisfied by *power.Engine in
// production and by fakes in tests.
type Prober interface {
	// ProbeAll probes every host worth reporting: ~3s of concurrent
	// ping+TCP dials.
	ProbeAll(ctx context.Context) []power.HostStatus
	// FansStatus reads the rack-fan Shelly plug: an HTTP round trip bounded
	// by a 5s timeout.
	FansStatus(ctx context.Context) (power.FansState, error)
	// ACStatus reads the f-host mains AC Shelly plug, likewise.
	ACStatus(ctx context.Context) (power.ACState, error)
}

// Probe takes the fleet snapshot through this surface's Prober -- one host
// probe and one read of each plug -- and returns s carrying it. The
// composition root calls it once per request whose route is not
// contract.Route.SkipsProbe; everything else in the Snapshot's life (its
// type, its Slot, every read of it) is this surface's too.
func (sf *Surface) Probe(ctx context.Context, s contract.State) contract.State {
	var snap Snapshot
	snap.Hosts = sf.Prober.ProbeAll(ctx)
	snap.Fans, snap.FansErr = sf.Prober.FansStatus(ctx)
	snap.AC, snap.ACErr = sf.Prober.ACStatus(ctx)
	return WithSnapshot(s, snap)
}
