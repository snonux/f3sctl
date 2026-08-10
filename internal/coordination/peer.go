// Package coordination implements the fleet-coordination logic the HTTP API
// depends on but that is not itself HTTP: whether the other API node has a
// job in flight right now (PeerSet, this file), and the lifecycle of a job
// this node started (Manager and RunJob, in manager.go and run.go).
//
// It exists as its own package so internal/httpapi can stay pure HTTP
// transport and rendering: parse a request, ask this package what is true,
// render the answer. None of "is anything running right now" needs a live
// CGI request to test any more -- see peer_test.go and manager_test.go.
package coordination

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// runningState is the JSON value a peer's job resource reports while a job is
// in flight. It intentionally is not shared code with Manager's JobRunning:
// a peer's job is read out of someone else's JSON response, not constructed
// here, and TestPeerRunningStateMatchesJobRunning is what keeps the two
// literals in lockstep instead.
const runningState = "running"

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
	fetch func(addr, path, apiKey string) (*peerJob, error)
	// localAddrs lists this machine's own network addresses. Nil means the
	// real net.InterfaceAddrs; only tests substitute anything else.
	localAddrs func() ([]net.Addr, error)
	// lookupHost resolves a hostname to addresses. Nil means the real
	// net.LookupHost; only tests substitute anything else.
	lookupHost func(host string) ([]string, error)
}

// NewPeerSet returns a PeerSet that consults nodes for the job resource at
// jobPath.
func NewPeerSet(nodes []string, jobPath string) *PeerSet {
	return &PeerSet{Nodes: nodes, JobPath: jobPath}
}

// peerJob is the sliver of a peer's job entity this package needs.
type peerJob struct {
	State string
	Node  string
}

// Busy reports whether any other node in the set currently has a job
// running. self is this node's own hostname (so it is never asked), and
// apiKey authenticates the request the same way a client would.
//
// Failing to reach a peer is deliberately NOT treated as "busy": the peers
// are the machines that answer this API, and if one is unreachable the other
// must still be able to power the cluster on. Refusing to act because a peer
// is down would make the tool useless exactly when it is most needed.
func (ps *PeerSet) Busy(self, apiKey string) (bool, string) {
	selfHost := shortHost(self)

	for _, addr := range ps.Nodes {
		if ps.isSelf(addr, selfHost) {
			continue
		}

		job, err := ps.fetchPeer(addr, apiKey)
		if err != nil {
			continue
		}
		if job != nil && job.State == runningState {
			return true, job.Node
		}
	}
	return false, ""
}

func (ps *PeerSet) fetchPeer(addr, apiKey string) (*peerJob, error) {
	if ps.fetch != nil {
		return ps.fetch(addr, ps.JobPath, apiKey)
	}
	return fetchPeerJob(addr, ps.JobPath, apiKey)
}

// fetchPeerJob reads the peer's current job.
//
// The peer is asked over plain HTTP on the LAN rather than through relayd,
// because going out through the load balancer could route the question
// straight back to this node.
func fetchPeerJob(addr, path, apiKey string) (*peerJob, error) {
	url := fmt.Sprintf("http://%s%s", addr, path)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", apiKey)

	// Short timeout: this sits in front of every action, and a slow peer must
	// not make the API feel broken.
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer %s returned %s", addr, resp.Status)
	}

	var e struct {
		Properties struct {
			State string `json:"state"`
			Node  string `json:"node"`
		} `json:"properties"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, err
	}

	return &peerJob{State: e.Properties.State, Node: e.Properties.Node}, nil
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
