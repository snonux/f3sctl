// Package gogiosapi is the REST surface for everything Gogios: the mute
// concern (/monitoring) and the alert-report browse concern (/gogios*).
//
// It is one of internal/httpapi's two domain surfaces -- the other is powerapi
// -- and holds exactly the routes and handlers whose subject is Gogios: the
// marker-file mute on the two gateways, and the read-only browse of the alert
// report itself. Everything that is neither (the Siren vocabulary, the route
// plumbing, the composition root) stays in the parent, which imports this
// package rather than the other way round; the shared vocabulary both sides
// speak is contract.
//
// The two concerns housed here are kept apart inside the package the same way
// the API presents them, because they are different operations on different
// machines: mute/unmute is a WRITE, two SSH round trips through the power
// engine's Monitor; browsing the report is a READ of internal/gogios's
// cached-or-fetched report, never touching the engine. The overview handler
// cross-links to /monitoring so a client can reach the mute controls from the
// report, but nothing in the code conflates them.
package gogiosapi

import (
	"context"
	"io"
	"strings"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
	"github.com/snonux/f3sctl/internal/power"
)

// Path prefixes -- the PATH_INFO prefixes this surface answers on, named
// as constants so the composition root's enrichState (which pre-fetches state
// for exactly these families) and the routes themselves cannot drift apart.
const (
	// monitorPathPrefix covers /monitoring, /monitoring/mute and
	// /monitoring/unmute.
	monitorPathPrefix = "/monitoring"
	// reportPathPrefix covers the whole alert-browse family: /gogios, every
	// drill-down, /gogios/check and /gogios/cache/clear.
	reportPathPrefix = "/gogios"
	// cacheClearPath is the cache-clear action, which answers with the
	// re-rendered folder (see handleClearCache) and so counts as a folder
	// path for IsFolderPath. It must match the gogios-cache-clear route's
	// Path (routes.go); TestIsFolderPathCoversEveryFolderRender pins that.
	cacheClearPath = reportPathPrefix + "/cache/clear"
)

// Surface is the Gogios REST surface, bound to the collaborators its handlers
// need.
//
// Everything here is injected by the composition root (internal/httpapi) when
// it assembles a Server: the node name and href builder identify this
// deployment, Config reaches the report's URL/TTL/cache knobs, and Monitor is
// the one slice of the power engine the mute drives. Nil Href/collaborators
// are safe at table-declaration time -- the closures dereference them only
// while serving the route that needs them.
type Surface struct {
	// Node is this node's hostname, reported on every entity ("node"
	// property) so a client can tell which of pi0/pi1 answered.
	Node string
	// Href builds the absolute href for a route path, under the CGI mount
	// this node answers on. See contract.Href.
	Href func(string) string
	// Config carries the Gogios report's fetch/cache configuration (the same
	// cfg.Config the report itself is read with).
	Config config.Config
	// Monitor changes and reads the Gogios mute marker on the gateways. In
	// production this is the power engine's Monitor; a subset interface of
	// it, because mute/unmute are the only engine powers this surface needs.
	Monitor Monitor
	// actions renders the actions list a resource advertises for the named
	// routes, judged against the current state. It is unexported and set only
	// by New, which refuses a nil one, so a Surface built outside this
	// package always renders its actions through the composition root's
	// Router -- the single source of the Siren action shape (name, title,
	// method, href, cliVerb, fields) -- and there is no path on which a served
	// resource silently advertises nothing.
	actions contract.ActionRenderer
}

// Monitor is the slice of the power engine the mute drives. Satisfied by
// *power.Engine in production and by fakes in tests.
type Monitor interface {
	// MuteGogios sets the mute marker on both gateways.
	MuteGogios(ctx context.Context, log io.Writer) error
	// UnmuteNow clears it immediately, without waiting for the next power-on.
	UnmuteNow(ctx context.Context, log io.Writer) error
	// MonitoringStatus reads the marker from each gateway.
	MonitoringStatus(ctx context.Context) []power.GatewayMute
}

// New returns a Surface bound to its collaborators, rendering every actions
// list through actions.
//
// It panics on a nil actions: unlike Monitor, which a test serving only the
// report routes may leave nil, every resource with controls renders through
// it, and a Surface without one is a wiring bug in the caller, not a state to
// serve in. In production actions resolves the composition root's Router
// lazily, since the Router is built from the very route table this Surface
// declares.
func New(node string, href func(string) string, cfg config.Config, monitor Monitor, actions contract.ActionRenderer) *Surface {
	if actions == nil {
		panic("gogiosapi: New called with a nil ActionRenderer")
	}
	return &Surface{Node: node, Href: href, Config: cfg, Monitor: monitor, actions: actions}
}

// Muted reports whether Gogios is muted on at least one gateway.
//
// A method on contract.State cannot exist for this -- State is shared
// vocabulary, and only this surface knows what "muted" means (power.AnyMuted
// over the gateways) -- so it is a plain function here.
func Muted(s contract.State) bool { return power.AnyMuted(s.Monitoring) }

// NotAllMuted reports whether some gateway is not known to be muted -- i.e.
// whether a mute might still change anything. False when no gateway was read
// at all (nil Monitoring: the route skipped the lookup) or every gateway
// answered "muted".
//
// It is deliberately not !Muted: after a partial mute (one gateway muted, the
// other alerting -- the mute runs on every gateway and keeps going past a
// failure) both are true, and both actions are then worth offering. Keying
// the mute on "nothing muted" would withhold the one call that finishes the
// job.
//
// An unreadable gateway is not known to be muted, so it earns the mute here
// just as it does not earn the un-mute in Muted: un-mute is offered only for
// a mute actually seen, mute unless silence is actually seen on every
// gateway. Neither can leave a client believing it is monitored when it is
// not. Muting is idempotent, so offering it on an unknown state costs at most
// a no-op -- and {muted, unreachable} is exactly what a partial mute usually
// leaves behind.
func NotAllMuted(s contract.State) bool {
	for _, gw := range s.Monitoring {
		if gw.Err != nil || !gw.Muted {
			return true
		}
	}
	return false
}

// IsMonitorPath reports whether path belongs to the mute family (/monitoring
// and its actions). The composition root uses it to decide whether to pay for
// the SSH round trips that read the gateways -- see enrichState.
func IsMonitorPath(path string) bool { return strings.HasPrefix(path, monitorPathPrefix) }

// IsFolderPath reports whether path answers with the Gogios folder: /gogios
// itself, and the cache-clear action, whose response is the re-rendered
// folder (see handleClearCache) -- as opposed to the drill-downs and the check
// detail beneath it, which render no mute state. The folder advertises the
// mute pair alongside the report (see handleOverview), so enrichState must
// also fetch the gateway mute for every such path -- the same SSH round trips
// IsMonitorPath exists to gate. Missing the cache-clear path here is what
// once made its response silently drop monitoring-mute/unmute.
func IsFolderPath(path string) bool { return path == reportPathPrefix || path == cacheClearPath }

// IsReportPath reports whether path belongs to the alert-browse family
// (/gogios and its drill-downs). The composition root uses it to decide
// whether to fetch the report -- see enrichState.
func IsReportPath(path string) bool { return strings.HasPrefix(path, reportPathPrefix) }
