package client

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/snonux/f3sctl/internal/gogios"
)

// runGogios dispatches a `gogios` command against the remote API. args has
// the leading "gogios" token already stripped.
//
// Unlike power/fans/monitoring, most gogios verbs are GET reads discovered
// via links (showGogios/showGogiosStatus/showGogiosCheck), not actions; only
// "cache clear" is a POST, which reuses runAction the same way
// monitoring-mute/unmute do -- see runAction's doc comment.
//
// The drill-down categories come from gogios.Statuses -- the same list the
// server builds its /gogios/<status> routes and overview links from, as
// compiled into this binary; an older or newer server may differ -- rather
// than being derived from the overview's links at run time. They are CLI
// grammar: `gogios <status>` must be accepted or rejected before any request
// (as internal/cli's parseGogiosArgs does when routing here), and "detail"
// searches them in a fixed order. Deriving them would cost a round trip just
// to reject a typo and would mean guessing which overview rels (self, up,
// monitoring, ...) are categories. Each category is still reached by
// following its rel, never a literal path.
func (c *Client) runGogios(ctx context.Context, args []string, force bool) error {
	switch {
	case len(args) == 0 || (len(args) == 1 && args[0] == "status"):
		return c.showGogios(ctx)

	case len(args) == 1 && slices.Contains(gogios.Statuses(), args[0]):
		return c.showGogiosStatus(ctx, args[0])

	case len(args) >= 2 && args[0] == "detail":
		return c.showGogiosCheck(ctx, strings.Join(args[1:], " "))

	case len(args) == 2 && args[0] == "cache" && args[1] == "clear":
		return c.runAction(ctx, "gogios cache clear", "gogios", force)
	}

	return fmt.Errorf("unknown gogios command %q", strings.Join(append([]string{"gogios"}, args...), " "))
}

// showGogios renders the Gogios alert report overview, followed from the
// root's "gogios" link, then what the gogios entity offers right now (cache
// clear, monitoring mute/unmute) -- also when the report itself is
// unreachable, since clearing the cache is exactly what an operator may want
// to try then.
func (c *Client) showGogios(ctx context.Context) error {
	root, err := c.Root(ctx)
	if err != nil {
		return err
	}
	e, err := c.Follow(ctx, root, "gogios")
	if err != nil {
		return err
	}
	printGogiosOverview(c.stdout, e)
	c.printAvailable(e.Actions)
	return nil
}

// showGogiosStatus renders one drill-down category, followed from the
// gogios entity's own link for it.
func (c *Client) showGogiosStatus(ctx context.Context, status string) error {
	root, err := c.Root(ctx)
	if err != nil {
		return err
	}
	overview, err := c.Follow(ctx, root, "gogios")
	if err != nil {
		return err
	}
	list, err := c.Follow(ctx, overview, status)
	if err != nil {
		return err
	}
	printGogiosChecks(c.stdout, status, list)
	return nil
}

// showGogiosCheck renders one check's full detail, found by name.
//
// There is no hypermedia link directly to an arbitrary check by name:
// internal/httpapi's gogios-check route deliberately carries no root-level
// or overview-level link (see contract.Route's NoRootLink), since a check's own
// self link is how a client that has already listed it is meant to reach
// it -- and building that href by hand here would be the one literal API
// path in this whole package (see the package doc comment above). "detail
// <name>" exists for the different case of an operator who already knows a
// name (from an alert email, say) and has not browsed a drill-down first, so
// this instead searches the gogios.Statuses categories in order until one
// lists the name. Like the server's own Report.Check it finds the one check
// with that name (names are unique across the report), though it walks the
// report in a different order, at the cost of up to six requests instead of
// one. A category the overview does not advertise (a server built with a
// different list) is skipped rather than failing the whole search; asking
// for that category directly (`gogios <status>`) still fails.
func (c *Client) showGogiosCheck(ctx context.Context, name string) error {
	root, err := c.Root(ctx)
	if err != nil {
		return err
	}
	overview, err := c.Follow(ctx, root, "gogios")
	if err != nil {
		return err
	}

	for _, status := range gogios.Statuses() {
		if _, ok := overview.Link(status); !ok {
			continue
		}
		list, err := c.Follow(ctx, overview, status)
		if err != nil {
			return err
		}
		for _, e := range list.Entities {
			if n, _ := e.Properties["name"].(string); n == name {
				printGogiosCheck(c.stdout, e)
				return nil
			}
		}
	}
	return fmt.Errorf("no such Gogios check: %q", name)
}

