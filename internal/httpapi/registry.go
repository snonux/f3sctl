package httpapi

import (
	"context"
	"net/http"

	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// jobPath is PATH_INFO for the job route, as CGI sees it -- i.e. without the
// SCRIPT_NAME mount point. This is a different value from
// config.Config.PeerJobPath, which is the *full* URL path a peer's job is
// fetched from over a real HTTP connection; see that field's doc comment.
//
// Named so Server.serve can exclude it from triggering a peer check of its
// own: a peer check that recurses into another peer check makes each node's
// answer wait on the other's.
const jobPath = "/job"

// State is a snapshot of the world, taken once per request before anything is
// rendered.
//
// Availability predicates read only from here, never from live probes of their
// own, so every action in a single response is judged against the same instant.
type State struct {
	Hosts   []power.HostStatus
	Fans    power.FansState
	FansErr error
	Job     *coordination.Job
	// Monitoring is the per-gateway Gogios mute state. Nil when it was not
	// collected for this request: reading it costs two SSH round trips to the
	// gateways, so only the routes that render it pay for it.
	Monitoring []power.GatewayMute

	// PeerBusy reports whether the *other* API node is running a job.
	//
	// relayd load-balances pi0 and pi1, so a job started on one node is
	// invisible to the other's local state. Without this, the idle node cheerfully
	// advertises power-off while the busy node is mid-shutdown, and every one of
	// those actions 409s the instant a client tries it -- which is exactly the
	// "read the 409, not the response" behaviour this API exists to avoid.
	PeerBusy bool
}

// monitoringMuted reports whether Gogios is muted on at least one gateway.
func (s State) monitoringMuted() bool { return power.AnyMuted(s.Monitoring) }

// jobRunning reports whether a power operation is in flight on *either* API
// node. While one is, every power action is withheld: they are not queued, and
// offering a button that can only fail is exactly what the self-describing
// design exists to avoid.
//
// The peer half matters as much as the local half. Both nodes serve the same
// hosts, so a job on either one makes a power action impossible on both.
func (s State) jobRunning() bool {
	return s.PeerBusy || (s.Job != nil && s.Job.State == coordination.JobRunning)
}

// host returns the named host's status.
func (s State) host(name string) (power.HostStatus, bool) {
	for _, h := range s.Hosts {
		if h.Name == name {
			return h, true
		}
	}
	return power.HostStatus{}, false
}

// clusterHostsUp reports how many of f0/f1/f2 answer ICMP, and how many are
// additionally reachable over SSH.
//
// The two counts answer different questions. Waking is about power, so it uses
// ping. Shutting down runs entirely over SSH -- the zusb pre-flight, the guest
// stop, the poweroff itself -- so it needs sshUp: offering "power off" to a
// host that is merely mid-boot produces a job that can only fail. That is
// exactly what happened on 2026-08-08, when f3 was shut down 48 seconds after
// waking and the pre-flight got "connection refused".
func (s State) clusterHostsUp() (up, sshUp, total int) {
	for _, h := range s.Hosts {
		if h.Role != "f" || h.Name == "f3" {
			continue
		}
		total++
		if h.Ping {
			up++
		}
		if h.SSH {
			sshUp++
		}
	}
	return up, sshUp, total
}

// everyFHostUp counts f0-f3, f3 included: the set `power all` acts on.
//
// Separate from clusterHostsUp, which deliberately excludes f3, so the two
// commands are judged against exactly the hosts they would touch.
func (s State) everyFHostUp() (up, sshUp, total int) {
	for _, h := range s.Hosts {
		if h.Role != "f" {
			continue
		}
		total++
		if h.Ping {
			up++
		}
		if h.SSH {
			sshUp++
		}
	}
	return up, sshUp, total
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
func (s State) rackBusy() power.RackActivity {
	return power.RackActivityFrom(s.Hosts)
}

// route is one entry in the API surface.
//
// Router, Siren rendering and the OpenAPI document are all generated from this
// one declaration. That is deliberate: three hand-maintained lists of the same
// endpoints would drift, and a self-describing API that lies is worse than one
// that never claimed to describe itself.
type route struct {
	// Name is the stable identifier clients match on.
	Name string
	// Title is human-readable text for the action or link.
	Title string
	// Method is the HTTP method.
	Method string
	// Path is the exact PATH_INFO this route answers on.
	Path string
	// Action marks a state change (rendered in "actions"). Routes without it
	// are resources, rendered in "links".
	Action bool
	// CLIVerb is the exact CLI words that invoke this action, e.g. "power on"
	// or "power f1 on". It is the single declaration of the "power off" <->
	// action-name <-> job-run-argv contract: jobArgs (handlers.go) splits it
	// into the detached child's argv, and Router.action carries it over the
	// wire on the Siren action so the remote client (internal/client/run.go)
	// can match a typed command to an action by reading what the server just
	// advertised, rather than the two of them independently guessing a name
	// from the same string. Left empty on GET/link routes, which are not
	// invoked by a CLI verb at all. See sy0's annotation for the drift this
	// replaced -- three hand-written maps of the same strings that could
	// silently disagree the moment a new action was added to only one of them.
	CLIVerb string
	// JobAction is the identifier jobArgs and coordination.Manager's Job.Action
	// match this route's job against, when it differs from Name. It differs
	// only for power-on/power-off: their job identifier ("on"/"off") predates
	// the route registry and is part of the documented job wire contract
	// (docs/CLIENT.md's `"action": "off"`), so it is kept rather than renamed
	// to match Name ("power-on"/"power-off"). Empty means "use Name" -- true
	// for every other action, where the two have always been the same string.
	JobAction string
	// Available reports whether this route may be used given the current
	// state. Resources are always available; actions are advertised only when
	// they would succeed.
	Available func(State) bool
	// Fields describes the action's parameters for the current state.
	Fields func(State) []Field
	// Handle performs the route. ctx is the request's context, taken from
	// serve() and carried through to every Shelly/SSH/Gogios call a handler
	// makes, so a slow backend is bounded by the request's own deadline
	// rather than by a fresh context.Background() with no deadline at all.
	// It is not threaded into coordination.Manager.Start's detached child
	// spawn -- that job deliberately outlives the CGI request that started
	// it, so it must not be cancelled when this context is (see
	// handleAction and internal/coordination/run.go).
	Handle func(*Server, context.Context, State, request) (Entity, int, error)
}

// routes is the complete API surface.
func routes() []route {
	out := []route{
		{
			Name: "self", Title: "API root",
			Method: http.MethodGet, Path: "/",
			Handle: (*Server).handleRoot,
		},
		{
			Name: "status", Title: "Host and rack status",
			Method: http.MethodGet, Path: "/status",
			Handle: (*Server).handleStatus,
		},
		{
			Name: "fans", Title: "Rack fan plug",
			Method: http.MethodGet, Path: "/fans",
			Handle: (*Server).handleFans,
		},
		{
			Name: "job", Title: "Current or last power job",
			Method: http.MethodGet, Path: jobPath,
			Handle: (*Server).handleJob,
		},
		{
			Name: "monitoring", Title: "Gogios alerting mute",
			Method: http.MethodGet, Path: "/monitoring",
			Handle: (*Server).handleMonitoring,
		},
		{
			Name: "describedby", Title: "OpenAPI description",
			Method: http.MethodGet, Path: "/openapi.json",
			Handle: (*Server).handleOpenAPI,
		},

		{
			Name: "power-on", Title: "Power on f0/f1/f2",
			Method: http.MethodPost, Path: "/power/on", Action: true,
			CLIVerb: "power on", JobAction: "on",
			// Offered only when something is actually off. When the whole
			// group already answers, waking it again is a no-op that would
			// still cost the caller a job slot.
			Available: func(s State) bool {
				up, _, total := s.clusterHostsUp()
				return !s.jobRunning() && up < total
			},
			Handle: handleAction("on"),
		},
		{
			Name: "power-off", Title: "Power off f0/f1/f2",
			Method: http.MethodPost, Path: "/power/off", Action: true,
			CLIVerb: "power off", JobAction: "off",
			// Requires SSH, not just ping: the whole shutdown runs over
			// SSH, so a host that is only mid-boot cannot be shut down and
			// must not be offered as if it could.
			Available: func(s State) bool {
				_, sshUp, _ := s.clusterHostsUp()
				return !s.jobRunning() && sshUp > 0
			},
			Handle: handleAction("off"),
		},
		{
			Name: "all-on", Title: "Power on every f-host (f0-f3)",
			Method: http.MethodPost, Path: "/power/all/on", Action: true,
			CLIVerb: "power all on",
			Available: func(s State) bool {
				up, _, total := s.everyFHostUp()
				return !s.jobRunning() && up < total
			},
			Handle: handleAction("all-on"),
		},
		{
			Name: "all-off", Title: "Power off every f-host (f0-f3)",
			Method: http.MethodPost, Path: "/power/all/off", Action: true,
			CLIVerb: "power all off",
			// SSH, not ping, for the same reason as power-off: the whole
			// shutdown runs over SSH.
			Available: func(s State) bool {
				_, sshUp, _ := s.everyFHostUp()
				return !s.jobRunning() && sshUp > 0
			},
			Handle: handleAction("all-off"),
		},
	}

	// Per-host actions for every f-host, so any one of f0-f3 can be powered
	// independently of the cluster-wide pair above.
	//
	// Generated from the inventory rather than written out four times: adding
	// a host to the inventory should not mean remembering to add two routes,
	// two OpenAPI entries and two client mappings by hand.
	for _, h := range inventory.Default().ByRole(inventory.RoleF) {
		out = append(out, hostRoutes(h.Name)...)
	}

	out = append(out, []route{
		{
			Name: "fans-on", Title: "Switch the rack fans on",
			Method: http.MethodPost, Path: "/fans/on", Action: true,
			CLIVerb: "fans on",
			// Unavailable when the plug cannot be read: without a read-back
			// there is no way to report truthfully whether it worked.
			Available: func(s State) bool { return s.FansErr == nil && !s.Fans.On },
			Handle:    (*Server).handleFansOn,
		},
		{
			Name: "fans-off", Title: "Switch the rack fans off",
			Method: http.MethodPost, Path: "/fans/off", Action: true,
			CLIVerb:   "fans off",
			Available: func(s State) bool { return s.FansErr == nil && s.Fans.On },
			// The guard is expressed as a field rather than documented as a
			// rule: while the rack may be busy the client is handed a
			// confirmation toggle with the reason in its title, and when the
			// rack is cold the field simply is not there.
			//
			// The reason is spelled out rather than fixed, because "f3 still
			// running" and "f3 could not be probed, so assumed running" call for
			// very different reactions from whoever is reading it, and clients
			// are told to render this title verbatim (docs/CLIENT.md §6).
			Fields: func(s State) []Field {
				busy := s.rackBusy()
				if !busy.Busy() {
					return nil
				}
				return []Field{{
					Name:     "force",
					Type:     "checkbox",
					Value:    false,
					Required: true,
					Title: "Hosts may still be running (" + busy.Why() + "): the rack fans " +
						"keep them cool, so switching the plug off now risks overheating. " +
						"Confirm to proceed.",
				}}
			},
			Handle: (*Server).handleFansOff,
		},

		// Muting monitoring is deliberately decoupled from powering anything.
		//
		// It used to be reachable only as a step inside power-on/power-off,
		// which meant a mute stranded by a timed-out un-mute could not be
		// cleared through the API at all: once the fleet was up, power-on was
		// withheld, and with it the only route to the marker. Gogios then
		// stayed blind until somebody SSHed to both gateways by hand. A
		// monitoring gap has to be closeable on its own terms.
		{
			Name: "monitoring-unmute", Title: "Resume Gogios alerting",
			Method: http.MethodPost, Path: "/monitoring/unmute", Action: true,
			CLIVerb:   "monitoring unmute",
			Available: func(s State) bool { return s.monitoringMuted() },
			Handle:    (*Server).handleUnmute,
		},
		{
			Name: "monitoring-mute", Title: "Suppress Gogios alerting",
			Method: http.MethodPost, Path: "/monitoring/mute", Action: true,
			CLIVerb: "monitoring mute",
			Available: func(s State) bool {
				return s.Monitoring != nil && !s.monitoringMuted()
			},
			Handle: (*Server).handleMute,
		},
	}...)

	return out
}

// hostRoutes builds the on/off pair for one f-host.
//
// Powering a single host is not the same operation as powering the group: it
// leaves the rack fans and the Gogios mute alone, because one host going down
// does not mean the rack is idle, and the muted checks belong to the cluster
// as a whole.
//
// Note for f0: it holds the CARP storage VIP, so taking it down on its own
// fails that over to f1 -- which is what CARP is for, and is fine as long as
// f1 stays up. The danger is only in shutting f0 down and then f1 moments
// later, which is why the cluster-wide sequence orders f0 last.
func hostRoutes(name string) []route {
	return []route{
		{
			Name: name + "-on", Title: "Power on " + name,
			Method: http.MethodPost, Path: "/power/" + name + "/on", Action: true,
			CLIVerb: "power " + name + " on",
			Available: func(s State) bool {
				h, ok := s.host(name)
				return ok && !s.jobRunning() && !h.Ping
			},
			Handle: handleAction(name + "-on"),
		},
		{
			Name: name + "-off", Title: "Power off " + name,
			Method: http.MethodPost, Path: "/power/" + name + "/off", Action: true,
			CLIVerb: "power " + name + " off",
			// SSH, not ping: the shutdown runs over SSH, so a host that is
			// only mid-boot cannot be shut down and must not be offered as if
			// it could.
			Available: func(s State) bool {
				h, ok := s.host(name)
				return ok && !s.jobRunning() && h.SSH
			},
			Handle: handleAction(name + "-off"),
		},
	}
}

// jobAction returns the identifier jobArgs and the started job's Action
// field match this route against: JobAction when the route declares one
// (power-on/power-off only -- see JobAction's doc comment), Name otherwise.
func (r route) jobAction() string {
	if r.JobAction != "" {
		return r.JobAction
	}
	return r.Name
}

// available reports whether a route may be used now.
func (r route) available(s State) bool {
	if r.Available == nil {
		return true
	}
	return r.Available(s)
}

// fields returns the route's parameters for the current state.
func (r route) fields(s State) []Field {
	if r.Fields == nil {
		return nil
	}
	return r.Fields(s)
}
