package client

import (
	"context"
	"errors"
	"fmt"

	"github.com/snonux/f3sctl/internal/power"
)

// SetMute asks the API to mute (or un-mute) Gogios on both gateways and
// returns the per-gateway state the server reports afterwards.
//
// It is the programmatic twin of `f3sctl monitoring mute|unmute`: the same
// discovery (root -> gogios folder -> /monitoring) and the same action,
// matched by its CLI verb, but returning state instead of printing it, so the
// local wake path can use it as its un-mute transport when this machine holds
// no SSH key the gateways accept (see power.GatewaySwitch).
//
// The server withholds an action that would change nothing (unmute is only
// advertised while something is muted), so a missing action is not an error
// by itself: the state the monitoring resource reports is returned for the
// caller to judge. Only a resource that reports no gateways at all -- nothing
// to judge -- is an error here.
func (c *Client) SetMute(ctx context.Context, mute bool) ([]power.GatewayMute, error) {
	root, err := c.Root(ctx)
	if err != nil {
		return nil, err
	}
	mon, err := c.resolveHolder(ctx, root, "monitoring")
	if err != nil {
		return nil, err
	}

	verb := "monitoring unmute"
	if mute {
		verb = "monitoring mute"
	}
	if action, ok := mon.ActionForVerb(verb); ok {
		// Perform answers with the re-read monitoring resource (the server's
		// setMute), so no second GET is needed to learn the outcome.
		if mon, err = c.Perform(ctx, action, false); err != nil {
			return nil, err
		}
	}

	states := gatewayStates(mon)
	if len(states) == 0 {
		return nil, fmt.Errorf("the API offered no %q and reported no gateway state", verb)
	}
	return states, nil
}

// gatewayStates decodes the per-gateway entities of a monitoring resource.
// A gateway the server could not read carries an "error" property instead of
// "muted" (unknown is not the same as un-muted), which becomes Err here.
func gatewayStates(mon Entity) []power.GatewayMute {
	var out []power.GatewayMute
	for _, gw := range mon.Entities {
		name, _ := gw.Properties["name"].(string)
		st := power.GatewayMute{Name: name}
		if msg, _ := gw.Properties["error"].(string); msg != "" {
			st.Err = errors.New(msg)
		} else {
			st.Muted, _ = gw.Properties["muted"].(bool)
		}
		out = append(out, st)
	}
	return out
}
