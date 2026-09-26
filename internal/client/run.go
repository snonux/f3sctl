package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/power"
	"github.com/snonux/f3sctl/internal/presenter"
)

// jobWaitBuffer is added on top of the server's worst-case UnmuteTimeout to
// get the client's poll deadline.
//
// UnmuteTimeout alone is not the job's full worst-case runtime: On() also
// switches the fans, sends the magic packets, and eachGateway does two SSH
// round trips to clear the mute after waitForCluster returns (see
// internal/power/monitor.go, internal/power/wake.go on()). That prelude and
// cleanup cost single-digit seconds normally, but a gateway SSH connection
// under load is not bounded by anything in this codebase, so the buffer is
// minutes rather than seconds -- generous on purpose, since the bug this
// guards against is the client giving up moments before the server actually
// finishes.
const jobWaitBuffer = 5 * time.Minute

// jobWaitTimeout returns the wake-only budget jobDeadline starts from: how
// long waitForJob polls before giving up unless the server advertised a
// longer staleness ceiling for the job.
//
// It must exceed the server's worst-case job runtime, or the client can (and
// on 2026-08-09 did) give up seconds before a job that was about to succeed.
// Deriving it from cfg.UnmuteTimeout rather than an independently chosen
// constant is the fix: the old code hardcoded 20 minutes to match
// UnmuteTimeout's default of 1200s, and the two only looked equal -- the
// server's actual worst case is UnmuteTimeout plus the prelude and gateway
// work jobWaitBuffer accounts for, so the client was always going to lose
// that race. Computing from the same config value means an operator who
// raises UnmuteTimeout (as happened 2026-08-09, 600s -> 1200s, to survive a
// slow ntpd_sync_on_start) automatically raises the client's patience too.
//
// A zero cfg.UnmuteTimeout means "unset" (a caller that built config.Config{}
// directly instead of going through config.Default()/config.Load()), not
// "wait zero seconds" -- there is no scenario where an operator wants
// waitForJob to give up after just jobWaitBuffer (~5m) while r0/r1/r2 are
// still booting. Substituting config.Default()'s UnmuteTimeout for that case
// mirrors how internal/agent/poweroff.go's vmShutdownTimeout already treats
// an unset/zero VMShutdownTimeout: fall back to the compiled-in default
// rather than let a forgotten config.Default() silently produce a deadline
// nobody chose (see uz0).
func (c *Client) jobWaitTimeout() time.Duration {
	unmute := c.cfg.UnmuteTimeout.D()
	if unmute <= 0 {
		unmute = config.Default().UnmuteTimeout.D()
	}
	return unmute + c.poll.withDefaults().waitBuffer
}

// Run executes a CLI command against the remote API.
//
// ctx bounds every round trip and the job poll: a `--remote` run interrupted
// with Ctrl-C (runRemote wires signal.NotifyContext) stops mid-poll rather
// than looping for up to jobWaitTimeout (~25m) with no way out, and a
// cancelled HTTP call stops the in-flight request rather than running the
// client's own 60s timeout out first.
func Run(ctx context.Context, c *Client, args []string, force bool) error {
	cmd := strings.Join(args, " ")

	if cmd == "power status" || cmd == "fans status" || cmd == "ac status" {
		return c.showStatus(ctx)
	}
	if cmd == "monitoring status" {
		return c.showMonitoring(ctx)
	}
	if len(args) > 0 && args[0] == "gogios" {
		return c.runGogios(ctx, args[1:], force)
	}

	// args[0] is the CLI noun ("power", "fans", "monitoring"). Where the root
	// has a link with that rel, the matching resource is where the action is
	// advertised -- see runAction.
	return c.runAction(ctx, cmd, args[0], force)
}

