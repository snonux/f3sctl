package coordination

import (
	"errors"
	"net"
	"testing"
)

// fetchCall records one invocation of a fake PeerSet.fetch, so tests can
// assert both what was asked and what was skipped.
type fetchCall struct {
	addr, path, apiKey string
}

// fakeFetcher returns a PeerSet.fetch stub driven by a map from address to
// canned (job, error) results, plus the slice of calls actually made.
func fakeFetcher(byAddr map[string]struct {
	job *peerJob
	err error
}) (func(addr, path, apiKey string) (*peerJob, error), *[]fetchCall) {
	calls := &[]fetchCall{}
	fn := func(addr, path, apiKey string) (*peerJob, error) {
		*calls = append(*calls, fetchCall{addr, path, apiKey})
		r, ok := byAddr[addr]
		if !ok {
			return nil, errors.New("no fixture for " + addr)
		}
		return r.job, r.err
	}
	return fn, calls
}

// TestPeerSetBusyDetectsPeerRunning is the core positive case: the other node
// reports a job in flight, so Busy must say so and name it.
func TestPeerSetBusyDetectsPeerRunning(t *testing.T) {
	fetch, calls := fakeFetcher(map[string]struct {
		job *peerJob
		err error
	}{
		"192.168.1.126": {job: &peerJob{State: "running", Node: "pi1"}},
	})
	ps := &PeerSet{Nodes: []string{"192.168.1.126"}, JobPath: "/cgi-bin/f3sctl/job", fetch: fetch}

	busy, node := ps.Busy("pi0", "sekrit")
	if !busy {
		t.Fatal("Busy() = false, want true: the peer reported a running job")
	}
	if node != "pi1" {
		t.Errorf("node = %q, want %q", node, "pi1")
	}
	if len(*calls) != 1 || (*calls)[0].apiKey != "sekrit" || (*calls)[0].path != "/cgi-bin/f3sctl/job" {
		t.Errorf("fetch called with %+v, want the api key and job path forwarded", *calls)
	}
}

// TestPeerSetBusyReturnsFalseWhenPeerIsIdle is the companion negative case:
// nothing running anywhere must not make Busy report a false positive.
func TestPeerSetBusyReturnsFalseWhenPeerIsIdle(t *testing.T) {
	fetch, _ := fakeFetcher(map[string]struct {
		job *peerJob
		err error
	}{
		"192.168.1.126": {job: &peerJob{State: "done", Node: "pi1"}},
	})
	ps := &PeerSet{Nodes: []string{"192.168.1.126"}, JobPath: "/job", fetch: fetch}

	if busy, node := ps.Busy("pi0", "k"); busy {
		t.Errorf("Busy() = (true, %q), want false: the peer's job is done, not running", node)
	}
}

// TestPeerSetBusyTreatsAnUnreachablePeerAsIdle pins the deliberate failure
// mode documented on PeerSet.Busy: a peer that cannot be reached must not
// block power actions, because the peers ARE the machines serving this API,
// and refusing to act because one is down defeats the tool's purpose.
func TestPeerSetBusyTreatsAnUnreachablePeerAsIdle(t *testing.T) {
	fetch, calls := fakeFetcher(map[string]struct {
		job *peerJob
		err error
	}{
		"192.168.1.126": {err: errors.New("connection refused")},
	})
	ps := &PeerSet{Nodes: []string{"192.168.1.126"}, JobPath: "/job", fetch: fetch}

	busy, _ := ps.Busy("pi0", "k")
	if busy {
		t.Error("Busy() = true, want false: an unreachable peer must count as idle")
	}
	if len(*calls) != 1 {
		t.Errorf("fetch called %d times, want exactly 1", len(*calls))
	}
}

