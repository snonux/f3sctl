// Package coordination implements the fleet-coordination logic the HTTP API
// depends on but that is not itself HTTP: whether the other API node has a
// job in flight right now (PeerSet, this file), and the lifecycle of a job
// this node started (Manager, in manager.go). The detached job
// child itself lives in internal/jobrun, the composition root.
//
// It exists as its own package so internal/httpapi can stay pure HTTP
// transport and rendering: parse a request, ask this package what is true,
// render the answer. None of "is anything running right now" needs a live
// CGI request to test any more -- see peer_test.go and manager_test.go.
package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// PeerSet answers "is the other API node mid-job?" for one node's power
// actions.
//
// The job lock (see Manager) is a flock on local disk, which serialises
// requests arriving at one node and nothing more. relayd load-balances pi0
// and pi1, so two clicks seconds apart land on different nodes, each sees an
// idle local lock, and two shutdowns run at once against the same hosts.
// Observed on 2026-08-08: a second power-off three seconds after the first
// was accepted with 202 instead of 409, and both jobs ran.
//
// Asking the peer closes that. It is not a distributed lock and cannot be --
// the only storage the two nodes share is NFS from the cluster they are about
// to switch off. But it turns the race window from "seconds" into "the
// round-trip of one HTTP request", which covers every realistic case: a human
// double-tapping, a watch retrying, two people acting at once.
type PeerSet struct {
	// Nodes are the other API nodes to consult before starting a job.
	//
	// Supplied by the caller (from config.Config.PeerNodes) rather than a
	// package-level var, so this package has no built-in idea of how many
	// nodes exist or where they live, and can be constructed with a fake set
	// in a test.
	Nodes []string
	// JobPath is the full URL path (including the CGI mount point) a peer's
	// current job is read from, e.g. "/cgi-bin/f3sctl/job". Supplied by the
	// caller (from config.Config.PeerJobPath) for the same reason as Nodes.
	JobPath string

	// fetch retrieves one peer's current job over HTTP. Nil means the real
	// network call; only tests substitute anything else -- the same seam as
	// power.Engine.isUp.
	fetch func(ctx context.Context, addr, path, apiKey string) (*Job, error)
	// localAddrs lists this machine's own network addresses. Nil means the
	// real net.InterfaceAddrs; only tests substitute anything else.
	localAddrs func() ([]net.Addr, error)
	// lookupHost resolves a hostname to addresses. Nil means the real
	// net.LookupHost; only tests substitute anything else.
	lookupHost func(host string) ([]string, error)

	// warn reports a peer fetch failure somewhere an operator can find it.
	// Nil means warnPeerFetchFailed (stderr -- see its doc comment for why);
	// only tests substitute anything else, so a test can assert a fetch
	// failure was reported without capturing the real process's stderr. Same
	// nil-means-real seam pattern as fetch, localAddrs and lookupHost above.
	warn func(addr string, err error)
}

// NewPeerSet returns a PeerSet that consults nodes for the job resource at
// jobPath.
func NewPeerSet(nodes []string, jobPath string) *PeerSet {
	return &PeerSet{Nodes: nodes, JobPath: jobPath}
}

// Busy reports whether any other node in the set currently has a job
// running. self is this node's own hostname (so it is never asked), and
// apiKey authenticates the request the same way a client would.
//
// ctx bounds the peer fetch: an unreachable peer already fails fast through
// the client's own 3s timeout (see fetchPeerJob), but a cancelled request
// context -- the CGI request that started this action going home, say -- now
// stops waiting on a peer that is neither idle nor answering rather than
// running the request's full deadline out first.
//
// Failing to reach a peer is deliberately NOT treated as "busy": the peers
// are the machines that answer this API, and if one is unreachable the other
// must still be able to power the cluster on. Refusing to act because a peer
// is down would make the tool useless exactly when it is most needed.
//
// A fetch failure is still reported (see ps.warnFunc / warnPeerFetchFailed)
// rather than swallowed outright: silently continuing past every error here
// is exactly what let a mis-derived JobPath (see ResolvePeerJobPath)
// look identical, from the field, to a peer that is genuinely down. The
// behaviour -- treat as idle -- does not change; only whether anything is
// left behind to debug it with.
func (ps *PeerSet) Busy(ctx context.Context, self, apiKey string) (bool, string) {
	// Lenient: every fetch error counts as idle, so err is always nil here.
	job, _ := ps.runningJob(ctx, self, apiKey, false)
	if job != nil {
		return true, job.Node
	}
	return false, ""
}