// runAction performs the action whose declared CLI verb is cmd, looking for
// it on the resource named by holderRel and falling back to the root.
//
// cmd is matched against Action.CLIVerb rather than resolved to an action
// name first: the server is the single source for that mapping (it is
// declared once, on the route -- see contract.Route's
// route.CLIVerb), and this client already fetches the actions it advertises,
// so reading the mapping off them replaces a local actionFor table that had
// to be updated by hand every time a route's CLI spelling changed -- and
// could (and did) drift from the server's if it wasn't. See sy0.
//
// Not every noun's actions live on the root, and since the section folders
// (see docs/CLIENT.md §3) not even the power actions live there: the root is
// a folder index, and holderRel is walked through the folders first --
// "power" resolves to its own /power folder, "ac" and "fans" through the
// AC control folder (both Shelly plugs), "monitoring" through the Gogios
// folder, "gogios" to the resource the root's own rel offers. Reading the
// link chain from the documents rather than building a path keeps this
// discovery-driven: no action name or path is hard-coded, and a noun whose
// rel the root does not offer simply falls back to the root -- behaviour a
// server predating the folders (or a fixture without them) still navigates.
func (c *Client) runAction(ctx context.Context, cmd, holderRel string, force bool) error {
	root, err := c.Root(ctx)
	if err != nil {
		return err
	}

	holder, err := c.resolveHolder(ctx, root, holderRel)
	if err != nil {
		return err
	}

	action, ok := holder.ActionForVerb(cmd)
	if !ok {
		// Either cmd names nothing the server has ever heard of, or it names
		// something currently withheld -- the two look identical from here,
		// since only possible actions are advertised (the server renders
		// them from its route table, filtered by each route's Available
		// predicate -- see powerapi.Surface.Actions). Either way, showing
		// the state it was judged against is more useful than a bare error.
		fmt.Fprintf(c.stdout, "%q is not available right now.\n\n", cmd)
		switch holderRel {
		case "monitoring":
			return c.showMonitoring(ctx)
		case "gogios":
			return c.showGogios(ctx)
		}
		return c.showStatus(ctx)
	}

	result, err := c.Perform(ctx, action, force)
	if err != nil {
		return err
	}

	// A synchronous action (the fan plug) comes back with its new state; an
	// asynchronous one comes back as a running job to follow.
	if state, _ := result.Properties["state"].(string); state == "running" {
		id, _ := result.Properties["id"].(string)
		fmt.Fprintf(c.stdout, "%s accepted; waiting for it to finish...\n", action.Name)
		return c.waitForJob(ctx, root, id, serverStaleCeiling(result))
	}

	if action.Name == "gogios-cache-clear" {
		// The action's own response already carries the fresh overview
		// (the Gogios surface's handleClearCache re-renders it server-side), but showGogios
		// re-fetches rather than rendering result directly, the same
		// "mutate, then re-follow" shape the monitoring-* branch below uses
		// for consistency across every action in this function.
		return c.showGogios(ctx)
	}
	if strings.HasPrefix(action.Name, "monitoring-") {
		return c.showMonitoring(ctx)
	}

	fmt.Fprintf(c.stdout, "%s: done\n", action.Name)
	return c.showStatus(ctx)
}

// nounHolderPath is the rel chain from the root to the entity whose actions
// serve each CLI noun, through the section folders (see docs/CLIENT.md §3 --
// the root is a folder index and a noun's controls live in its folder). It
// carries no paths and no action names, only the order of links to follow;
// every hop is read from the document the previous hop handed back, so a
// server that rearranges its folders needs no change here, and a document
// that does not offer a link (the pre-folder shape, or a test fixture)
// degrades gracefully: whatever the last successful hop reached is the
// holder, the root if none followed.
var nounHolderPath = map[string][]string{
	"power":      {"power"},
	"fans":       {"ac-control", "fans"},
	"ac":         {"ac-control", "ac"},
	"monitoring": {"gogios", "monitoring"},
	"gogios":     {"gogios"},
}

// resolveHolder walks a noun's rel chain from the root and returns the
// entity to look the noun's actions up on. Silence about a broken chain is
// deliberate -- the caller's ActionForVerb miss renders the state it was
// judged against -- the same tolerated-degradation the single-hop version
// this replaced had. See runAction.
func (c *Client) resolveHolder(ctx context.Context, root Entity, noun string) (Entity, error) {
	entity := root
	for _, rel := range nounHolderPath[noun] {
		if _, ok := entity.Link(rel); !ok {
			break
		}
		e, err := c.Follow(ctx, entity, rel)
		if err != nil {
			break
		}
		entity = e
	}
	return entity, nil
}

// showMonitoring renders the Gogios mute for each gateway.
//
// Resolved through the section folders (the gogios folder links /monitoring;
// the root links the folder) rather than fetched from a known path: the
// state is only read when asked for, because it costs the server an SSH
// round trip to each gateway.
func (c *Client) showMonitoring(ctx context.Context) error {
	root, err := c.Root(ctx)
	if err != nil {
		return err
	}
	mon, err := c.resolveHolder(ctx, root, "monitoring")
	if err != nil {
		return err
	}

	for _, gw := range mon.Entities {
		name, _ := gw.Properties["name"].(string)
		if msg, _ := gw.Properties["error"].(string); msg != "" {
			fmt.Fprintf(c.stdout, "%s: unknown (%s)\n", name, msg)
			continue
		}
		if muted, _ := gw.Properties["muted"].(bool); muted {
			fmt.Fprintf(c.stdout, "%s: MUTED\n", name)
		} else {
			fmt.Fprintf(c.stdout, "%s: alerting\n", name)
		}
	}

	if muted, _ := mon.Properties["muted"].(bool); muted {
		fmt.Fprintln(c.stdout, "\nGogios alerting is suppressed. "+
			"Clear it with: f3sctl monitoring unmute")
	}
	c.printAvailable(mon.Actions)
	return nil
}

