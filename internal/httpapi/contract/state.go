package contract

import (
	"github.com/snonux/f3sctl/internal/coordination"
	"github.com/snonux/f3sctl/internal/gogios"
	"github.com/snonux/f3sctl/internal/power"
)

// State is a snapshot of the world, taken once per request before anything is
// rendered.
//
// Availability predicates read only from here, never from live probes of their
// own, so every action in a single response is judged against the same instant.
//
// It is pure data on purpose: the per-domain predicates that read it (power
// availability, the Gogios mute) live with the surface package that owns that
// domain, while this struct says only what a snapshot carries at all.
type State struct {
	Hosts   []power.HostStatus
	Fans    power.FansState
	FansErr error
	Job     *coordination.Job
	// Monitoring is the per-gateway Gogios mute state. Nil when it was not
	// collected for this request: reading it costs two SSH round trips to the
	// gateways, so only the routes that render it pay for it.
	Monitoring []power.GatewayMute

	// Gogios is the fetched-or-cached Gogios alert report (internal/gogios),
	// populated by the composition root only for /gogios* paths -- reading it
	// costs an HTTP round trip on a cold cache, so only those routes pay for
	// it. Nil when not collected for this request, or when the fetch failed;
	// the two are told apart by GogiosErr, the same pattern Fans/FansErr
	// uses.
	Gogios *gogios.Report
	// GogiosErr is set when the Gogios fetch failed; Gogios is nil in that case.
	GogiosErr error

	// PeerBusy reports whether the *other* API node is running a job.
	//
	// relayd load-balances pi0 and pi1, so a job started on one node is
	// invisible to the other's local state. Without this, the idle node cheerfully
	// advertises power-off while the busy node is mid-shutdown, and every one of
	// those actions 409s the instant a client tries it -- which is exactly the
	// "read the 409, not the response" behaviour this API exists to avoid.
	PeerBusy bool
}