// errNoJobState is fetchPeerJob's answer for a 200 whose document carries
// no job state -- not a job resource at all. See fetchPeerJob.
var errNoJobState = errors.New("answered 200 with no job state (is peer_job_path the job resource?)")

// ErrPeerJobUnknown wraps a peer that answered RunningJob's question but not
// with a job: a non-200 status (401 for a wrong or rotated API key, 404 for
// a wrong peer_job_path, 5xx for a broken node) or a body that is not a job.
var ErrPeerJobUnknown = errors.New("an API node answered but did not say whether it is running a job")

// RunningJob is Busy for a caller outside the API -- the local `fans off` /
// `ac off` guard in internal/cli -- that must not read a peer it could not
// understand as idle. It returns the first running job a peer reports.
//
// Only a peer that could not be reached at all (connection refused, timed
// out, dropped mid-answer: the node is down, the case Busy's fail-open
// exists for) is skipped as idle, with the same warning. A peer that
// answered with anything but a job makes it return an error wrapping
// ErrPeerJobUnknown: that is this caller's configuration (its API key, the
// job path) talking, and treating it as idle would switch a plug under a
// job nobody could see.
//
// The API itself keeps Busy: a CGI node's peer check must not start
// refusing every action over the other node's misconfiguration.
func (ps *PeerSet) RunningJob(ctx context.Context, self, apiKey string) (*Job, error) {
	return ps.runningJob(ctx, self, apiKey, true)
}

// runningJob asks every other node in turn for its job and returns the first
// running one. strict decides what an error does: lenient (Busy) warns and
// skips every one; strict (RunningJob) skips only an unreachable peer and
// returns any other failure.
func (ps *PeerSet) runningJob(ctx context.Context, self, apiKey string, strict bool) (*Job, error) {
	selfHost := shortHost(self)

	for _, addr := range ps.Nodes {
		if ps.isSelf(addr, selfHost) {
			continue
		}

		job, err := ps.fetchPeer(ctx, addr, apiKey)
		if !strict && errors.Is(err, errNoJobState) {
			// Lenient, as before errNoJobState existed: an answer with no
			// job state is a peer with no job to report, not a failure.
			continue
		}
		if err != nil {
			var unreachable *peerUnreachableError
			if strict && !errors.As(err, &unreachable) {
				return nil, fmt.Errorf("%w: %w", ErrPeerJobUnknown, err)
			}
			ps.warnFunc()(addr, err)
			continue
		}
		if job != nil && job.State == JobRunning {
			return job, nil
		}
	}
	return nil, nil
}

// FetchJob asks each node in the set for its current or last job in turn,
// returning the first one that answers -- so that GET /job (the power
// surface's currentJob) can report the same job regardless of which of
// pi0/pi1 relayd routed the request to. self and apiKey are as in Busy; ctx
// is as in Busy too (bounds the peer fetch against the request that asked for
// it).
//
// Consulting more than one node only matters once the set holds more than
// the pair this project actually runs; with exactly one peer it is just
// "ask it". A peer that cannot be reached, same as Busy, is skipped rather
// than treated as an error -- this node's own job then simply stands alone,
// which is what happened before this existed. A peer that answers but
// reports no job at all (fetchPeerJob's nil, nil case) is likewise skipped,
// so a later, reachable peer that *does* have one still gets a chance.
func (ps *PeerSet) FetchJob(ctx context.Context, self, apiKey string) *Job {
	selfHost := shortHost(self)

	for _, addr := range ps.Nodes {
		if ps.isSelf(addr, selfHost) {
			continue
		}

		job, err := ps.fetchPeer(ctx, addr, apiKey)
		if errors.Is(err, errNoJobState) {
			continue // no job to report, as for "none"; see runningJob
		}
		if err != nil {
			ps.warnFunc()(addr, err)
			continue
		}
		if job != nil {
			return job
		}
	}
	return nil
}

func (ps *PeerSet) fetchPeer(ctx context.Context, addr, apiKey string) (*Job, error) {
	if ps.fetch != nil {
		return ps.fetch(ctx, addr, ps.JobPath, apiKey)
	}
	return fetchPeerJob(ctx, addr, ps.JobPath, apiKey)
}

// warnFunc returns the peer-fetch-failure reporter, falling back to the real
// stderr write. Same nil-safety pattern as fetchPeer/addrs/lookup.
func (ps *PeerSet) warnFunc() func(addr string, err error) {
	if ps.warn != nil {
		return ps.warn
	}
	return warnPeerFetchFailed
}