// printAvailable lists the actions an entity advertises as possible right
// now, or prints nothing when it advertises none. Showing what can be done
// next is the point of a hypermedia client: this list is the server's, not a
// guess -- only possible actions are ever advertised (the server renders them
// from its route table, filtered by each route's Available predicate -- see
// powerapi.Surface.Actions and gogiosapi.Surface.ActionsFor).
//
// Each action is shown by its CLIVerb, the command the operator actually
// types ("power on"), falling back to its name for a server that predates
// the field (see ActionForVerb's legacy path).
func (c *Client) printAvailable(actions []Action) {
	if len(actions) == 0 {
		return
	}
	verbs := make([]string, 0, len(actions))
	for _, a := range actions {
		if a.CLIVerb != "" {
			verbs = append(verbs, a.CLIVerb)
		} else {
			verbs = append(verbs, a.Name)
		}
	}
	fmt.Fprintf(c.stdout, "\navailable now: %s\n", strings.Join(verbs, ", "))
}

// waitForJob polls the job resource until the job with the given id stops
// running.
//
// Matching on the id is essential, not defensive. relayd load-balances pi0 and
// pi1, so a poll routinely lands on the node that did not run this job -- and
// that node holds a *different* job, quite possibly an old failed one. Reading
// its state as ours reports a perfectly healthy shutdown as a failure, which is
// exactly what happened on 2026-08-08 before ids existed. Anything that is not
// our id is "no news", not news.
//
// The poll deadline comes from jobWaitTimeout, not a constant here, so it
// tracks the server's actual worst-case runtime -- see jobWaitTimeout's
// comment for why an independent constant caused a "gave up" report on
// 2026-08-09 for a job that succeeded moments later. serverCeiling, the
// accepting node's own staleness ceiling (see serverStaleCeiling), raises that
// deadline when it is the longer of the two: a power cycle runs a shutdown and
// a wake in one job, which outlasts the wake-only budget jobWaitTimeout
// derives from this side's UnmuteTimeout.
func (c *Client) waitForJob(ctx context.Context, root Entity, id string, serverCeiling time.Duration) error {
	poll := c.poll.withDefaults()
	timeout := c.jobDeadline(serverCeiling)
	// Bound the wait by BOTH the caller's ctx and the server's worst-case
	// runtime: a Ctrl-C (runRemote wires signal.NotifyContext) cancels ctx, and
	// a caller that handed over an unbounded context still gives up after
	// jobDeadline(serverCeiling) rather than looping forever. Whichever fires first wins.
	// The cause tells the two apart below: a deadline the caller's own ctx
	// carried is the caller's, not this function's "gave up".
	ctx, cancel := context.WithTimeoutCause(ctx, timeout, errJobWaitTimeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			if errors.Is(context.Cause(ctx), errJobWaitTimeout) {
				return fmt.Errorf("%w after %s", errJobWaitTimeout, timeout)
			}
			// The caller cancelled (Ctrl-C, or the request that drove this went
			// home). Surface that rather than reporting a synthetic "gave up".
			return ctx.Err()
		case <-time.After(poll.interval):
		}

		if c.reportPoll(ctx, root, id) {
			return c.showStatus(ctx)
		}
	}
}

// jobDeadline returns how long waitForJob polls: jobWaitTimeout, raised to
// the accepting node's staleness ceiling plus the wait buffer when that is
// longer (a power cycle outlasts the wake-only budget -- see waitForJob).
func (c *Client) jobDeadline(serverCeiling time.Duration) time.Duration {
	timeout := c.jobWaitTimeout()
	if extended := serverCeiling + c.poll.withDefaults().waitBuffer; extended > timeout {
		return extended
	}
	return timeout
}

// errJobWaitTimeout is what waitForJob returns (wrapped, with the deadline it
// used) when the job outlived the client's patience, as opposed to the caller
// cancelling the wait.
var errJobWaitTimeout = errors.New("gave up waiting for the job")

