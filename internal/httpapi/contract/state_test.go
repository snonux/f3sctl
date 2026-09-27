package contract

import (
	"slices"
	"testing"
)

// TestSlotRoundTrip pins a Slot's reads: nothing stored reads as the zero
// value and not-found, a stored value reads back as stored -- including a
// stored zero value, which Lookup tells apart from nothing stored at all.
func TestSlotRoundTrip(t *testing.T) {
	k := NewSlot[[]string]("gateways")

	if got, ok := k.Lookup(State{}); got != nil || ok {
		t.Errorf("Lookup on an empty State = %v, %v; want nil, false", got, ok)
	}
	if got := k.Get(State{}); got != nil {
		t.Errorf("Get on an empty State = %v, want nil", got)
	}

	s := k.With(State{}, []string{"blowfish"})
	if got, ok := k.Lookup(s); !ok || !slices.Equal(got, []string{"blowfish"}) {
		t.Errorf("Lookup after With = %v, %v; want [blowfish], true", got, ok)
	}
	if _, ok := k.Lookup(k.With(State{}, nil)); !ok {
		t.Error("Lookup after With(nil) reports nothing stored: a stored zero value must still be found")
	}
}

// TestSlotsAreKeyedByIdentity is the negative half of Slot ownership: a
// value stored under one Slot is invisible through any other -- another Slot
// of the same type and even the same name included -- so no surface can read
// or clobber another's state by guessing its key.
func TestSlotsAreKeyedByIdentity(t *testing.T) {
	mine := NewSlot[int]("n")
	twin := NewSlot[int]("n")
	other := NewSlot[string]("n")

	s := mine.With(State{}, 42)
	if got, ok := twin.Lookup(s); ok {
		t.Errorf("a same-typed, same-named Slot read %v from another's state", got)
	}
	if got, ok := other.Lookup(s); ok {
		t.Errorf("a differently-typed Slot read %q from another's state", got)
	}

	s = twin.With(s, 7)
	if got := mine.Get(s); got != 42 {
		t.Errorf("storing under another Slot changed this one's value to %d, want 42", got)
	}
}

// TestSlotWithKeepsValueSemantics pins that With never writes through to the
// State it was given: a handler that stores a re-read into its own copy (the
// mute's read-back, the cache clear's re-fetch) must not change the state its
// caller -- or another handler -- is still holding.
func TestSlotWithKeepsValueSemantics(t *testing.T) {
	k := NewSlot[string]("k")
	other := NewSlot[int]("other")

	orig := other.With(k.With(State{PeerBusy: true}, "before"), 1)
	changed := k.With(orig, "after")

	if got := k.Get(orig); got != "before" {
		t.Errorf("the original State reads %q after With on a copy, want before", got)
	}
	if got := k.Get(changed); got != "after" {
		t.Errorf("the new State reads %q, want after", got)
	}
	if got := other.Get(changed); got != 1 || !changed.PeerBusy {
		t.Errorf("With dropped the rest of the State: other = %d, PeerBusy = %v", got, changed.PeerBusy)
	}
}

// TestSlotStoresANilInterface pins Lookup on an interface-typed Slot: a nil
// stored there -- a fetch that succeeded, recording "no error" -- is still
// stored, and must not read back as nothing collected at all.
func TestSlotStoresANilInterface(t *testing.T) {
	k := NewSlot[error]("err")

	if _, ok := k.Lookup(State{}); ok {
		t.Error("Lookup on an empty State found an error slot")
	}
	got, ok := k.Lookup(k.With(State{}, nil))
	if !ok || got != nil {
		t.Errorf("Lookup after With(nil) = %v, %v; want nil, true", got, ok)
	}
}
