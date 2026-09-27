package gogiosapi

import (
	"net/http"
	"strings"

	"github.com/snonux/f3sctl/internal/gogios"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
)

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
//
// NoRootLink: monitoring is reached through the Gogios folder (/gogios),
// which links it and carries the mute pair's actions alongside the report
// browse -- it is a Gogios concern, not an overview-level control, and
// keeping it off the root is what lets the root's menu be two folders and
// the read-only resources (see contract.Route.NoRootLink).
func (sf *Surface) monitoringRoutes() []contract.Route {
	return []contract.Route{
		{
			Name: "monitoring", Title: "Gogios alerting mute",
			Method: http.MethodGet, Path: "/monitoring",
			// handleMonitoring, and the monitoring-mute/monitoring-unmute
			// actions it renders via ActionsFor, all read only
			// Monitoring(state), which NeedMonitoring has fetched.
			NoRootLink: true,
			SkipsProbe: true,
			Needs:      contract.Needs{NeedMonitoring},
			Handle:     sf.handleMonitoring,
		},
		{
			Name: "monitoring-unmute", Title: "Resume Gogios alerting",
			Method: http.MethodPost, Path: "/monitoring/unmute", Action: true,
			CLIVerb:    "monitoring unmute",
			Errors:     []contract.ErrorResponse{gatewayWriteFailed},
			SkipsProbe: true,
			Needs:      contract.Needs{NeedMonitoring},
			Available:  func(s contract.State) bool { return Muted(s) },
			Handle:     sf.handleUnmute,
		},
		{
			Name: "monitoring-mute", Title: "Suppress Gogios alerting",
			Method: http.MethodPost, Path: "/monitoring/mute", Action: true,
			CLIVerb:    "monitoring mute",
			Errors:     []contract.ErrorResponse{gatewayWriteFailed},
			SkipsProbe: true,
			Needs:      contract.Needs{NeedMonitoring},
			// Not !Muted: a partial mute leaves a gateway alerting (or
			// unknown), and the mute is what finishes it -- so both
			// actions can be offered at once (see NotAllMuted).
			Available: NotAllMuted,
			Handle:    sf.handleMute,
		},
	}
}

// reportRoutes is the read-only Gogios alert-browse surface: an overview, a
// fixed drill-down route per gogios.Statuses category, a per-check lookup (by
// ?name=, not a path segment -- the composition root's Router matches exact
// paths only, and a check name may itself contain spaces/slashes), and the
// cache-clear action.
//
// This is a separate concern from monitoringRoutes: that pair mutes or
// unmutes Gogios alerting on the two gateways (a write, via the Monitor),
// while this reads the alert report itself (internal/gogios, never touching
// the engine). handleOverview cross-links to /monitoring so a client can
// reach the mute controls from the report, and also advertises the mute pair
// itself (see there) so the folder holds the whole Gogios concern -- the two
// surfaces stay independent underneath.
//
// Every drill-down is NoRootLink: they are reachable through the /gogios
// folder (the overview links each by rel), which is what keeps the root's
// overview menu two folders and the read-only resources instead of six
// drill-down entries (see contract.Route.NoRootLink).
func (sf *Surface) reportRoutes() []contract.Route {
	out := []contract.Route{sf.overviewRoute()}
	out = append(out, sf.drillDownRoutes()...)
	return append(out, sf.checkRoute(), sf.cacheClearRoute())
}

// overviewRoute is the /gogios folder itself: the report overview, which
// links every drill-down and carries the mute pair alongside the cache clear.
func (sf *Surface) overviewRoute() contract.Route {
	return contract.Route{
		Name: "gogios", Title: "Gogios status and alerting",
		Method: http.MethodGet, Path: "/gogios",
		// handleOverview and every /gogios* handler below read only
		// Report(state) (NeedReport), never the power surface's fleet
		// snapshot. The folder also advertises the mute pair, judged on
		// the gateway mute -- hence NeedMonitoring too.
		SkipsProbe: true,
		Needs:      contract.Needs{NeedReport, NeedMonitoring},
		Handle:     sf.handleOverview,
	}
}

// drillDownRoutes is one read-only route per gogios.Statuses category, in
// that list's order.
func (sf *Surface) drillDownRoutes() []contract.Route {
	statuses := gogios.Statuses()
	out := make([]contract.Route, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, contract.Route{
			Name: "gogios-" + status, Title: "Gogios " + strings.ToUpper(status) + " checks",
			Method: http.MethodGet, Path: "/gogios/" + status,
			// NoRootLink: reached through the /gogios folder, which links
			// each category by rel -- see reportRoutes' doc comment.
			NoRootLink: true,
			SkipsProbe: true,
			Needs:      contract.Needs{NeedReport},
			Handle:     sf.statusHandle(status),
		})
	}
	return out
}

// checkRoute is the per-check lookup, addressed by ?name= (see reportRoutes).
func (sf *Surface) checkRoute() contract.Route {
	return contract.Route{
		Name: "gogios-check", Title: "One Gogios check's detail",
		Method: http.MethodGet, Path: "/gogios/check",
		Query: []contract.QueryParam{{
			Name: "name", Required: true,
			Description: "The check's exact name, as in its entity's \"name\" property.",
		}},
		Errors: []contract.ErrorResponse{
			{Status: http.StatusNotFound, Description: "no check has that name"},
			{Status: http.StatusBadGateway, Description: "the Gogios report could not be fetched"},
		},
		// Not linked from root: its href is meaningless without ?name=,
		// which Router.Links() has no way to fill in. A client reaches it
		// through each check entity's own self link instead (see
		// checkEntity, handlers.go) -- handleOverview deliberately never
		// links to the bare route either.
		NoRootLink: true,
		SkipsProbe: true,
		Needs:      contract.Needs{NeedReport},
		Handle:     sf.handleCheck,
	}
}

// cacheClearRoute is the one write in the report family: dropping the
// cached Gogios report so the next read fetches it afresh.
func (sf *Surface) cacheClearRoute() contract.Route {
	return contract.Route{
		Name: "gogios-cache-clear", Title: "Clear the cached Gogios report",
		Method: http.MethodPost, Path: "/gogios/cache/clear", Action: true,
		CLIVerb: "gogios cache clear",
		Errors: []contract.ErrorResponse{{
			Status: http.StatusInternalServerError, Description: "the on-disk report cache could not be cleared",
		}},
		// Always advertised: unlike the power/fan/monitoring actions, there
		// is no state in which clearing the cache would fail to make sense.
		SkipsProbe: true,
		// NeedMonitoring: the response is the re-rendered /gogios folder,
		// mute pair included (see handleClearCache). Not NeedReport: the
		// handler re-reads the report after clearing it, so a report
		// fetched before the clear would only be thrown away.
		Needs:  contract.Needs{NeedMonitoring},
		Handle: sf.handleClearCache,
	}
}

// gatewayWriteFailed is the 502 the mute pair answers when changing the
// marker on the gateways fails (setMute). Declared on the routes for the
// OpenAPI document; see contract.Route.Errors.
var gatewayWriteFailed = contract.ErrorResponse{
	Status:      http.StatusBadGateway,
	Description: "the mute marker could not be changed on the gateways",
}