// TestPeerSetBusyChecksEveryNodeUntilOneIsBusy pins that a set with more than
// two nodes does not stop at the first idle/unreachable answer.
func TestPeerSetBusyChecksEveryNodeUntilOneIsBusy(t *testing.T) {
	fetch, calls := fakeFetcher(map[string]struct {
		job *peerJob
		err error
	}{
		"10.0.0.1": {err: errors.New("unreachable")},
		"10.0.0.2": {job: &peerJob{State: "done"}},
		"10.0.0.3": {job: &peerJob{State: "running", Node: "third"}},
	})
	ps := &PeerSet{Nodes: []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, JobPath: "/job", fetch: fetch}

	busy, node := ps.Busy("pi0", "k")
	if !busy || node != "third" {
		t.Errorf("Busy() = (%v, %q), want (true, \"third\")", busy, node)
	}
	if len(*calls) != 3 {
		t.Errorf("fetch called %d times, want all 3 nodes consulted", len(*calls))
	}
}

// TestPeerSetBusySkipsItselfByInterfaceAddress pins the half of self-skipping
// that matters off the Pis: an address that matches one of this machine's own
// interfaces must never be asked, or a node could report itself as its own
// busy peer.
func TestPeerSetBusySkipsItselfByInterfaceAddress(t *testing.T) {
	fetch, calls := fakeFetcher(map[string]struct {
		job *peerJob
		err error
	}{
		"10.0.0.9": {job: &peerJob{State: "running", Node: "other"}},
	})
	ps := &PeerSet{
		Nodes:   []string{"127.0.0.1", "10.0.0.9"},
		JobPath: "/job",
		fetch:   fetch,
		localAddrs: func() ([]net.Addr, error) {
			// Mirrors what net.InterfaceAddrs actually returns: the IPNet's
			// IP field is the interface's own address, not the masked
			// network address net.ParseCIDR would give back.
			return []net.Addr{&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)}}, nil
		},
	}

	busy, node := ps.Busy("earth", "k")
	if !busy || node != "other" {
		t.Fatalf("Busy() = (%v, %q), want (true, \"other\")", busy, node)
	}
	for _, c := range *calls {
		if c.addr == "127.0.0.1" {
			t.Error("fetch was called for this machine's own address")
		}
	}
}

// TestPeerSetBusySkipsItselfByHostnameLookup pins the pi0/pi1-specific path:
// on the Pis, isSelf resolves the local short hostname and compares it to
// each candidate address, since a Pi does not necessarily see its own IP in
// InterfaceAddrs the way the peer list expects.
func TestPeerSetBusySkipsItselfByHostnameLookup(t *testing.T) {
	fetch, calls := fakeFetcher(map[string]struct {
		job *peerJob
		err error
	}{
		"192.168.1.126": {job: &peerJob{State: "running", Node: "pi1"}},
	})
	ps := &PeerSet{
		Nodes:   []string{"192.168.1.125", "192.168.1.126"},
		JobPath: "/job",
		fetch:   fetch,
		lookupHost: func(host string) ([]string, error) {
			if host == "pi0" {
				return []string{"192.168.1.125"}, nil
			}
			return nil, errors.New("no such host")
		},
		localAddrs: func() ([]net.Addr, error) { return nil, nil },
	}

	busy, node := ps.Busy("pi0.mesh.internal", "k")
	if !busy || node != "pi1" {
		t.Fatalf("Busy() = (%v, %q), want (true, \"pi1\")", busy, node)
	}
	for _, c := range *calls {
		if c.addr == "192.168.1.125" {
			t.Error("fetch was called for pi0's own address, which self-lookup should have skipped")
		}
	}
}

// TestNewPeerSetStoresNodesAndPath pins the constructor's simple job: no
// package-level var backs Nodes or JobPath any more, so two PeerSets built
// with different config never share state.
func TestNewPeerSetStoresNodesAndPath(t *testing.T) {
	a := NewPeerSet([]string{"10.0.0.1"}, "/job")
	b := NewPeerSet([]string{"10.0.0.2"}, "/other")

	if a.Nodes[0] == b.Nodes[0] {
		t.Fatal("precondition failed")
	}
	if a.JobPath == b.JobPath {
		t.Fatal("precondition failed")
	}
}
