package powerapi

import (
	"net/http"

	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/inventory"
)

// Routes is this surface's complete slice of the API: the two read-only
// status/job resources, the rack-fan resource, the cluster-wide power pair,
// the every-f-host pair, a power pair per f-host, and the fan plug's own
// on/off actions.
//
// The per-host routes are generated from sf.Inv, the inventory this surface
// was configured with, NOT from inventory.Default(): power.New, ProbeAll and
// every other site resolve the inventory through injected config.Config, so
// the route table must too. Reading the compiled-in global here would build
// the action routes from a different inventory than the one the engine acts
// on -- an operator who overrode "inventory" in f3sctl.json would get new
// hosts probed and shown in /status but no action route for them, and removed
// hosts would keep a dead route -- and JobArgs derives the detached child's
// argv from these same routes, so the CLI<->API<->job contract would miss the
// host as well.
func (sf *Surface) Routes() []contract.Route {
	return sf.section(
		sf.resourceRoutes(),
		sf.clusterRoutes(),
		sf.allHostsRoutes(),
		sf.hostsRoutes(),
		sf.fanRoutes(),
	)
}

// section stamps every route this surface declares with this package's OpenAPI
// tag (contract.SectionPower), in one place. The package split IS the section
// split -- that is what "one domain per surface package" means on the wire --
// so stamping here means a route added to any of this package's route methods
// is sectioned correctly with no chance of being forgotten. See
// contract.Route.Section and openapi.go's sections.
func (sf *Surface) section(groups ...[]contract.Route) []contract.Route {
	var out []contract.Route
	for _, g := range groups {
		for i := range g {
			g[i].Section = contract.SectionPower
		}
		out = append(out, g...)
	}
	return out
}

// hostsRoutes is the per-host on/off pair for every f-host, so any one of
// f0-f3 can be powered independently of the cluster-wide pair above.
//
// Generated from the inventory rather than written out four times: adding
// a host to the inventory should not mean remembering to add two routes,
// two OpenAPI entries and two client mappings by hand.
func (sf *Surface) hostsRoutes() []contract.Route {
	var out []contract.Route
	for _, h := range sf.Inv.ByRole(inventory.RoleF) {
		out = append(out, sf.hostRoutes(h.Name)...)
	}
	return out
}

// resourceRoutes is the read-only power resources: the power section folder,
// the status overview, the current-or-last job, and the rack-fan plug.
// Navigable resources rendered in "links", never in "actions" (see
// TestGETRoutesAreLinksNotActions). None of these takes a CLIVerb -- they are
// followed by relation, not invoked by a CLI verb.
//
// The /power folder is section navigation and the root's "power" rel: a GET
// resource whose actions list is every power operation possible right now
// (see handlePowerFolder), replacing the flat actions list the ROOT used to
// carry. It is NOT SkipsProbe -- its actions are judged on fleet state -- so
// unlike the root itself a folder render still pays the probe.
func (sf *Surface) resourceRoutes() []contract.Route {
	return []contract.Route{
		{
			Name: "power", Title: "Power control",
			Method: http.MethodGet, Path: "/power",
			Handle: sf.handlePowerFolder,
		},
		{
			Name: "status", Title: "Host and rack status",
			Method: http.MethodGet, Path: StatusPath,
			Handle: sf.handleStatus,
		},
		{
			Name: "job", Title: "Current or last power job",
			Method: http.MethodGet, Path: JobPath,
			// handleJob renders only state.Job, which snapshot() always reads
			// regardless of this flag (it is a cheap local disk read).
			SkipsProbe: true,
			Handle:     sf.handleJob,
		},
		{
			Name: "fans", Title: "Rack fan plug",
			Method: http.MethodGet, Path: "/fans",
			Handle: sf.handleFans,
		},
	}
}

// clusterRoutes is the cluster-wide power pair: f0/f1/f2 only, f3 excluded.
// The every-f-host pair lives in allHostsRoutes, and per-host actions in
// hostRoutes, generated from the inventory.
func (sf *Surface) clusterRoutes() []contract.Route {
	return []contract.Route{
		{
			Name: "power-on", Title: "Power on f0/f1/f2",
			Method: http.MethodPost, Path: "/power/on", Action: true,
			CLIVerb: "power on", JobActionName: "on",
			// Offered only when something is actually off. When the whole
			// group already answers, waking it again is a no-op that would
			// still cost the caller a job slot.
			Available: func(s contract.State) bool {
				up, _, total := ClusterHostsUp(s)
				return !JobRunning(s) && up < total
			},
			Handle: sf.action("on"),
		},
		{
			Name: "power-off", Title: "Power off f0/f1/f2",
			Method: http.MethodPost, Path: "/power/off", Action: true,
			CLIVerb: "power off", JobActionName: "off",
			// Requires SSH, not just ping: the whole shutdown runs over
			// SSH, so a host that is only mid-boot cannot be shut down and
			// must not be offered as if it could.
			Available: func(s contract.State) bool {
				_, sshUp, _ := ClusterHostsUp(s)
				return !JobRunning(s) && sshUp > 0
			},
			Handle: sf.action("off"),
		},
	}
}