// reportPoll runs one polling cycle for the job with the given id and prints
// what it learned. It reports true once the job has stopped running.
func (c *Client) reportPoll(ctx context.Context, root Entity, id string) bool {
	job, err := c.pollJob(ctx, root, id)
	if err != nil && ctx.Err() != nil {
		// Cancelled or out of time mid-poll: the read failed because of that,
		// not the network, and waitForJob's next select reports why.
		return false
	}
	if err != nil {
		// A transient network blip mid-shutdown is expected -- the
		// cluster is, after all, being taken apart.
		fmt.Fprintf(c.stdout, "  (cannot read the job right now: %v)\n", err)
		return false
	}
	if job == nil {
		// Every attempt this cycle reached the other node. Say so plainly
		// rather than reporting someone else's outcome as this one's.
		fmt.Fprintln(c.stdout, "  (polled the other API node; still waiting)")
		return false
	}

	state, _ := job.Properties["state"].(string)
	if state == "running" {
		// Show the stage rather than a bare "still running": a shutdown
		// takes minutes, and knowing which host it is on is the
		// difference between waiting patiently and wondering if it hung.
		if step, _ := job.Properties["step"].(string); step != "" {
			fmt.Fprintf(c.stdout, "  %s\n", step)
		} else {
			fmt.Fprintln(c.stdout, "  still running...")
		}
		return false
	}
	if msg, _ := job.Properties["error"].(string); msg != "" {
		fmt.Fprintf(c.stdout, "job %s: %s\n", state, msg)
	} else {
		fmt.Fprintf(c.stdout, "job %s\n", state)
	}
	return true
}

// serverStaleCeiling reads the staleness ceiling a job entity advertises
// ("staleAfterSeconds", see docs/CLIENT.md), or zero when it carries none --
// an older server, or a fixture without it. The property arrives as a JSON
// number, i.e. float64 once decoded.
func serverStaleCeiling(job Entity) time.Duration {
	secs, _ := job.Properties["staleAfterSeconds"].(float64)
	return time.Duration(secs) * time.Second
}

// jobPollInterval is the gap between polling cycles, and jobPollRetries is how
// many reads one cycle may take to reach the node that actually holds the job.
const (
	jobPollInterval = 10 * time.Second
	jobPollRetries  = 3
	jobRetryGap     = time.Second
)

// jobPolling holds the timings waitForJob and pollJob wait on. It exists as a
// seam: production always runs with the defaults, while tests shrink them so
// the retry, id-matching and deadline paths can run against a real HTTP
// server in milliseconds.
type jobPolling struct {
	interval   time.Duration // gap between polling cycles
	retryGap   time.Duration // gap between reads within one cycle
	waitBuffer time.Duration // added to the server's worst case for the deadline
}

// withDefaults fills every unset (zero or negative) timing with its
// production value, so a Client built without New -- or a test that only
// cares about one gap -- still polls sensibly.
func (p jobPolling) withDefaults() jobPolling {
	if p.interval <= 0 {
		p.interval = jobPollInterval
	}
	if p.retryGap <= 0 {
		p.retryGap = jobRetryGap
	}
	if p.waitBuffer <= 0 {
		p.waitBuffer = jobWaitBuffer
	}
	return p
}

// pollJob reads this job, retrying briefly when the read lands on the other
// API node. It returns nil, nil when every attempt did.
//
// relayd spreads requests across pi0 and pi1, so roughly half of a single-shot
// poll's reads reach the node that is not running this job. That is not an
// error -- waitForJob has always treated another node's job as "no news" -- but
// paying a full ten-second cycle for it means the operator sees a stage change
// every twenty seconds on average, and during a parallel shutdown the visible
// step then lags what the rack is actually doing. Retrying two or three times
// a second apart costs a couple of cheap reads and makes it very likely that
// each cycle carries real news.
//
// The retries are deliberately bounded and slow enough to stay polite: this is
// a CGI on a Raspberry Pi, and the job it is reporting on takes minutes.
func (c *Client) pollJob(ctx context.Context, root Entity, id string) (*Entity, error) {
	retryGap := c.poll.withDefaults().retryGap
	var lastErr error

	for attempt := 0; attempt < jobPollRetries; attempt++ {
		if attempt > 0 {
			// Stop retrying the moment the caller is cancelled, rather than
			// paying jobPollRetries more reads into a poll nobody is waiting
			// on.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryGap):
			}
		}

		job, err := c.Follow(ctx, root, "job")
		if err != nil {
			lastErr = err
			continue
		}
		if got, _ := job.Properties["id"].(string); got == id {
			return &job, nil
		}
	}

	// Nothing but the other node's job (or nothing but errors) this cycle.
	return nil, lastErr
}

