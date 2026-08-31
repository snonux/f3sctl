package gogiosapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/snonux/f3sctl/internal/gogios"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
)

// handleMonitoring renders whether Gogios alerting is muted, per gateway.
//
// The mute is a marker file on each gateway, set while the cluster is taken
// down on purpose so a deliberate outage does not page. Showing it as a
// resource is what makes a *stranded* mute visible: nothing else in the API
// would reveal that alerting is off.
func (sf *Surface) handleMonitoring(_ context.Context, state contract.State, _ contract.Request) (contract.Entity, int, error) {
	gateways := make([]contract.Entity, 0, len(state.Monitoring))
	for _, gw := range state.Monitoring {
		props := map[string]any{"name": gw.Name}
		if gw.Err != nil {
			// Unreachable is not the same as un-muted, and reporting it as
			// "alerting is fine" would be the more dangerous lie of the two.
			props["error"] = gw.Err.Error()
		} else {
			props["muted"] = gw.Muted
		}
		gateways = append(gateways, contract.Entity{
			Class:      []string{"gateway"},
			Rel:        []string{"item"},
			Properties: props,
		})
	}

	return contract.Entity{
		Class: []string{"monitoring"},
		Title: "Gogios alerting mute",
		Properties: map[string]any{
			"muted": Muted(state),
			"node":  sf.Node,
		},
		Entities: gateways,
		Links: []contract.Link{
			{Rel: []string{"self"}, Href: sf.Href("/monitoring")},
			{Rel: []string{"up"}, Href: sf.Href("/")},
		},
		Actions: sf.actionsFor(state, "monitoring-mute", "monitoring-unmute"),
	}, http.StatusOK, nil
}

// actionsFor renders this resource's own actions, nil-safely: the table can
// be declared (and its predicates tested) without the composition root's
// Router attached, and a route declaration never needs to render an actions
// list -- only serving one does. The injected ActionsFor (see Surface) is
// what keeps the Siren action shape in exactly one place, the composition
// root's Router, so a surface cannot grow its own action rendering that
// drifts from the rest of the API's.
func (sf *Surface) actionsFor(state contract.State, names ...string) []contract.Action {
	if sf.ActionsFor == nil {
		return nil
	}
	return sf.ActionsFor(state, names...)
}

func (sf *Surface) handleUnmute(ctx context.Context, state contract.State, req contract.Request) (contract.Entity, int, error) {
	return sf.setMute(ctx, state, req, false)
}

func (sf *Surface) handleMute(ctx context.Context, state contract.State, req contract.Request) (contract.Entity, int, error) {
	return sf.setMute(ctx, state, req, true)
}

// setMute changes the marker on both gateways and re-reads it.
//
// Re-reading rather than assuming: these are two independent gateways reached
// over SSH, and a partial success -- one muted, one not -- is a real outcome
// that the caller needs to see rather than infer from a 200. ctx comes from
// serve() rather than a fresh context.Background(), so the two SSH round
// trips this makes are bounded by the request's own deadline like every other
// backend call a handler makes.
func (sf *Surface) setMute(ctx context.Context, state contract.State, _ contract.Request, mute bool) (contract.Entity, int, error) {
	var err error
	if mute {
		err = sf.Monitor.MuteGogios(ctx, io.Discard)
	} else {
		err = sf.Monitor.UnmuteNow(ctx, io.Discard)
	}
	if err != nil {
		return contract.Entity{}, http.StatusBadGateway, err
	}

	state.Monitoring = sf.Monitor.MonitoringStatus(ctx)
	return sf.handleMonitoring(ctx, state, contract.Request{})
}

// handleOverview renders the Gogios folder: the alert report overview -- the
// subject headline, the six summary counts, when it was last updated -- plus
// the links to each drill-down category and to /monitoring (the separate
// mute resource), and the whole family's controls: the cache-clear action
// and the mute/unmute pair. It is one of the two section folders (the other
// is powerapi's /power), and it is what the root's "gogios" rel resolves to --
// a browser's Gogios menu opens here rather than across six root-level
// drill-down entries and a mute resource it would have to find separately.
//
// A fetch failure is reported as an "error" property with 200 OK, not a
// non-2xx status -- the same convention handleMonitoring uses for a backend
// that could not be reached: the resource itself (the report, or whether it
// is currently obtainable) is what a client is asking about, and that is
// still meaningful to render even when the answer is "unreachable".
func (sf *Surface) handleOverview(_ context.Context, state contract.State, _ contract.Request) (contract.Entity, int, error) {
	links := []contract.Link{
		{Rel: []string{"self"}, Href: sf.Href("/gogios")},
		{Rel: []string{"up"}, Href: sf.Href("/")},
		{Rel: []string{"monitoring"}, Href: sf.Href("/monitoring")},
	}
	for _, status := range Statuses {
		links = append(links, contract.Link{Rel: []string{status}, Href: sf.Href("/gogios/" + status)})
	}

	props := map[string]any{"node": sf.Node}
	if state.GogiosErr != nil {
		props["error"] = state.GogiosErr.Error()
	} else {
		props["subject"] = state.Gogios.Subject
		props["lastUpdated"] = state.Gogios.LastUpdated
		props["summary"] = map[string]any{
			"critical":   state.Gogios.Summary.Critical,
			"warning":    state.Gogios.Summary.Warning,
			"unknown":    state.Gogios.Summary.Unknown,
			"stale":      state.Gogios.Summary.Stale,
			"suppressed": state.Gogios.Summary.Suppressed,
			"ok":         state.Gogios.Summary.Ok,
		}
	}

	return contract.Entity{
		Class:      []string{"gogios", "section"},
		Title:      "Gogios status and alerting",
		Properties: props,
		Links:      links,
		// The folder advertises the whole family's controls: the report cache
		// clear, and the gateway mute pair -- which is why the route's render
		// needs state.Monitoring too (see enrichState's IsFolderPath fetch).
		Actions: sf.actionsFor(state, "gogios-cache-clear", "monitoring-mute", "monitoring-unmute"),
	}, http.StatusOK, nil
}