// warnPeerFetchFailed reports a peer fetch failure to this process's stderr.
//
// A CGI request (see httpapi.ServeCGI) has no log stream of its own -- its
// only output channel carries the HTTP response body, and writing a warning
// into that would corrupt it -- so stderr is the one channel left. Under
// bozohttpd (this deployment's CGI host) a CGI child's stderr is captured
// into the web server's own error log, matching the
// fmt.Fprintf(os.Stderr, ...) pattern this project already uses for
// out-of-band diagnostics that have nowhere else to go (see
// cmd/f3sctl/main.go and internal/jobrun).
//
// The message distinguishes a connection-level failure (peer down,
// unreachable, timed out -- *peerHTTPStatusError is not among err's chain)
// from an HTTP-level one (the peer answered, but not with 200 --
// *peerHTTPStatusError is), because the two point at very different
// problems: the former is a peer that is plausibly actually down, which is
// the case Busy's "treat as idle" is designed for; the latter -- e.g. a 404
// -- is much more likely this node asking the wrong path, such as the
// empty-SCRIPT_NAME derivation bug ResolvePeerJobPath guards against.
func warnPeerFetchFailed(addr string, err error) {
	fmt.Fprintf(os.Stderr, "f3sctl: peer %s fetch failed (%s, treating peer as idle): %v\n",
		addr, peerFetchFailureKind(err), err)
}

// peerFetchFailureKind classifies a peer fetch error for warnPeerFetchFailed:
// "HTTP error" when the peer answered but not with 200 (a *peerHTTPStatusError
// is in err's chain), "connection failure" otherwise -- the request never
// completed at all (dial refused, timed out, DNS failure, ...). Split out
// from warnPeerFetchFailed so the classification can be tested without
// capturing the real process's stderr.
func peerFetchFailureKind(err error) string {
	var statusErr *peerHTTPStatusError
	if errors.As(err, &statusErr) {
		return "HTTP error"
	}
	return "connection failure"
}

// peerHTTPStatusError means the peer answered over HTTP but not with 200,
// as opposed to the request never completing at all (dial refused, timed
// out, DNS failure, ...). warnPeerFetchFailed distinguishes the two: this
// one means the peer is reachable but something about the request was
// wrong -- e.g. the path this node asked for is not served there, which is
// what a mis-derived JobPath (see ResolvePeerJobPath) would produce.
type peerHTTPStatusError struct {
	addr   string
	status string
}

func (e *peerHTTPStatusError) Error() string {
	return fmt.Sprintf("peer %s returned %s", e.addr, e.status)
}

// peerUnreachableError means the request to a peer never completed: it
// could not be dialled, timed out, or the connection dropped before the
// answer was read. It is the one failure RunningJob may read as an idle peer
// (the node is down), unlike an answer it could not use.
type peerUnreachableError struct {
	addr string
	err  error
}

func (e *peerUnreachableError) Error() string {
	return fmt.Sprintf("peer %s unreachable: %v", e.addr, e.err)
}

func (e *peerUnreachableError) Unwrap() error { return e.err }

// JobPath is the job resource's path under the API's CGI mount point:
// powerapi serves its job route here, and a peer's job is fetched from the
// mount plus this. Declared on this side so the peer path can be derived
// without this package importing the REST surface.
const JobPath = "/job"

// DefaultCGIMount is this project's own documented CGI mount convention (see
// README.md's example config, and config.Default() before uy0). It is the
// last-resort fallback ResolvePeerJobPath uses when there is no mount to
// derive anything from -- see that function's doc comment for why an empty
// base cannot be trusted as "mounted at the root".
const DefaultCGIMount = "/cgi-bin/f3sctl"

// ResolvePeerJobPath returns the URL path at which a peer's current job is
// asked for (PeerSet.JobPath). explicit is config.Config.PeerJobPath; base is
// the asking CGI node's own mount point (SCRIPT_NAME without its trailing
// slash), or "" for a process that is not serving a CGI request.
//
// An explicit path always wins, for the rare case where the two peers are
// not mounted the same way. Otherwise (the default) it is derived from this
// node's own mount -- the identical mechanism every link and action handed
// back to a client already goes through -- on the assumption that pi0 and
// pi1 are symmetric peers sharing one CGI mount. That keeps a SCRIPT_NAME
// remount a one-place change instead of two: without this, an operator who
// moves the mount point but forgets the separate peer_job_path config value
// gets a peer check that silently reads back as idle forever, which is the
// dangerous failure mode -- two jobs can start.
//
// The one case that derivation must NOT be trusted for: base itself being
// empty. For a CGI node that happens whenever its own SCRIPT_NAME was empty
// or missing (bozohttpd not setting it, a proxy that strips the header,
// ServeCGI invoked outside its normal CGI harness) -- and an empty
// SCRIPT_NAME is far more likely to be a broken environment than a
// deliberate "the API is mounted at the filesystem root". Deriving anyway
// would silently hand PeerSet a bare "/job", which almost certainly 404s on
// the peer, and Busy treats every fetch error as "peer not busy". Falling
// back to DefaultCGIMount is a safer bet, and matches every real deployment
// of this project. It is also the whole answer for a caller with no mount of
// its own, such as the local plug guard.
func ResolvePeerJobPath(explicit, base string) string {
	if explicit != "" {
		return explicit
	}
	if base == "" {
		base = DefaultCGIMount
	}
	return base + JobPath
}

