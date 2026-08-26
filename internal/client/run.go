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
// internal/power/monitor.go, internal/power/engine.go on()). That prelude and
// cleanup cost single-digit seconds normally, but a gateway SSH connection
// under load is not bounded by anything in this codebase, so the buffer is
// minutes rather than seconds -- generous on purpose, since the bug this
// guards against is the client giving up moments before the server actually
// finishes.
const jobWaitBuffer = 5 * time.Minute

// jobWaitTimeout returns how long waitForJob polls before giving up.
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
	return unmute + jobWaitBuffer
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

	if cmd == "power status" || cmd == "fans status" {
		return c.showStatus(ctx)
	}
	if cmd == "monitoring status" {
		return c.showMonitoring(ctx)
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
// declared once, on the route -- see internal/httpapi/registry.go's
// route.CLIVerb), and this client already fetches the actions it advertises,
// so reading the mapping off them replaces a local actionFor table that had
// to be updated by hand every time a route's CLI spelling changed -- and
// could (and did) drift from the server's if it wasn't. See sy0.
//
// Not every action is advertised on the root. Reading the Gogios mute costs the
// server an SSH round trip to each gateway, so the root carries only a
// "monitoring" link and the mute/unmute pair lives on that resource. Looking
// only at the root made `f3sctl monitoring mute` report "not available right
// now" while `f3sctl monitoring status` was simultaneously advertising it --
// the client contradicting itself about the same server state.
//
// The rel is derived from the CLI noun and checked against the root's links, so
// this stays discovery-driven: no action name or path is hard-coded, and a
// noun with no matching link (like "power") simply falls back to the root.
func (c *Client) runAction(ctx context.Context, cmd, holderRel string, force bool) error {
	root, err := c.Root(ctx)
	if err != nil {
		return err
	}

	holder := root
	if _, ok := root.Link(holderRel); ok {
		if e, err := c.Follow(ctx, root, holderRel); err == nil {
			holder = e
		}
	}

	action, ok := holder.ActionForVerb(cmd)
	if !ok {
		// Either cmd names nothing the server has ever heard of, or it names
		// something currently withheld -- the two look identical from here,
		// since only possible actions are advertised (see
		// httpapi.Router.Actions). Either way, showing the state it was
		// judged against is more useful than a bare error.
		fmt.Fprintf(c.stdout, "%q is not available right now.\n\n", cmd)
		if holderRel == "monitoring" {
			return c.showMonitoring(ctx)
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
		return c.waitForJob(ctx, root, id)
	}

	if strings.HasPrefix(action.Name, "monitoring-") {
		return c.showMonitoring(ctx)
	}

	fmt.Fprintf(c.stdout, "%s: done\n", action.Name)
	return c.showStatus(ctx)
}

// showMonitoring renders the Gogios mute for each gateway.
//
// Followed from the root's "monitoring" link rather than fetched from a known
// path: the state is only read when asked for, because it costs the server an
// SSH round trip to each gateway.
func (c *Client) showMonitoring(ctx context.Context) error {
	root, err := c.Root(ctx)
	if err != nil {
		return err
	}
	mon, err := c.Follow(ctx, root, "monitoring")
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
	if len(mon.Actions) > 0 {
		var names []string
		for _, a := range mon.Actions {
			names = append(names, a.Name)
		}
		fmt.Fprintf(c.stdout, "\navailable now: %s\n", strings.Join(names, ", "))
	}
	return nil
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
// 2026-08-09 for a job that succeeded moments later.
func (c *Client) waitForJob(ctx context.Context, root Entity, id string) error {
	timeout := c.jobWaitTimeout()
	// Bound the wait by BOTH the caller's ctx and the server's worst-case
	// runtime: a Ctrl-C (runRemote wires signal.NotifyContext) cancels ctx, and
	// a caller that handed over an unbounded context still gives up after
	// jobWaitTimeout rather than looping forever. Whichever fires first wins.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("gave up waiting for the job after %s", timeout)
			}
			// The caller cancelled (Ctrl-C, or the request that drove this went
			// home). Surface that rather than reporting a synthetic "gave up".
			return ctx.Err()
		case <-time.After(jobPollInterval):
		}

		job, err := c.pollJob(ctx, root, id)
		if err != nil {
			// A transient network blip mid-shutdown is expected -- the
			// cluster is, after all, being taken apart.
			fmt.Fprintf(c.stdout, "  (cannot read the job right now: %v)\n", err)
			continue
		}
		if job == nil {
			// Every attempt this cycle reached the other node. Say so plainly
			// rather than reporting someone else's outcome as this one's.
			fmt.Fprintln(c.stdout, "  (polled the other API node; still waiting)")
			continue
		}

		switch state, _ := job.Properties["state"].(string); state {
		case "running":
			// Show the stage rather than a bare "still running": a shutdown
			// takes minutes, and knowing which host it is on is the
			// difference between waiting patiently and wondering if it hung.
			if step, _ := job.Properties["step"].(string); step != "" {
				fmt.Fprintf(c.stdout, "  %s\n", step)
			} else {
				fmt.Fprintln(c.stdout, "  still running...")
			}
		default:
			if msg, _ := job.Properties["error"].(string); msg != "" {
				fmt.Fprintf(c.stdout, "job %s: %s\n", state, msg)
			} else {
				fmt.Fprintf(c.stdout, "job %s\n", state)
			}
			return c.showStatus(ctx)
		}
	}
}

