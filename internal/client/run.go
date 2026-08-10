package client

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/snonux/f3sctl/internal/power"
	"github.com/snonux/f3sctl/internal/presenter"
)

// jobWaitBuffer is added on top of the server's worst-case UnmuteTimeout to
// get the client's poll deadline.
//
// UnmuteTimeout alone is not the job's full worst-case runtime: On() also
// switches the fans, sends the magic packets, and eachGateway does two SSH
// round trips to clear the mute after waitForCluster returns (see
// internal/power/gogios.go, internal/power/engine.go on()). That prelude and
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
func (c *Client) jobWaitTimeout() time.Duration {
	return c.cfg.UnmuteTimeout.D() + jobWaitBuffer
}

// actionFor maps a CLI verb onto the action name the API advertises.
//
// The mapping exists because the two vocabularies are allowed to differ: the
// CLI reads as a sentence ("power f3 on"), the API as an identifier
// ("f3-on"). Action names are part of the API's stable contract; CLI spelling
// is ours to change.
var actionFor = map[string]string{
	"power on":          "power-on",
	"power off":         "power-off",
	"fans on":           "fans-on",
	"fans off":          "fans-off",
	"monitoring mute":   "monitoring-mute",
	"monitoring unmute": "monitoring-unmute",
}

// actionName maps a CLI command to the API action it invokes.
//
// Per-host commands ("power f1 off") are derived rather than listed, so adding
// a host to the inventory needs no change here.
func actionName(cmd string) (string, bool) {
	if name, ok := actionFor[cmd]; ok {
		return name, true
	}

	fields := strings.Fields(cmd)
	if len(fields) == 3 && fields[0] == "power" && (fields[2] == "on" || fields[2] == "off") {
		return fields[1] + "-" + fields[2], true
	}
	return "", false
}

// Run executes a CLI command against the remote API.
func Run(c *Client, args []string, force bool) error {
	cmd := strings.Join(args, " ")

	if cmd == "power status" || cmd == "fans status" {
		return c.showStatus()
	}
	if cmd == "monitoring status" {
		return c.showMonitoring()
	}

	name, ok := actionName(cmd)
	if !ok {
		return fmt.Errorf("%q cannot be run remotely", cmd)
	}
	// args[0] is the CLI noun ("power", "fans", "monitoring"). Where the root
	// has a link with that rel, the matching resource is where the action is
	// advertised -- see runAction.
	return c.runAction(name, args[0], force)
}

// runAction performs a named action, looking for it on the resource named by
// holderRel and falling back to the root.
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
func (c *Client) runAction(name, holderRel string, force bool) error {
	root, err := c.Root()
	if err != nil {
		return err
	}

	holder := root
	if _, ok := root.Link(holderRel); ok {
		if e, err := c.Follow(root, holderRel); err == nil {
			holder = e
		}
	}

	action, ok := holder.Action(name)
	if !ok {
		// The server withheld it, which is information rather than an error:
		// show why by printing the state it was judged against.
		fmt.Fprintf(c.stdout, "%q is not available right now.\n\n", name)
		if strings.HasPrefix(name, "monitoring-") {
			return c.showMonitoring()
		}
		return c.showStatus()
	}

	result, err := c.Perform(action, force)
	if err != nil {
		return err
	}

	// A synchronous action (the fan plug) comes back with its new state; an
	// asynchronous one comes back as a running job to follow.
	if state, _ := result.Properties["state"].(string); state == "running" {
		id, _ := result.Properties["id"].(string)
		fmt.Fprintf(c.stdout, "%s accepted; waiting for it to finish...\n", name)
		return c.waitForJob(root, id)
	}

	if strings.HasPrefix(name, "monitoring-") {
		return c.showMonitoring()
	}

	fmt.Fprintf(c.stdout, "%s: done\n", name)
	return c.showStatus()
}

// showMonitoring renders the Gogios mute for each gateway.
//
// Followed from the root's "monitoring" link rather than fetched from a known
// path: the state is only read when asked for, because it costs the server an
// SSH round trip to each gateway.
func (c *Client) showMonitoring() error {
	root, err := c.Root()
	if err != nil {
		return err
	}
	mon, err := c.Follow(root, "monitoring")
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
func (c *Client) waitForJob(root Entity, id string) error {
	timeout := c.jobWaitTimeout()
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Second)

		job, err := c.Follow(root, "job")
		if err != nil {
			// A transient network blip mid-shutdown is expected -- the
			// cluster is, after all, being taken apart.
			fmt.Fprintf(c.stdout, "  (cannot read the job right now: %v)\n", err)
			continue
		}

		if got, _ := job.Properties["id"].(string); got != id {
			// Another node's job, or none at all. Say so plainly rather than
			// reporting someone else's outcome as this one's.
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
			return c.showStatus()
		}
	}
	return fmt.Errorf("gave up waiting for the job after %s", timeout)
}

// showStatus renders the remote status in the same shape as the local CLI, so
// --remote is a routing detail rather than a different tool.
//
// The table itself is built by internal/presenter, shared with the local CLI
// (internal/cli.printStatus) so the two cannot drift the way they had before
// this was unified -- see ry0. ShowRole is deliberately false: see
// presenter.Options.ShowRole for why the remote client does not reach into a
// host entity's "class" array for the role hiding there.
func (c *Client) showStatus() error {
	root, err := c.Root()
	if err != nil {
		return err
	}
	statusEntity, err := c.Follow(root, "status")
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
