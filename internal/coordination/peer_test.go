package coordination

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
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
	job *Job
	err error
}) (func(addr, path, apiKey string) (*Job, error), *[]fetchCall) {
	calls := &[]fetchCall{}
	fn := func(addr, path, apiKey string) (*Job, error) {
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
		job *Job
		err error
	}{
		"192.168.1.126": {job: &Job{State: JobRunning, Node: "pi1"}},
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
		job *Job
		err error
	}{
		"192.168.1.126": {job: &Job{State: JobDone, Node: "pi1"}},
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
		job *Job
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
		job *Job
		err error
	}{
		"10.0.0.1": {err: errors.New("unreachable")},
		"10.0.0.2": {job: &Job{State: JobDone}},
		"10.0.0.3": {job: &Job{State: JobRunning, Node: "third"}},
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
		job *Job
		err error
	}{
		"10.0.0.9": {job: &Job{State: JobRunning, Node: "other"}},
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
		job *Job
		err error
	}{
		"192.168.1.126": {job: &Job{State: JobRunning, Node: "pi1"}},
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

// TestPeerSetBusyWarnsOnFetchFailure is the regression test for the second
// half of uy0: a fetch failure used to be swallowed by Busy's `continue` with
// nothing left behind to debug it -- indistinguishable in the field from a
// peer that is genuinely down. Busy must now report every fetch failure
// through ps.warn (the same nil-means-real seam as fetch/localAddrs/
// lookupHost), so a test -- or, in production, warnPeerFetchFailed's stderr
// write -- has something to go on.
func TestPeerSetBusyWarnsOnFetchFailure(t *testing.T) {
	wantErr := errors.New("connection refused")
	fetch, _ := fakeFetcher(map[string]struct {
		job *Job
		err error
	}{
		"192.168.1.126": {err: wantErr},
	})

	type warnCall struct {
		addr string
		err  error
	}
	var warns []warnCall
	ps := &PeerSet{
		Nodes:   []string{"192.168.1.126"},
		JobPath: "/job",
		fetch:   fetch,
		warn:    func(addr string, err error) { warns = append(warns, warnCall{addr, err}) },
	}

	ps.Busy("pi0", "k")

	if len(warns) != 1 {
		t.Fatalf("warn called %d times, want exactly 1", len(warns))
	}
	if warns[0].addr != "192.168.1.126" {
		t.Errorf("warn addr = %q, want %q", warns[0].addr, "192.168.1.126")
	}
	if warns[0].err != wantErr {
		t.Errorf("warn err = %v, want %v", warns[0].err, wantErr)
	}
}

// TestPeerFetchFailureKindDistinguishesHTTPStatusFromConnectionFailure pins
// warnPeerFetchFailed's classification: a *peerHTTPStatusError (the peer
// answered, just not with 200 -- e.g. a 404 from a mis-derived JobPath, see
// httpapi.resolvePeerJobPath) must read as "HTTP error", while a plain
// error (the request never completed at all) must read as "connection
// failure". An operator debugging "peer coordination isn't working" needs
// exactly this distinction: the former points at this node's own
// configuration, the latter at the peer actually being down.
func TestPeerFetchFailureKindDistinguishesHTTPStatusFromConnectionFailure(t *testing.T) {
	httpErr := &peerHTTPStatusError{addr: "192.168.1.126", status: "404 Not Found"}
	if got := peerFetchFailureKind(httpErr); got != "HTTP error" {
		t.Errorf("peerFetchFailureKind(%v) = %q, want %q", httpErr, got, "HTTP error")
	}

	connErr := errors.New("dial tcp 192.168.1.126:80: connect: connection refused")
	if got := peerFetchFailureKind(connErr); got != "connection failure" {
		t.Errorf("peerFetchFailureKind(%v) = %q, want %q", connErr, got, "connection failure")
	}
}

// TestPeerSetFetchJobReturnsThePeersJob is FetchJob's core positive case: a
// reachable peer with a job to report is what a client asking either node
// about /job must see, per httpapi.currentJob.
func TestPeerSetFetchJobReturnsThePeersJob(t *testing.T) {
	fetch, calls := fakeFetcher(map[string]struct {
		job *Job
		err error
	}{
		"192.168.1.126": {job: &Job{ID: "abc", State: JobDone, Node: "pi1"}},
	})
	ps := &PeerSet{Nodes: []string{"192.168.1.126"}, JobPath: "/job", fetch: fetch}

	got := ps.FetchJob("pi0", "k")
	if got == nil || got.ID != "abc" {
		t.Fatalf("FetchJob() = %v, want the peer's job (id \"abc\")", got)
	}
	if len(*calls) != 1 {
		t.Errorf("fetch called %d times, want exactly 1", len(*calls))
	}
}

// TestPeerSetFetchJobReturnsNilWhenUnreachable pins the same tolerance as
// Busy: an unreachable peer must not turn /job into an error, only into this
// node's own job standing alone -- see coordination.NewestJob and
// httpapi.currentJob.
func TestPeerSetFetchJobReturnsNilWhenUnreachable(t *testing.T) {
	fetch, _ := fakeFetcher(map[string]struct {
		job *Job
		err error
	}{
		"192.168.1.126": {err: errors.New("connection refused")},
	})
	ps := &PeerSet{Nodes: []string{"192.168.1.126"}, JobPath: "/job", fetch: fetch}

	if got := ps.FetchJob("pi0", "k"); got != nil {
		t.Errorf("FetchJob() = %v, want nil for an unreachable peer", got)
	}
}

// TestPeerSetFetchJobSkipsAPeerWithNoJobInFavourOfOneWithOne pins that
// fetchPeerJob's (nil, nil) "reached the peer, nothing to show" case is not
// treated as a reason to stop looking: a later node in the set that does have
// a job must still be found.
func TestPeerSetFetchJobSkipsAPeerWithNoJobInFavourOfOneWithOne(t *testing.T) {
	fetch, _ := fakeFetcher(map[string]struct {
		job *Job
		err error
	}{
		"10.0.0.1": {job: nil, err: nil},
		"10.0.0.2": {job: &Job{ID: "found", State: JobDone}},
	})
	ps := &PeerSet{Nodes: []string{"10.0.0.1", "10.0.0.2"}, JobPath: "/job", fetch: fetch}

	got := ps.FetchJob("pi0", "k")
	if got == nil || got.ID != "found" {
		t.Errorf("FetchJob() = %v, want the second node's job", got)
	}
}

// TestFetchPeerJobMarksTheRequestAsAPeerQuery exercises the real fetchPeerJob
// against an httptest server, pinning PeerQueryParam's presence on the wire:
// httpapi.handleJob relies on seeing it to answer with its own unmerged job
// rather than recursing back into the peer that just asked it -- see
// PeerQueryParam's doc comment.
func TestFetchPeerJobMarksTheRequestAsAPeerQuery(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		fmt.Fprint(w, `{"properties":{"state":"done","node":"pi1"}}`)
	}))
	defer srv.Close()

	job, err := fetchPeerJob(srv.Listener.Addr().String(), "/job", "k")
	if err != nil {
		t.Fatalf("fetchPeerJob: %v", err)
	}
	if job == nil || job.State != JobDone {
		t.Fatalf("fetchPeerJob() = %v, want a done job", job)
	}
	if gotQuery != PeerQueryParam+"=1" {
		t.Errorf("request query = %q, want %q", gotQuery, PeerQueryParam+"=1")
	}
}

// TestFetchPeerJobReportsNoJobAsNilNotError pins the distinction fetchPeerJob
// documents: a peer that answers with the "no job has ever run" shape
// (properties.state "none", httpapi.handleJob's own no-job response) is a
// successful fetch with nothing to report, not a failure -- FetchJob must be
// able to tell that apart from a peer it could not reach at all.
func TestFetchPeerJobReportsNoJobAsNilNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"properties":{"state":"none","node":"pi1"}}`)
	}))
	defer srv.Close()

	job, err := fetchPeerJob(srv.Listener.Addr().String(), "/job", "k")
	if err != nil {
		t.Fatalf("fetchPeerJob: %v, want nil error for a peer reporting no job", err)
	}
	if job != nil {
		t.Errorf("fetchPeerJob() = %v, want nil job for state \"none\"", job)
	}
}
