package httpapi

import (
	"net/http"

	"github.com/snonux/f3sctl/internal/inventory"
	"github.com/snonux/f3sctl/internal/power"
)

// State is a snapshot of the world, taken once per request before anything is
// rendered.
//
// Availability predicates read only from here, never from live probes of their
// own, so every action in a single response is judged against the same instant.
type State struct {
	Hosts   []power.HostStatus
	Fans    power.FansState
	FansErr error
	Job     *Job
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
	return s.PeerBusy || (s.Job != nil && s.Job.State == JobRunning)
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

// anyFHostUp reports whether anything in the rack is drawing power. This is
// what gates switching the fans off.
func (s State) anyFHostUp() bool {
	for _, h := range s.Hosts {
		if h.Role == "f" && h.Ping {
			return true
		}
	}
	return false
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
	// Available reports whether this route may be used given the current
	// state. Resources are always available; actions are advertised only when
	// they would succeed.
	Available func(State) bool
	// Fields describes the action's parameters for the current state.
	Fields func(State) []Field
	// Handle performs the route.
	Handle func(*Server, State, request) (Entity, int, error)
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
			Available: func(s State) bool {
				up, _, total := s.everyFHostUp()
				return !s.jobRunning() && up < total
			},
			Handle: handleAction("all-on"),
		},
		{
			Name: "all-off", Title: "Power off every f-host (f0-f3)",
			Method: http.MethodPost, Path: "/power/all/off", Action: true,
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
			// Unavailable when the plug cannot be read: without a read-back
			// there is no way to report truthfully whether it worked.
			Available: func(s State) bool { return s.FansErr == nil && !s.Fans.On },
			Handle:    (*Server).handleFansOn,
		},
		{
			Name: "fans-off", Title: "Switch the rack fans off",
			Method: http.MethodPost, Path: "/fans/off", Action: true,
			Available: func(s State) bool { return s.FansErr == nil && s.Fans.On },
			// The guard is expressed as a field rather than documented as a
			// rule: while a host is up the client is handed a confirmation
			// toggle with the reason in its title, and when the rack is cold
			// the field simply is not there.
			Fields: func(s State) []Field {
				if !s.anyFHostUp() {
					return nil
				}
				return []Field{{
					Name:     "force",
					Type:     "checkbox",
					Value:    false,
					Required: true,
					Title: "Hosts are still running: the rack fans keep them cool, " +
						"so switching the plug off now risks overheating. Confirm to proceed.",
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
			Available: func(s State) bool { return s.monitoringMuted() },
			Handle:    (*Server).handleUnmute,
		},
		{
			Name: "monitoring-mute", Title: "Suppress Gogios alerting",
			Method: http.MethodPost, Path: "/monitoring/mute", Action: true,
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
			Available: func(s State) bool {
				h, ok := s.host(name)
				return ok && !s.jobRunning() && !h.Ping
			},
			Handle: handleAction(name + "-on"),
		},
		{
			Name: name + "-off", Title: "Power off " + name,
			Method: http.MethodPost, Path: "/power/" + name + "/off", Action: true,
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

// lookup finds the route serving method and path.
func lookup(method, path string) (route, bool) {
	for _, r := range routes() {
		if r.Method == method && r.Path == path {
			return r, true
		}
	}
	return route{}, false
}

// pathExists reports whether any route serves this path, regardless of method.
// It is what separates a 404 from a 405.
func pathExists(path string) bool {
	for _, r := range routes() {
		if r.Path == path {
			return true
		}
	}
	return false
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