// PeerQueryParam marks a GET /job request as one node asking another for its
// own job, as opposed to an ordinary client request.
//
// the power surface's handleJob normally answers with currentJob's merge of its own job
// and its peer's (see that function), so that /job reads the same regardless
// of which of pi0/pi1 a request landed on. Left unchecked, that merge would
// recurse forever: fetchPeerJob asking pi1 for its job would have pi1's own
// handleJob ask pi0 back, which would ask pi1 back, and so on. Every
// peer-to-peer job fetch -- both this one and Busy's -- carries this marker
// so the node answering it reports its own local job only, never merged,
// breaking the cycle at the first hop. See handleJob for the other end of
// this contract.
const PeerQueryParam = "peer"

// fetchPeerJob reads the peer's current job.
//
// The peer is asked over plain HTTP on the LAN rather than through relayd,
// because going out through the load balancer could route the question
// straight back to this node.
//
// The response decodes straight into Job: the wire property names already
// match Job's own json tags, since it is the identical document jobEntity
// renders locally. A peer that has never run a job answers with
// properties.state "none" (the power surface's handleJob's no-job case) rather than a
// real job at all; that is reported here as (nil, nil) -- reached the peer,
// nothing to show -- which is a different outcome from (nil, err) and must
// stay one: the latter is what Busy and FetchJob warn about and skip past as
// "peer unreachable", while the former is a peer that answered perfectly
// well and simply has no job to report.
//
// A 200 whose properties carry no state at all -- `null`, `{}`, or some
// other resource's document, as a peer_job_path pointing at the wrong route
// would fetch -- is NOT that "none": it returns an error wrapping
// errNoJobState. Busy and FetchJob skip it silently, exactly as they always
// treated it, but RunningJob must not read a document that is not a job as
// an idle peer.
func fetchPeerJob(ctx context.Context, addr, path, apiKey string) (*Job, error) {
	url := fmt.Sprintf("http://%s%s?%s=1", addr, path, PeerQueryParam)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", apiKey)

	// Short timeout: this sits in front of every action, and a slow peer must
	// not make the API feel broken.
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &peerUnreachableError{addr: addr, err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &peerHTTPStatusError{addr: addr, status: resp.Status}
	}

	var e struct {
		Properties Job `json:"properties"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, &peerUnreachableError{addr: addr, err: err}
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, err
	}

	switch e.Properties.State {
	case "none":
		return nil, nil
	case "":
		return nil, fmt.Errorf("peer %s: %w", addr, errNoJobState)
	}
	job := e.Properties
	return &job, nil
}

// isSelf reports whether addr is one of this machine's own addresses, so a
// node never asks itself.
func (ps *PeerSet) isSelf(addr, selfName string) bool {
	if strings.HasPrefix(selfName, "pi") {
		// Cheap path: hostnames here are piN and the peer list is in the same
		// order as their addresses.
		if ips, err := ps.lookup(selfName); err == nil {
			for _, ip := range ips {
				if ip == addr {
					return true
				}
			}
		}
	}

	addrs, err := ps.addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.String() == addr {
			return true
		}
	}
	return false
}

func (ps *PeerSet) lookup(host string) ([]string, error) {
	if ps.lookupHost != nil {
		return ps.lookupHost(host)
	}
	return net.LookupHost(host)
}

func (ps *PeerSet) addrs() ([]net.Addr, error) {
	if ps.localAddrs != nil {
		return ps.localAddrs()
	}
	return net.InterfaceAddrs()
}

// shortHost trims a FQDN down to its first label, or falls back to the local
// hostname when name is empty (a bare process invocation rather than one that
// already knows the CGI node's name).
func shortHost(name string) string {
	if h, _, ok := strings.Cut(name, "."); ok {
		return h
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	return name
}