// jobPollInterval is the gap between polling cycles, and jobPollRetries is how
// many reads one cycle may take to reach the node that actually holds the job.
const (
	jobPollInterval = 10 * time.Second
	jobPollRetries  = 3
	jobRetryGap     = time.Second
)

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
	var lastErr error

	for attempt := 0; attempt < jobPollRetries; attempt++ {
		if attempt > 0 {
			// Stop retrying the moment the caller is cancelled, rather than
			// paying jobPollRetries more reads into a poll nobody is waiting
			// on.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(jobRetryGap):
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
// this was unified -- see ry0. ShowRole is deliberately false: see
// presenter.Options.ShowRole for why the remote client does not reach into a
// host entity's "class" array for the role hiding there.
func (c *Client) showStatus(ctx context.Context) error {
	root, err := c.Root(ctx)
	if err != nil {
		return err
	}
	statusEntity, err := c.Follow(ctx, root, "status")
	if err != nil {
		return err
	}

	statuses, fans, fansErr := parseStatus(statusEntity)
	if err := presenter.Status(c.stdout, statuses, presenter.Options{ShowRole: false}, fans, fansErr); err != nil {
		return err
	}

	// Showing what can be done next is the point of a hypermedia client: this
	// list is the server's, not a guess.
	if len(root.Actions) > 0 {
		var names []string
		for _, a := range root.Actions {
			names = append(names, a.Name)
		}
		fmt.Fprintf(c.stdout, "\navailable now: %s\n", strings.Join(names, ", "))
	}
	return nil
}

// parseStatus turns a /status entity into the presenter's inputs: one
// power.HostStatus per host entity (in response order), plus the fan state.
//
// This is where hz0 was fixed: the old code built its own table row here and
// read only ping/ssh, never pingKnown, so an unmeasured host rendered as
// "off". Routing through power.HostStatus -- which has always carried
// PingKnown -- and presenter.Describe -- which has always read it -- means
// the remote client gets that check by construction rather than by
// remembering to add it a second time.
func parseStatus(status Entity) (statuses []power.HostStatus, fans power.FansState, fansErr error) {
	for _, e := range status.Entities {
		if hasClass(e, "fans") {
			fans, fansErr = parseFans(e)
			continue
		}
		if st, ok := parseHost(e); ok {
			statuses = append(statuses, st)
		}
	}
	return statuses, fans, fansErr
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
// httpapi.fansEntity and presenter.Status.
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

func hasClass(e Entity, want string) bool {
	for _, c := range e.Class {
		if c == want {
			return true
		}
	}
	return false
}