// allHostsRoutes is the every-f-host pair: f0-f3, f3 included. See
// clusterRoutes for the cluster-only pair this complements.
func (sf *Surface) allHostsRoutes() []contract.Route {
	return []contract.Route{
		{
			Name: "all-on", Title: "Power on every f-host (f0-f3)",
			Method: http.MethodPost, Path: "/power/all/on", Action: true,
			CLIVerb: "power all on",
			Available: func(s contract.State) bool {
				up, _, total := EveryFHostUp(s)
				return !JobRunning(s) && up < total
			},
			Handle: sf.action("all-on"),
		},
		{
			Name: "all-off", Title: "Power off every f-host (f0-f3)",
			Method: http.MethodPost, Path: "/power/all/off", Action: true,
			CLIVerb: "power all off",
			// SSH, not ping, for the same reason as power-off: the whole
			// shutdown runs over SSH.
			Available: func(s contract.State) bool {
				_, sshUp, _ := EveryFHostUp(s)
				return !JobRunning(s) && sshUp > 0
			},
			Handle: sf.action("all-off"),
		},
	}
}

// fanRoutes is the rack-fan plug's on/off pair.
func (sf *Surface) fanRoutes() []contract.Route {
	return []contract.Route{
		{
			Name: "fans-on", Title: "Switch the rack fans on",
			Method: http.MethodPost, Path: "/fans/on", Action: true,
			CLIVerb: "fans on",
			// Unavailable when the plug cannot be read: without a read-back
			// there is no way to report truthfully whether it worked.
			Available: func(s contract.State) bool { return s.FansErr == nil && !s.Fans.On },
			Handle:    sf.handleFansOn,
		},
		{
			Name: "fans-off", Title: "Switch the rack fans off",
			Method: http.MethodPost, Path: "/fans/off", Action: true,
			CLIVerb:   "fans off",
			Available: func(s contract.State) bool { return s.FansErr == nil && s.Fans.On },
			// The guard is expressed as a field rather than documented as a
			// rule: while the rack may be busy the client is handed a
			// confirmation toggle with the reason in its title, and when the
			// rack is cold the field simply is not there.
			//
			// The reason is spelled out rather than fixed, because "f3 still
			// running" and "f3 could not be probed, so assumed running" call for
			// very different reactions from whoever is reading it, and clients
			// are told to render this title verbatim (docs/CLIENT.md §6).
			Fields: func(s contract.State) []contract.Field {
				busy := RackBusy(s)
				if !busy.Busy() {
					return nil
				}
				return []contract.Field{{
					Name:     "force",
					Type:     "checkbox",
					Value:    false,
					Required: true,
					Title: "Hosts may still be running (" + busy.Why() + "): the rack fans " +
						"keep them cool, so switching the plug off now risks overheating. " +
						"Confirm to proceed.",
				}}
			},
			Handle: sf.handleFansOff,
		},
	}
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
func (sf *Surface) hostRoutes(name string) []contract.Route {
	return []contract.Route{
		{
			Name: name + "-on", Title: "Power on " + name,
			Method: http.MethodPost, Path: "/power/" + name + "/on", Action: true,
			CLIVerb: "power " + name + " on",
			Available: func(s contract.State) bool {
				h, ok := Host(s, name)
				return ok && !JobRunning(s) && !h.Ping
			},
			Handle: sf.action(name + "-on"),
		},
		{
			Name: name + "-off", Title: "Power off " + name,
			Method: http.MethodPost, Path: "/power/" + name + "/off", Action: true,
			CLIVerb: "power " + name + " off",
			// SSH, not ping: the shutdown runs over SSH, so a host that is
			// only mid-boot cannot be shut down and must not be offered as if
			// it could.
			Available: func(s contract.State) bool {
				h, ok := Host(s, name)
				return ok && !JobRunning(s) && h.SSH
			},
			Handle: sf.action(name + "-off"),
		},
	}
}