// showStatus renders the remote status in the same shape as the local CLI, so
// --remote is a routing detail rather than a different tool.
//
// The table itself is built by internal/presenter, shared with the local CLI
// (internal/cli.printStatus) so the two cannot drift the way they had before
// this was unified -- see ry0. ShowRole is false: the host entity does carry
// the role (the second entry of its "class"), but the remote client leaves the
// ROLE column off by choice (see presenter.Options.ShowRole).
func (c *Client) showStatus(ctx context.Context) error {
	root, err := c.Root(ctx)
	if err != nil {
		return err
	}
	statusEntity, err := c.Follow(ctx, root, "status")
	if err != nil {
		return err
	}

	statuses, fans, fansErr, ac, acErr := parseStatus(statusEntity)
	if err := presenter.Status(c.stdout, statuses, presenter.Options{ShowRole: false}, fans, fansErr, ac, acErr); err != nil {
		return err
	}

	// The actions come off the status entity, not the root: since the
	// section folders the root is a folder index that renders no actions of
	// its own (see httpapi's handleRoot), while /status renders every action
	// possible right now -- so reading the root here printed nothing at all.
	c.printAvailable(statusEntity.Actions)
	return nil
}

// parseStatus turns a /status entity into the presenter's inputs: one
// power.HostStatus per host entity (in response order), plus the fan and AC
// plug states.
//
// This is where hz0 was fixed: the old code built its own table row here and
// read only ping/ssh, never pingKnown, so an unmeasured host rendered as
// "off". Routing through power.HostStatus -- which has always carried
// PingKnown -- and presenter.Describe -- which has always read it -- means
// the remote client gets that check by construction rather than by
// remembering to add it a second time.
func parseStatus(status Entity) (statuses []power.HostStatus, fans power.FansState, fansErr error, ac power.ACState, acErr error) {
	for _, e := range status.Entities {
		if hasClass(e, "fans") {
			fans, fansErr = parseFans(e)
			continue
		}
		if hasClass(e, "ac") {
			ac, acErr = parseAC(e)
			continue
		}
		if st, ok := parseHost(e); ok {
			statuses = append(statuses, st)
		}
	}
	return statuses, fans, fansErr, ac, acErr
}

// parseHost turns one host entity's properties into a power.HostStatus. ok is
// false for an entity with no name, which is not a host this response meant
// to describe.
//
// pingKnown defaults to true when the property is absent, matching
// docs/client-reference.js's describe(): an older server that predates the
// field is assumed to have completed the probe, not to have skipped it.
func parseHost(e Entity) (power.HostStatus, bool) {
	name, _ := e.Properties["name"].(string)
	if name == "" {
		return power.HostStatus{}, false
	}

	pingKnown := true
	if v, ok := e.Properties["pingKnown"].(bool); ok {
		pingKnown = v
	}

	ip, _ := e.Properties["ip"].(string)
	ping, _ := e.Properties["ping"].(bool)
	ssh, _ := e.Properties["ssh"].(bool)
	ms, _ := e.Properties["ms"].(float64)

	return power.HostStatus{
		Name:      name,
		IP:        ip,
		Ping:      ping,
		PingKnown: pingKnown,
		SSH:       ssh,
		MS:        ms,
	}, true
}

// parseFans turns a "fans" entity into a power.FansState, or an error when
// the server reported the plug as unreachable rather than a state -- see
// powerapi's fansEntity and presenter.Status.
func parseFans(e Entity) (power.FansState, error) {
	if msg, _ := e.Properties["error"].(string); msg != "" {
		// Unreachable is not the same as off, and saying "off" here would
		// send someone to the garage for nothing. presenter.Status renders
		// this error as "unknown (<msg>)", never as a state.
		return power.FansState{}, errors.New(msg)
	}
	on, _ := e.Properties["on"].(bool)
	ip, _ := e.Properties["ip"].(string)
	return power.FansState{On: on, IP: ip}, nil
}

// parseAC turns an "ac" entity into a power.ACState, same unknown-not-off
// rule as parseFans.
func parseAC(e Entity) (power.ACState, error) {
	if msg, _ := e.Properties["error"].(string); msg != "" {
		return power.ACState{}, errors.New(msg)
	}
	on, _ := e.Properties["on"].(bool)
	ip, _ := e.Properties["ip"].(string)
	return power.ACState{On: on, IP: ip}, nil
}

func hasClass(e Entity, want string) bool {
	for _, c := range e.Class {
		if c == want {
			return true
		}
	}
	return false
}
