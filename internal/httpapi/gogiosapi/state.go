package gogiosapi

import (
	"context"

	"github.com/snonux/f3sctl/internal/gogios"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
)

// The request-scoped state this surface owns: the Gogios mute and the alert
// report. Both are declared here rather than in contract -- the contract
// knows neither gogios type -- and filled by this Surface's own Providers,
// so the composition root's enrichState runs them without knowing what they
// read or where from.
var (
	// NeedMonitoring is the Gogios mute marker on each gateway (Monitoring)
	// -- an SSH round trip to each. The mute pair is judged on it.
	NeedMonitoring = contract.NewNeed("gogios-monitoring")
	// NeedReport is the Gogios alert report (Report) -- a stat of the
	// on-disk cache, and on a cold or expired cache an HTTP fetch.
	NeedReport = contract.NewNeed("gogios-report")
)

// monitoringSlot and reportSlot are where this surface keeps its state in a
// contract.State. Unexported: everything else goes through the accessors
// below.
var (
	monitoringSlot = contract.NewSlot[[]gogios.GatewayMute]("gogios monitoring")
	reportSlot     = contract.NewSlot[reportState]("gogios report")
)

// reportState is one report fetch's outcome: the report, or why there is
// none.
type reportState struct {
	report *gogios.Report
	err    error
}

// Monitoring returns the per-gateway mute state in s. Nil when it was not
// collected for this request: reading it costs two SSH round trips to the
// gateways, so only the routes declaring NeedMonitoring pay for it.
func Monitoring(s contract.State) []gogios.GatewayMute { return monitoringSlot.Get(s) }

// WithMonitoring returns s carrying gws as its per-gateway mute state.
func WithMonitoring(s contract.State, gws []gogios.GatewayMute) contract.State {
	return monitoringSlot.With(s, gws)
}

// Report returns the fetched-or-cached alert report in s, and the fetch
// error when there is none. Both are nil when the report was not collected
// for this request (the route did not declare NeedReport) -- the same
// pattern as contract.State's Fans/FansErr.
func Report(s contract.State) (*gogios.Report, error) {
	rs := reportSlot.Get(s)
	return rs.report, rs.err
}

// WithReport returns s carrying the outcome of a report fetch.
func WithReport(s contract.State, r *gogios.Report, err error) contract.State {
	return reportSlot.With(s, reportState{report: r, err: err})
}

// Providers returns the Fetches that fill this surface's Needs, bound to its
// own collaborators: the mute read through Monitor, the report through the
// ReportSource New was given -- the very source the cache clear clears, so a
// request never reads the report from one source and clears another.
func (sf *Surface) Providers() []contract.Provider {
	return []contract.Provider{
		{Need: NeedMonitoring, Fetch: sf.fetchMonitoring},
		{Need: NeedReport, Fetch: sf.fetchReport},
	}
}

func (sf *Surface) fetchMonitoring(ctx context.Context, s contract.State, _ contract.Request) contract.State {
	return WithMonitoring(s, sf.Monitor.MonitoringStatus(ctx))
}

func (sf *Surface) fetchReport(ctx context.Context, s contract.State, _ contract.Request) contract.State {
	r, err := sf.reports.Fetch(ctx)
	return WithReport(s, r, err)
}