// statusHandle returns the Handle for one Statuses drill-down route: the
// checks in that category, or the fetch error, following the same "error is a
// property, not a status code" convention as handleOverview.
//
// A closure bound to status, the same shape the power surface's action
// handles bind their action identifier -- routes generated in a loop
// (reportRoutes) need one Handle per iteration value, not one shared function
// that would have to re-derive status from the request path.
func (sf *Surface) statusHandle(status string) contract.Handle {
	return func(_ context.Context, state contract.State, _ contract.Request) (contract.Entity, int, error) {
		props := map[string]any{"status": status, "node": sf.Node}
		e := contract.Entity{
			Class:      []string{"gogios", "checks"},
			Title:      "Gogios " + strings.ToUpper(status) + " checks",
			Properties: props,
			Links: []contract.Link{
				{Rel: []string{"self"}, Href: sf.Href("/gogios/" + status)},
				{Rel: []string{"up"}, Href: sf.Href("/gogios")},
			},
		}

		if state.GogiosErr != nil {
			props["error"] = state.GogiosErr.Error()
			return e, http.StatusOK, nil
		}

		for _, c := range checksForStatus(state.Gogios, status) {
			e.Entities = append(e.Entities, sf.checkEntity(c))
		}
		return e, http.StatusOK, nil
	}
}

// checksForStatus selects the checks for one Statuses category.
//
// "critical"/"warning"/"unknown"/"ok" are severities: Report.ByStatus groups
// every check, from every lifecycle section, by its own Status field.
// "stale"/"suppressed" are lifecycle groupings instead -- a stale check keeps
// whatever severity it already had, so filtering it out of ByStatus's result
// would either double-count it under both a severity and a lifecycle
// category, or require ByStatus to invent a status value Gogios itself never
// writes. Reading Sections.Stale/Suppressed directly avoids both.
func checksForStatus(r *gogios.Report, status string) []gogios.Check {
	switch status {
	case "stale":
		return r.Sections.Stale
	case "suppressed":
		return r.Sections.Suppressed
	default:
		return r.ByStatus()[strings.ToUpper(status)]
	}
}

// checkEntity renders one check, embeddable in a drill-down collection
// or standalone from handleCheck.
func (sf *Surface) checkEntity(c gogios.Check) contract.Entity {
	props := map[string]any{
		"name":   c.Name,
		"status": c.Status,
		"output": c.Output,
		"epoch":  c.Epoch,
	}
	if c.PrevStatus != "" {
		props["prevStatus"] = c.PrevStatus
	}
	if c.FederatedFrom != "" {
		props["federatedFrom"] = c.FederatedFrom
	}
	if c.LastCheckedAgeSeconds != 0 {
		props["lastCheckedAgeSeconds"] = c.LastCheckedAgeSeconds
	}
	return contract.Entity{
		Class:      []string{"check"},
		Rel:        []string{"item"},
		Properties: props,
		Links: []contract.Link{
			{Rel: []string{"self"}, Href: sf.Href("/gogios/check") + "?name=" + url.QueryEscape(c.Name)},
		},
	}
}

// handleCheck renders one check's full detail, looked up by name from the
// ?name= query parameter -- a query param rather than a path segment because
// the composition root's Router matches exact paths only, and a check's name
// (it mirrors the monitored command, e.g. "Check Ping6 r1.wg0.wan.buetow.org")
// may itself contain spaces or slashes that a path segment cannot carry.
//
// Unlike handleOverview/statusHandle, a fetch failure here is a hard error
// (502): those two render a list that can legitimately be empty or degraded,
// but a single-entity lookup cannot answer "does this check exist" at all
// without the report, so there is nothing meaningful to return as a 200.
func (sf *Surface) handleCheck(_ context.Context, state contract.State, req contract.Request) (contract.Entity, int, error) {
	if state.GogiosErr != nil {
		return contract.Entity{}, http.StatusBadGateway, fmt.Errorf("fetching the Gogios report: %w", state.GogiosErr)
	}

	name := req.Query.Get("name")
	check, ok := state.Gogios.Check(name)
	if !ok {
		return contract.Entity{}, http.StatusNotFound, fmt.Errorf("no such Gogios check: %q", name)
	}

	e := sf.checkEntity(check)
	e.Rel = nil
	e.Title = "Gogios check: " + check.Name
	e.Links = append(e.Links, contract.Link{Rel: []string{"up"}, Href: sf.Href("/gogios")})
	return e, http.StatusOK, nil
}

// handleClearCache clears the on-disk Gogios cache and re-reads it, so
// the very next read anywhere in the API sees a fresh fetch rather than
// waiting out cfg.GogiosCacheTTL. Mirrors setMute's shape: mutate, then
// re-populate state and re-render the overview, rather than assuming success.
func (sf *Surface) handleClearCache(ctx context.Context, state contract.State, _ contract.Request) (contract.Entity, int, error) {
	if err := gogios.ClearCache(sf.Config); err != nil {
		return contract.Entity{}, http.StatusInternalServerError, err
	}

	state.Gogios, state.GogiosErr = gogios.Fetch(ctx, sf.Config)
	return sf.handleOverview(ctx, state, contract.Request{})
}