// printGogiosOverview renders the gogios entity fetched by showGogios.
//
// An "error" property (see internal/httpapi/gogiosapi/handlers.go's handleOverview)
// means the report itself is currently unreachable -- shown as such rather
// than as an empty report, the same "unknown, not off" rule showMonitoring
// applies to a gateway it cannot reach.
func printGogiosOverview(out io.Writer, e Entity) {
	if msg, _ := e.Properties["error"].(string); msg != "" {
		fmt.Fprintf(out, "gogios: unknown (%s)\n", msg)
		return
	}

	fmt.Fprintln(out, gogiosProp(e, "subject"))
	fmt.Fprintf(out, "last updated: %s\n", gogiosProp(e, "lastUpdated"))

	summary, _ := e.Properties["summary"].(map[string]any)
	fmt.Fprintf(out, "critical=%d warning=%d unknown=%d stale=%d suppressed=%d ok=%d\n",
		gogiosCount(summary, "critical"), gogiosCount(summary, "warning"), gogiosCount(summary, "unknown"),
		gogiosCount(summary, "stale"), gogiosCount(summary, "suppressed"), gogiosCount(summary, "ok"))
}

// printGogiosChecks renders one drill-down collection fetched by
// showGogiosStatus.
func printGogiosChecks(out io.Writer, status string, list Entity) {
	if msg, _ := list.Properties["error"].(string); msg != "" {
		fmt.Fprintf(out, "gogios %s: unknown (%s)\n", status, msg)
		return
	}
	if len(list.Entities) == 0 {
		fmt.Fprintf(out, "no %s checks\n", status)
		return
	}
	for _, e := range list.Entities {
		fmt.Fprintf(out, "%s: %s - %s\n", gogiosProp(e, "status"), gogiosProp(e, "name"), gogiosProp(e, "output"))
	}
}

// printGogiosCheck renders one check entity's full detail, matching the
// field list internal/httpapi/gogiosapi/handlers.go's checkEntity sends: the
// conditional fields are shown only when the server actually included them
// (a zero-value JSON property, e.g. an empty string, is never sent -- see
// that function's doc comment -- so their absence here is the server's own
// signal, not something this client re-derives).
func printGogiosCheck(out io.Writer, e Entity) {
	fmt.Fprintf(out, "name:   %s\n", gogiosProp(e, "name"))
	fmt.Fprintf(out, "status: %s\n", gogiosProp(e, "status"))
	if v := gogiosProp(e, "prevStatus"); v != "" {
		fmt.Fprintf(out, "prev:   %s\n", v)
	}
	fmt.Fprintf(out, "output: %s\n", gogiosProp(e, "output"))
	if v := gogiosProp(e, "federatedFrom"); v != "" {
		fmt.Fprintf(out, "from:   %s\n", v)
	}
	if age, ok := e.Properties["lastCheckedAgeSeconds"].(float64); ok && age != 0 {
		fmt.Fprintf(out, "age:    %ds\n", int(age))
	}
}

// gogiosProp reads a string property, defaulting to "" for an absent or
// wrongly-typed one -- the same defensive-decode convention parseHost/
// parseFans already use for Entity.Properties.
func gogiosProp(e Entity, key string) string {
	s, _ := e.Properties[key].(string)
	return s
}

// gogiosCount reads one summary count. JSON numbers decode as float64 via
// encoding/json's default map[string]any handling, so this converts rather
// than leaving %d to format a float.
func gogiosCount(m map[string]any, key string) int {
	f, _ := m[key].(float64)
	return int(f)
}
