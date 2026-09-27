package contract

import (
	"maps"

	"github.com/snonux/f3sctl/internal/coordination"
)

// State is a snapshot of the world, taken once per request before anything is
// rendered.
//
// Availability predicates read only from here, never from live probes of their
// own, so every action in a single response is judged against the same instant.
//
// Its exported fields are the vocabulary every surface shares: this node's
// job, and whether the peer node is busy. Anything owned by a single domain
// -- the power surface's fleet snapshot, the Gogios mute and alert report --
// is not a field here at all: the surface owning it keeps it under a Slot of
// its own, with its own typed accessors, so this package need not know the
// domain's types and adding a domain does not edit this struct.
type State struct {
	// Job is this node's own job, read from local disk for every request.
	// Every power action is judged on it (with PeerBusy), and /job and
	// /status render it.
	Job *coordination.Job

	// PeerBusy reports whether the *other* API node is running a job.
	//
	// relayd load-balances pi0 and pi1, so a job started on one node is
	// invisible to the other's local state. Without this, the idle node cheerfully
	// advertises power-off while the busy node is mid-shutdown, and every one of
	// those actions 409s the instant a client tries it -- which is exactly the
	// "read the 409, not the response" behaviour this API exists to avoid.
	// Collected only for routes declaring NeedPeerBusy (see Route.Needs).
	PeerBusy bool

	// domains is every surface's own state, keyed by the *Slot it was stored
	// under. Only Slot reads and writes it, copy-on-write, so a State keeps
	// the value semantics its handlers rely on: a handler that stores a
	// re-read into its copy never changes the caller's.
	domains map[any]any
}

// Slot is a typed key under which one surface keeps its own domain state in
// a State. The surface declares it unexported and exposes typed accessors
// over it, so nothing but the owner can read or write that state -- and
// nothing here needs to know its type.
//
// Slots are compared by identity: two Slots of the same type and name are
// still two different keys.
type Slot[T any] struct{ name string }

// NewSlot declares a Slot. name is for diagnostics only.
func NewSlot[T any](name string) *Slot[T] { return &Slot[T]{name: name} }

// String returns the name the Slot was declared with.
func (k *Slot[T]) String() string { return k.name }

// Lookup returns the value stored under k in s, and whether one was stored.
// A stored nil -- of an interface or pointer T -- is still stored: it reads
// back as nil, true, not as nothing stored.
func (k *Slot[T]) Lookup(s State) (T, bool) {
	v, ok := s.domains[k]
	if !ok {
		var zero T
		return zero, false
	}
	t, _ := v.(T) // only With stores under k, so v is a T -- or a nil one
	return t, true
}

// Get returns the value stored under k in s, or T's zero value when none was
// -- the same "not collected for this request" a nil field used to mean.
func (k *Slot[T]) Get(s State) T {
	v, _ := k.Lookup(s)
	return v
}

// With returns s with v stored under k. s itself is left unchanged: the
// domain map is copied rather than written through, so a State passed by
// value never aliases another's domain state.
//
// The copy is shallow: v itself is stored as given, so a slice or pointer
// inside it is shared with every State it was stored in. A surface changes
// its state by storing a new value, never by writing through one it read.
func (k *Slot[T]) With(s State, v T) State {
	m := make(map[any]any, len(s.domains)+1)
	maps.Copy(m, s.domains)
	m[k] = v
	s.domains = m
	return s
}
