// Package jobcoord builds internal/coordination's pieces -- this node's job
// Manager and its PeerSet -- from a config.Config.
//
// Three places need them: the API (internal/httpapi), the API's detached job
// child (internal/jobrun) and the local plug guard (internal/cli). Each used
// to spell the construction out itself, and a Manager built with a
// different staleness ceiling than the API's would disagree with it about
// whether a job is still running. coordination stays a stdlib-only leaf that
// takes plain values; this package is the one place that knows which config
// fields and which power worst case those values come from. It imports
// config, coordination and power only, so all three callers can import it.
package jobcoord

import (
	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/power"
)

// ManagerFor returns the job Manager for cfg.StateDir, with the staleness
// ceiling derived from cfg's UnmuteTimeout and the shutdown worst case (see
// coordination.NewManager).
func ManagerFor(cfg config.Config) *coordination.Manager {
	return coordination.NewManager(cfg.StateDir, cfg.UnmuteTimeout.D(), power.ShutdownWorstCase(cfg))
}

// PeersFor returns the PeerSet that asks cfg.PeerNodes for their jobs. base
// is the asking CGI node's own mount point (SCRIPT_NAME without its trailing
// slash), or "" for a process that is not serving a CGI request; see
// coordination.ResolvePeerJobPath for how it and cfg.PeerJobPath decide the
// path asked at.
func PeersFor(cfg config.Config, base string) *coordination.PeerSet {
	return coordination.NewPeerSet(cfg.PeerNodes, coordination.ResolvePeerJobPath(cfg.PeerJobPath, base))
}
