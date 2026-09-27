package powerapi

import (
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/power"
)

// Snapshot is the fleet state this surface judges every power and plug
// action on: one probe of each host and one read of each Shelly plug, taken
// once per request by the composition root's snapshot() -- skipped entirely
// for a route declaring contract.Route.SkipsProbe -- and kept in the
// request's contract.State under this surface's own Slot, so the contract
// need not know any power type.
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
func WithSnapshot(s contract.State, snap Snapshot) contract.State {
	return snapshotSlot.With(s, snap)
}
