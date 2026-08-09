package client

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

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
func (c *Client) waitForJob(root Entity, id string) error {
	deadline := time.Now().Add(20 * time.Minute)

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
	return fmt.Errorf("gave up waiting for the job after 20 minutes")
}

// showStatus renders the remote status in the same shape as the local CLI, so
// --remote is a routing detail rather than a different tool.
func (c *Client) showStatus() error {
	root, err := c.Root()
	if err != nil {
		return err
	}
	status, err := c.Follow(root, "status")
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(c.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HOST\tADDRESS\tPING\tSSH\tRTT\tSTATE")

	var fans *Entity
	for i, e := range status.Entities {
		if hasClass(e, "fans") {
			fans = &status.Entities[i]
			continue
		}
		name, _ := e.Properties["name"].(string)
		if name == "" {
			continue
		}
		ip, _ := e.Properties["ip"].(string)
		ping, _ := e.Properties["ping"].(bool)
		ssh, _ := e.Properties["ssh"].(bool)
		ms, _ := e.Properties["ms"].(float64)

		rtt := "-"
		if ping {
			rtt = fmt.Sprintf("%.1fms", ms)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", name, ip, yesNo(ping), yesNo(ssh), rtt, describe(ping, ssh))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	if fans != nil {
		printFans(c.stdout, *fans)
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

func printFans(out io.Writer, fans Entity) {
	if msg, _ := fans.Properties["error"].(string); msg != "" {
		// Unreachable is not the same as off, and saying "off" here would
		// send someone to the garage for nothing.
		fmt.Fprintf(out, "\nrack fans: unknown (%s)\n", msg)
		return
	}
	on, _ := fans.Properties["on"].(bool)
	fmt.Fprintf(out, "\nrack fans: %s\n", onOff(on))
}

// describe turns the two probe signals into the state they imply.
//
// The middle case is deliberately not called "booting": answering ICMP with no
// sshd means the host is in transition, and from a single observation there is
// no way to tell a host coming up from one going down. Only the job that is
// running says which, and that is the client's to combine, not this label's to
// guess.
func describe(ping, ssh bool) string {
	switch {
	case ping && ssh:
		return "up"
	case ping:
		return "in transition"
	default:
		return "off (or hung in single-user)"
	}
}

func hasClass(e Entity, want string) bool {
	for _, c := range e.Class {
		if c == want {
			return true
		}
	}
	return false
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
