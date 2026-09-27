package contract

import (
	"context"
	"slices"
)

// Need is one request-scoped fact a route reads that is gathered only on
// demand, because it costs a round trip to another machine. See Route.Needs.
//
// A Need is declared by the package that owns the state it names, and
// compared by identity: NeedPeerBusy here, because State.PeerBusy is shared
// vocabulary; the Gogios mute and alert report by gogiosapi, which also keeps
// that state (see Slot). The same package supplies the Provider that fills
// it, so adding a domain's on-demand state touches neither this package nor
// the composition root's enrichState -- the root only runs whichever Fetch
// the matched route's Needs name.
type Need struct{ name string }

// NewNeed declares a Need. name is for diagnostics only; two Needs with the
// same name are still two different Needs.
func NewNeed(name string) *Need { return &Need{name: name} }

// String returns the name the Need was declared with.
func (n *Need) String() string {
	if n == nil {
		return "<nil Need>"
	}
	return n.name
}

// Needs is the set of Needs a route declares. The zero value declares none.
type Needs []*Need

// Has reports whether ns declares n.
func (ns Needs) Has(n *Need) bool { return slices.Contains(ns, n) }

// With returns ns plus n, as a set: ns itself when it already declares n,
// otherwise a new slice -- never one sharing ns's backing array, so two
// routes stamped from one declaration cannot end up sharing their Needs.
func (ns Needs) With(n *Need) Needs {
	if ns.Has(n) {
		return ns
	}
	return append(slices.Clip(ns), n)
}

// NeedPeerBusy is State.PeerBusy: whether the other API node is running a
// job -- an HTTP GET of the peer's /job, bounded by a 3s client timeout.
// Every power and plug action is judged on it (powerapi.JobRunning). It is
// declared here rather than by a surface because PeerBusy is shared
// vocabulary; the composition root, which owns the peer set, provides it.
var NeedPeerBusy = NewNeed("peer-busy")

// Fetch gathers one Need's state for the request req and returns s with it
// added. It must not drop anything else s carries: fetches for one route run
// one after another over the same State.
type Fetch func(ctx context.Context, s State, req Request) State

// Provider pairs a Need with the Fetch that fills it. The package owning the
// Need supplies it -- a surface through its Providers method -- and the
// composition root refuses to build a route table in which a declared Need
// has no Provider, or more than one.
type Provider struct {
	Need  *Need
	Fetch Fetch
}
