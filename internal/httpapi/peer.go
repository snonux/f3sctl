package httpapi

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

// peerNodes are the other API nodes to consult before starting a job.
//
// The job lock is a flock on local disk, which serialises requests arriving at
// one node and nothing more. relayd load-balances pi0 and pi1, so two clicks
// seconds apart land on different nodes, each sees an idle local lock, and two
// shutdowns run at once against the same hosts. Observed on 2026-08-08: a
// second power-off three seconds after the first was accepted with 202 instead
// of 409, and both jobs ran.
//
// Asking the peer closes that. It is not a distributed lock and cannot be --
// the only storage the two nodes share is NFS from the cluster they are about
// to switch off. But it turns the race window from "seconds" into "the
// round-trip of one HTTP request", which covers every realistic case: a human
// double-tapping, a watch retrying, two people acting at once.
var peerNodes = []string{"192.168.1.125", "192.168.1.126"}

// jobPath is the resource the peer check reads.
//
// Named so Server.handle can exclude it from triggering a peer check of its
// own: a peer check that recurses into another peer check makes each node's
// answer wait on the other's.
const jobPath = "/job"

// peerJobRunning reports whether any other API node currently has a job
// running.
//
// Failing to reach a peer is deliberately NOT treated as "busy": the peers are
// the machines that answer this API, and if one is unreachable the other must
// still be able to power the cluster on. Refusing to act because a peer is
// down would make the tool useless exactly when it is most needed.
func (s *Server) peerJobRunning(key string) (bool, string) {
	self := shortHost(s.node)

	for _, addr := range peerNodes {
		if isSelf(addr, self) {
			continue
		}

		job, err := fetchPeerJob(addr, key)
		if err != nil {
			continue
		}
		if job != nil && job.State == JobRunning {
			return true, job.Node
		}
	}
	return false, ""
}

// fetchPeerJob reads the peer's current job.
//
// The peer is asked over plain HTTP on the LAN rather than through relayd,
// because going out through the load balancer could route the question
// straight back to this node.
func fetchPeerJob(addr, key string) (*Job, error) {
	url := fmt.Sprintf("http://%s/cgi-bin/f3sctl/job", addr)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", key)

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

	return &Job{State: JobState(e.Properties.State), Node: e.Properties.Node}, nil
}

// isSelf reports whether addr is one of this machine's own addresses, so a
// node never asks itself.
func isSelf(addr, selfName string) bool {
	if strings.HasPrefix(selfName, "pi") {
		// Cheap path: hostnames here are piN and the peer list is in the same
		// order as their addresses.
		if ips, err := net.LookupHost(selfName); err == nil {
			for _, ip := range ips {
				if ip == addr {
					return true
				}
			}
		}
	}

	ifaceAddrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range ifaceAddrs {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.String() == addr {
			return true
		}
	}
	return false
}

func shortHost(name string) string {
	if h, _, ok := strings.Cut(name, "."); ok {
		return h
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	return name
}
