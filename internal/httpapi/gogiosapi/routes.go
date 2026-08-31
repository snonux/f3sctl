package gogiosapi

import (
	"net/http"
	"strings"

	"github.com/snonux/f3sctl/internal/httpapi/contract"
)

// Statuses is the fixed set of Gogios drill-down categories, matching
// the six counts in the report's own subject headline
// ("[C:.. W:.. U:.. S:.. SU:.. OK:..]"). "stale" and "suppressed" are
// lifecycle groupings (gogios.Report.Sections.Stale/Suppressed), not
// severities -- a stale check keeps its own underlying CRITICAL/WARNING/
// UNKNOWN/OK status -- which is why checksForStatus (handlers.go) handles
// those two differently from the other four.
//
// internal/client and internal/cli keep their own copies of these six
// literals, the same deliberate duplication the route table tolerates for
// them (the client resolves from what the server advertises; these names are
// also stable links).
var Statuses = []string{"critical", "warning", "unknown", "stale", "suppressed", "ok"}

// Routes is this surface's complete slice of the API: the /monitoring
// resource and its mute pair, then the read-only alert-browse family.
func (sf *Surface) Routes() []contract.Route {
	return sf.section(append(sf.monitoringRoutes(), sf.reportRoutes()...))
}

// section stamps every route this surface declares with this package's OpenAPI
// tag (contract.SectionGogios), in one place. The package split IS the section
// split -- that is what "one domain per surface package" now means on the wire
// -- so stamping here rather than per literal means a route added to any of
// this package's route methods is sectioned correctly with no chance of being
// forgotten. See contract.Route.Section and openapi.go's sections.
func (sf *Surface) section(rs []contract.Route) []contract.Route {
	for i := range rs {
		rs[i].Section = contract.SectionGogios
	}
	return rs
}

// monitoringRoutes is the Gogios mute/unmute pair plus the resource that
// renders the mute state.
//
// Muting monitoring is deliberately decoupled from powering anything.
//
// It used to be reachable only as a step inside power-on/power-off, which
// meant a mute stranded by a timed-out un-mute could not be cleared through
// the API at all: once the fleet was up, power-on was withheld, and with it
// the only route to the marker. Gogios then stayed blind until somebody
// SSHed to both gateways by hand. A monitoring gap has to be closeable on
// its own terms.
func (sf *Surface) monitoringRoutes() []contract.Route {
	return []contract.Route{
		{
			Name: "monitoring", Title: "Gogios alerting mute",
			Method: http.MethodGet, Path: "/monitoring",
			// handleMonitoring, and the monitoring-mute/monitoring-unmute
			// actions it renders via ActionsFor, all read only
			// state.Monitoring, populated separately by enrichState.
			SkipsProbe: true,
			Handle:     sf.handleMonitoring,
		},
		{
			Name: "monitoring-unmute", Title: "Resume Gogios alerting",
			Method: http.MethodPost, Path: "/monitoring/unmute", Action: true,
			CLIVerb:    "monitoring unmute",
			SkipsProbe: true,
			Available:  func(s contract.State) bool { return Muted(s) },
			Handle:     sf.handleUnmute,
		},
		{
			Name: "monitoring-mute", Title: "Suppress Gogios alerting",
			Method: http.MethodPost, Path: "/monitoring/mute", Action: true,
			CLIVerb:    "monitoring mute",
			SkipsProbe: true,
			Available: func(s contract.State) bool {
				return s.Monitoring != nil && !Muted(s)
			},
			Handle: sf.handleMute,
		},
	}
}

// reportRoutes is the read-only Gogios alert-browse surface: an overview, a
// fixed drill-down route per Statuses category, a per-check detail lookup (by
// ?name=, not a path segment -- the composition root's Router matches exact
// paths only, and a check name may itself contain spaces/slashes), and the
// cache-clear action.
//
// This is a separate concern from monitoringRoutes: that pair mutes or
// unmutes Gogios alerting on the two gateways (a write, via the Monitor),
// while this reads the alert report itself (internal/gogios, never touching
// the engine). handleOverview cross-links to /monitoring so a client can
// reach the mute controls from the report, but the two surfaces stay
// independent.
func (sf *Surface) reportRoutes() []contract.Route {
	out := []contract.Route{
		{
			Name: "gogios", Title: "Gogios alert report",
			Method: http.MethodGet, Path: "/gogios",
			// handleOverview and every /gogios* handler below read only
			// state.Gogios/state.GogiosErr (populated by enrichState for this
			// path prefix), never state.Hosts/state.Fans.
			SkipsProbe: true,
			Handle:     sf.handleOverview,
		},
	}

	for _, status := range Statuses {
		out = append(out, contract.Route{
			Name: "gogios-" + status, Title: "Gogios " + strings.ToUpper(status) + " checks",
			Method: http.MethodGet, Path: "/gogios/" + status,
			SkipsProbe: true,
			Handle:     sf.statusHandle(status),
		})
	}

	out = append(out,
		contract.Route{
			Name: "gogios-check", Title: "One Gogios check's detail",
			Method: http.MethodGet, Path: "/gogios/check",
			// Not linked from root: its href is meaningless without ?name=,
			// which Router.Links() has no way to fill in. A client reaches it
			// through each check entity's own self link instead (see
			// checkEntity, handlers.go) -- handleOverview deliberately never
			// links to the bare route either.
			NoRootLink: true,
			SkipsProbe: true,
			Handle:     sf.handleCheck,
		},
		contract.Route{
			Name: "gogios-cache-clear", Title: "Clear the cached Gogios report",
			Method: http.MethodPost, Path: "/gogios/cache/clear", Action: true,
			CLIVerb: "gogios cache clear",
			// Always advertised: unlike the power/fan/monitoring actions, there
			// is no state in which clearing the cache would fail to make sense.
			SkipsProbe: true,
			Handle:     sf.handleClearCache,
		},
	)
	return out
}
