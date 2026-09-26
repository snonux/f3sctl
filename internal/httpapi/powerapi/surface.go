// Package powerapi is the REST surface for everything power: host status,
// the power operations and their jobs, and the rack-fan plug.
//
// It is one of internal/httpapi's two domain surfaces -- the other is
// gogiosapi -- and holds exactly the routes and handlers whose subject is the
// rack: the fleet/status snapshot, the on/off operations (cluster-wide,
// all-hosts and per-host), the job that tracks a running operation, and the
// Shelly plug the fans hang on. Everything that is neither (the Siren
// vocabulary, the route plumbing, the composition root) stays in the parent,
// which imports this package rather than the other way round; the shared
// vocabulary both sides speak is contract.
//
// A deliberate subtlety of the split: the power ENGINE (internal/power)
// remains one implementation shared by the CLI and this surface, and this
// package never imports it wholesale -- it depends on the three slices of it
// it actually drives (Engine, Jobs, Peers below), declared as interfaces so
// the surface can be table-declared, tested and served without a real engine
// on the end of them.
package powerapi

import (
	"context"
	"time"

	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// JobPath is PATH_INFO for the job route, as CGI sees it -- i.e. without the
// SCRIPT_NAME mount point. coordination.PeerSet.JobPath is the *full* URL
// path (mount point included) a peer's job is fetched from over a real HTTP
// connection; as of uy0 the two are the SAME value by construction in the
// default case -- resolvePeerJobPath (in the composition root) builds
// PeerSet.JobPath from this node's own router.Href(JobPath), i.e. this
// constant plus this node's own SCRIPT_NAME, and passes it to
// coordination.NewPeerSet. It only diverges from that derivation when
// config.Config.PeerJobPath is explicitly set to override it (the
// asymmetric-mount case; see that field's doc comment) --
// config.Config.PeerJobPath itself stays empty in the default case, since
// resolvePeerJobPath never writes back into it.
//
// Named so Server.serve's enrichState can exclude it from triggering a peer
// check of its own: a peer check that recurses into another peer check makes
// each node's answer wait on the other's.
const JobPath = "/job"

// StatusPath is PATH_INFO for the status route, named for the same reason as
// JobPath: enrichState (composition root) excludes it from its own,
// separate peer-busy check, because handleStatus already makes its own single
// peer round trip (for the job it embeds) and reuses that answer rather than
// paying for a second one -- see handleStatus.
const StatusPath = "/status"

// Surface is the power REST surface, bound to the collaborators its handlers
// need.
//
// Everything here is injected by the composition root (internal/httpapi) when
// it assembles a Server. The engine slices, job manager and peer set are the
// same objects the composition root assembled for the rest of the pipeline;
// nil collaborators are safe at table-declaration time -- the closures
// dereference them only while serving the route that needs them. That is the
// exact contract, not a graduated one: every method the serving path can
// reach requires its collaborator to be non-nil (peerJob's nil tolerance
// covers exactly the /job-answers-own-job-without-a-peer case, nothing
// else), so a test serving a route must wire the collaborator that route
// reads -- the same rule the pre-split package's Server fields lived under.
type Surface struct {
	// Node is this node's hostname, reported on every entity ("node"
	// property) so a client can tell which of pi0/pi1 answered.
	Node string
	// Href builds the absolute href for a route path, under the CGI mount
	// this node answers on. See contract.Href.
	Href func(string) string
	// Inv is the configured inventory: the same one the engine acts on, which
	// is what makes the per-host action routes match exactly the hosts the
	// engine probes and shows in /status. See Routes' doc comment.
	//
	// Unlike the collaborators below, Inv is required whenever the surface
	// serves or advertises anything power-related: the availability
	// predicates (clusterHostsUp, everyFHostUp) and the snapshot halves of
	// the fan and AC guards (rackBusy, acBusy) judge the snapshot against its
	// groups. Without it both fail closed rather than guess: the counts are
	// zero, so no power action is offered, and the guards read the rack as
	// busy (see power.RackActivityFrom), so cutting fans or AC demands force.
	Inv inventory.Inventory
	// Engine is the fans-and-probes slice of the power engine this surface
	// drives directly (the plug write, and the strict rack-activity probe the
	// fan guard confirms with).
	Engine Engine
	// Jobs starts a detached power job and reports its staleness ceiling.
	Jobs Jobs
	// Peers asks the other API node about its job state.
	Peers Peers
	// RackConfirm re-probes what is running in the rack with the stricter,
	// consecutive-silence evidence the fan guard demands before anything cuts
	// cooling. Nil means Engine.RackActivity; only tests substitute anything
	// else. See confirmRack.
	RackConfirm func(context.Context) power.RackActivity
	// ACConfirm is the AC-off guard's strict re-probe (every f-host). Nil
	// means Engine.ACActivity. See confirmAC.
	ACConfirm func(context.Context) power.RackActivity
	// actions renders every actions list this surface's resources advertise:
	// the whole API's (the status route), a resource's own controls, or one
	// whole API section's (contract.Route.Section) -- what a section folder
	// offers (/power for host power, /ac-control for Shelly plugs), judged
	// state by state, without the folder naming any action by hand. It is
	// unexported and set once, by New, which rejects a nil one; in production
	// it is the composition root's Router (resolved lazily), the single
	// source of the Siren action shape (name, title, method, href, cliVerb,
	// fields).
	actions contract.ActionRenderer
}

// Engine is the slice of the power engine the REST surface drives directly:
// writing the plugs, and the strict rack-activity probes. Satisfied by
// *power.Engine in production and by fakes in tests.
type Engine interface {
	// FansSet switches the rack-fan plug and returns the read-back.
	FansSet(ctx context.Context, on bool) (power.FansState, error)
	// ACSet switches the f-host mains AC plug and returns the read-back.
	ACSet(ctx context.Context, on bool) (power.ACState, error)
	// RackActivity reports what may still be drawing power in the fan-cooled
	// rack (f0/f1/f2), judged on the strict, consecutive-silence evidence.
	RackActivity(ctx context.Context) power.RackActivity
	// ACActivity reports what may still be drawing power on the AC circuit
	// (f0–f3), judged on the same strict evidence.
	ACActivity(ctx context.Context) power.RackActivity
}

// Jobs is the slice of the coordination manager the surface drives: starting
// the detached child that performs a power operation, telling a polling
// client how long it may wait, and re-reading this node's job right before a
// plug write (see jobStartedMeanwhile). Satisfied by *coordination.Manager.
type Jobs interface {
	Start(action string, args []string) (coordination.Job, error)
	StaleCeiling() time.Duration
	Read() *coordination.Job
}

// Peers is the slice of the peer set the surface drives: the pre-start busy
// check, and the peer-job fetch that makes /job and /status answer the same
// regardless of which node served them. Satisfied by *coordination.PeerSet.
type Peers interface {
	Busy(ctx context.Context, self, apiKey string) (bool, string)
	FetchJob(ctx context.Context, self, apiKey string) *coordination.Job
}

// New returns a Surface bound to its collaborators, rendering every actions
// list through actions.
//
// It panics on a nil actions: unlike the other collaborators, which a test
// serving only some routes may leave nil, every resource with controls
// renders through it, and a Surface without one is a wiring bug in the
// caller, not a state to serve in. In production actions resolves the
// composition root's Router lazily, since the Router is built from the very
// route table this Surface declares.
func New(node string, href func(string) string, inv inventory.Inventory, eng Engine, jobs Jobs, peers Peers, actions contract.ActionRenderer) *Surface {
	if actions == nil {
		panic("powerapi: New called with a nil ActionRenderer")
	}
	return &Surface{
		Node: node, Href: href, Inv: inv,
		Engine: eng, Jobs: jobs, Peers: peers,
		actions: actions,
	}
}

// JobRunning reports whether a power operation is in flight on *either* API
// node. While one is, every power action is withheld: they are not queued, and
// offering a button that can only fail is exactly what the self-describing
// design exists to avoid.
//
// The peer half matters as much as the local half. Both nodes serve the same
// hosts, so a job on either one makes a power action impossible on both.
func JobRunning(s contract.State) bool {
	return s.PeerBusy || (s.Job != nil && s.Job.State == coordination.JobRunning)
}

// Host returns the named host's status.
func Host(s contract.State, name string) (power.HostStatus, bool) {
	for _, h := range s.Hosts {
		if h.Name == name {
			return h, true
		}
	}
	return power.HostStatus{}, false
}

// clusterHostsUp reports how many hosts of the power group (f0/f1/f2,
// power.PowerGroupStatuses) answer ICMP, and how many are additionally
// reachable over SSH.
//
// The two counts answer different questions. Waking is about power, so it uses
// ping. Shutting down runs entirely over SSH -- the zusb pre-flight, the guest
// stop, the poweroff itself -- so it needs sshUp: offering "power off" to a
// host that is merely mid-boot produces a job that can only fail. That is
// exactly what happened on 2026-08-08, when f3 was shut down 48 seconds after
// waking and the pre-flight got "connection refused".
func (sf *Surface) clusterHostsUp(s contract.State) (up, sshUp, total int) {
	return hostsUp(power.PowerGroupStatuses(sf.Inv, s.Hosts))
}

// everyFHostUp counts every f-host (f0-f3, power.EveryFHostStatuses): the set
// `power all` acts on.
//
// Separate from clusterHostsUp, which excludes the standalone f3, so the two
// commands are judged against exactly the hosts they would touch -- and both
// select through the same inventory groups the engine acts on, so an
// advertised action never covers a different set of hosts than the job it
// starts.
func (sf *Surface) everyFHostUp(s contract.State) (up, sshUp, total int) {
	return hostsUp(power.EveryFHostStatuses(sf.Inv, s.Hosts))
}

// hostsUp counts statuses: all of them, those answering ICMP, and those
// reachable over SSH.
func hostsUp(statuses []power.HostStatus) (up, sshUp, total int) {
	for _, h := range statuses {
		if h.Ping {
			up++
		}
		if h.SSH {
			sshUp++
		}
	}
	return up, sshUp, len(statuses)
}

// rackBusy reports which f-hosts may still be drawing power, judged against
// this request's snapshot. This is what gates switching the fans off.
//
// The rule is power's, not this package's, and deliberately so: the CLI's
// `fans off` and the fans-off step of a shutdown decide with the same code (see
// power.RackActivity). This used to be a local loop over HostStatus.Ping, and
// it disagreed with the other two in the one way that matters -- a probe that
// could not run reads as ping=false, so on the PATH-lacking-/sbin CGI the API
// advertised fans-off without a `force` field, and executed it, against a fully
// running rack.
//
// The snapshot is a single probe per host, so this is the *cheap* half of the
// guard: it is what an advertisement is judged against, and every response can
// afford it. handleFansOff confirms with the strict half before it switches
// anything.
func (sf *Surface) rackBusy(s contract.State) power.RackActivity {
	return power.RackActivityFrom(sf.Inv, s.Hosts)
}

// acBusy reports which f-hosts may still be drawing power on the AC circuit
// (f0–f3), judged against this request's snapshot. Gates switching AC off.
func (sf *Surface) acBusy(s contract.State) power.RackActivity {
	return power.ACActivityFrom(sf.Inv, s.Hosts)
}
