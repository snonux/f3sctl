package contract

import (
	"slices"
	"testing"
)

// TestNeedsHas pins Needs as a set of Need identities: Has is true only for a
// Need actually declared, the zero value -- a route declaring nothing -- has
// none, and a second Need of the same name is still a different Need, so two
// packages cannot collide by picking one name.
func TestNeedsHas(t *testing.T) {
	a, b := NewNeed("a"), NewNeed("b")
	twin := NewNeed("a")
	both := Needs{a, b}
	for _, tc := range []struct {
		name string
		set  Needs
		x    *Need
		want bool
	}{
		{"declared first", both, a, true},
		{"declared second", both, b, true},
		{"undeclared", both, NeedPeerBusy, false},
		{"same name, different Need", both, twin, false},
		{"zero value", Route{}.Needs, NeedPeerBusy, false},
		{"nil Need", both, nil, false},
	} {
		if got := tc.set.Has(tc.x); got != tc.want {
			t.Errorf("%s: Needs%v.Has(%v) = %v, want %v", tc.name, tc.set, tc.x, got, tc.want)
		}
	}
}

// TestNeedsWithIsASetAndNeverAliases pins Needs.With, the stamp powerapi
// adds NeedPeerBusy to every job route with: adding a Need already declared
// changes nothing, and adding a new one never writes into the backing array
// of the slice it was called on -- two routes stamped from one declaration
// must not end up sharing (and later clobbering) each other's Needs.
func TestNeedsWithIsASetAndNeverAliases(t *testing.T) {
	a, b, c := NewNeed("a"), NewNeed("b"), NewNeed("c")

	if got := (Needs{a}).With(a); !slices.Equal(got, Needs{a}) {
		t.Errorf("Needs{a}.With(a) = %v, want [a]", got)
	}
	if got := Needs(nil).With(a); !slices.Equal(got, Needs{a}) {
		t.Errorf("Needs(nil).With(a) = %v, want [a]", got)
	}

	base := make(Needs, 1, 4) // spare capacity an append would reuse
	base[0] = a
	withB, withC := base.With(b), base.With(c)
	if !slices.Equal(withB, Needs{a, b}) || !slices.Equal(withC, Needs{a, c}) {
		t.Errorf("With over one base = %v and %v, want [a b] and [a c]: they share a backing array", withB, withC)
	}
}

// TestNeedString names a Need for diagnostics, and does not panic on nil --
// the build error reporting a route's nil Need prints it.
func TestNeedString(t *testing.T) {
	if got := NeedPeerBusy.String(); got != "peer-busy" {
		t.Errorf("NeedPeerBusy.String() = %q, want peer-busy", got)
	}
	if got := (*Need)(nil).String(); got != "<nil Need>" {
		t.Errorf("(*Need)(nil).String() = %q", got)
	}
}
