package coordination

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// strictPeerSet is a PeerSet over one real httptest peer answering handler,
// with warnings collected rather than written to stderr.
func strictPeerSet(t *testing.T, handler http.HandlerFunc) (*PeerSet, *[]error) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	warns := &[]error{}
	ps := &PeerSet{
		Nodes:   []string{srv.Listener.Addr().String()},
		JobPath: "/job",
		warn:    func(_ string, err error) { *warns = append(*warns, err) },
	}
	return ps, warns
}

// TestRunningJobReturnsThePeersRunningJob is the positive case: the whole
// job comes back, so the caller's refusal can name it.
func TestRunningJobReturnsThePeersRunningJob(t *testing.T) {
	ps, _ := strictPeerSet(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"properties":{"id":"c1","action":"all-cycle","state":"running","node":"pi1"}}`)
	})

	job, err := ps.RunningJob(context.Background(), "pi0", "k")
	if err != nil || job == nil || job.ID != "c1" || job.Action != "all-cycle" || job.Node != "pi1" {
		t.Fatalf("RunningJob() = %+v, %v; want the peer's running job", job, err)
	}
}

// TestRunningJobIgnoresAnIdlePeer: no job, or a finished one, is idle.
func TestRunningJobIgnoresAnIdlePeer(t *testing.T) {
	for _, state := range []string{"none", "done", "failed"} {
		t.Run(state, func(t *testing.T) {
			ps, _ := strictPeerSet(t, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"properties":{"state":%q,"node":"pi1"}}`, state)
			})
			if job, err := ps.RunningJob(context.Background(), "pi0", "k"); job != nil || err != nil {
				t.Fatalf("RunningJob() = %+v, %v; want nil, nil", job, err)
			}
		})
	}
}

// TestRunningJobFailsClosedOnAnAnswerItCannotUse is the difference from
// Busy: a peer that answered 401 (wrong or rotated key), 403, 404 (wrong
// job path) or 5xx, or answered 200 with something that is not a job -- not
// JSON, `null`, `{}`, or another resource's document, as a peer_job_path
// pointing at the wrong route would fetch -- has not said it is idle. Only
// state "none" says that. RunningJob reports ErrPeerJobUnknown, and Busy and
// FetchJob still read the same peer as having no job -- the API's behaviour
// is unchanged.
func TestRunningJobFailsClosedOnAnAnswerItCannotUse(t *testing.T) {
	body := func(doc string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, doc) }
	}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		status  bool // answered with a non-200 status
		noState bool // answered 200 with a JSON document carrying no job state
	}{
		{"401", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }, true, false},
		{"403", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }, true, false},
		{"404", http.NotFound, true, false},
		{"500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, true, false},
		{"not json", body("<html>"), false, false},
		{"not a job", body(`{"properties":{"node":"pi1"}}`), false, true},
		{"null", body("null"), false, true},
		{"empty object", body("{}"), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps, warns := strictPeerSet(t, tc.handler)

			job, err := ps.RunningJob(context.Background(), "pi0", "k")
			if !errors.Is(err, ErrPeerJobUnknown) || job != nil {
				t.Fatalf("RunningJob() = %+v, %v; want ErrPeerJobUnknown", job, err)
			}
			var statusErr *peerHTTPStatusError
			if isStatus := errors.As(err, &statusErr); isStatus != tc.status {
				t.Errorf("err = %v; want a *peerHTTPStatusError exactly for the status cases", err)
			}
			if noState := errors.Is(err, errNoJobState); noState != tc.noState {
				t.Errorf("err = %v; want errNoJobState exactly for the stateless documents", err)
			}

			*warns = nil
			if busy, _ := ps.Busy(context.Background(), "pi0", "k"); busy {
				t.Error("Busy() = true; the API's lenient check must still read this peer as idle")
			}
			if got := ps.FetchJob(context.Background(), "pi0", "k"); got != nil {
				t.Errorf("FetchJob() = %+v, want nil", got)
			}
			if tc.noState && len(*warns) != 0 {
				t.Errorf("lenient checks warned %v; a stateless answer was always skipped silently", *warns)
			}
		})
	}
}

// TestRunningJobTreatsAnUnreachablePeerAsIdle: a peer that cannot be reached
// at all -- here a closed listener, connection refused -- is the down node
// the fail-open exists for. It is skipped with a warning, not an error.
func TestRunningJobTreatsAnUnreachablePeerAsIdle(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.Listener.Addr().String()
	srv.Close()
	var warns []error
	ps := &PeerSet{Nodes: []string{addr}, JobPath: "/job",
		warn: func(_ string, err error) { warns = append(warns, err) }}

	job, err := ps.RunningJob(context.Background(), "pi0", "k")
	if job != nil || err != nil {
		t.Fatalf("RunningJob() = %+v, %v; want nil, nil for an unreachable peer", job, err)
	}
	if len(warns) != 1 {
		t.Fatalf("warned %d times, want once", len(warns))
	}
	var unreachable *peerUnreachableError
	if !errors.As(warns[0], &unreachable) {
		t.Errorf("warning err = %v, want a *peerUnreachableError", warns[0])
	}
}

// TestRunningJobKeepsAskingPastAnUnreachablePeer: skipping a down peer must
// not stop the next one being asked.
func TestRunningJobKeepsAskingPastAnUnreachablePeer(t *testing.T) {
	fetch, calls := fakeFetcher(map[string]struct {
		job *Job
		err error
	}{
		"192.168.1.125": {err: &peerUnreachableError{addr: "192.168.1.125", err: errors.New("refused")}},
		"192.168.1.126": {job: &Job{ID: "x", State: JobRunning, Node: "pi1"}},
	})
	ps := &PeerSet{Nodes: []string{"192.168.1.125", "192.168.1.126"}, JobPath: "/job", fetch: fetch,
		warn: func(string, error) {}}

	job, err := ps.RunningJob(context.Background(), "earth", "k")
	if err != nil || job == nil || job.ID != "x" {
		t.Fatalf("RunningJob() = %+v, %v; want the second peer's job", job, err)
	}
	if len(*calls) != 2 {
		t.Errorf("fetched %d peers, want 2", len(*calls))
	}
}

// TestResolvePeerJobPath pins the derivation both the API (with its
// SCRIPT_NAME) and the local plug guard (with none) use.
//
// Derived from the mount when there is one (uy0: a remount must not need a
// config edit too); an explicit peer_job_path wins, for peers mounted
// differently; and an empty base falls back to DefaultCGIMount rather than a
// bare "/job" -- which would 404 on the peer and, for Busy, read as idle.
func TestResolvePeerJobPath(t *testing.T) {
	tests := []struct {
		name, explicit, base, want string
	}{
		{"derived from the mount", "", "/cgi-bin/f3sctl", "/cgi-bin/f3sctl/job"},
		{"derived from a remount", "", "/api", "/api/job"},
		{"explicit override wins", "/somewhere/else/job", "/cgi-bin/f3sctl", "/somewhere/else/job"},
		{"explicit override without a base", "/x/job", "", "/x/job"},
		{"empty base falls back", "", "", DefaultCGIMount + JobPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolvePeerJobPath(tt.explicit, tt.base); got != tt.want {
				t.Errorf("ResolvePeerJobPath(%q, %q) = %q, want %q", tt.explicit, tt.base, got, tt.want)
			}
		})
	}
}
