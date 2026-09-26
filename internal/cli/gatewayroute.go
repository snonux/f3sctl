package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/snonux/f3sctl/internal/client"
	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/gogios"
)

// gatewaySwitchFor picks how a local power run reaches the Gogios mute on the
// OpenBSD gateways.
//
// The gateways accept only the restricted f3sctl key, and that key is pinned
// to pi0/pi1 (README "Security model"). A wake, though, runs locally by
// default -- a magic packet is an unprivileged LAN broadcast -- so on a laptop
// the wake itself works while the un-mute at its end has no key to SSH with.
// `monitoring unmute` never had that problem because globalFlags.useAPI
// routes every monitoring verb through the API, where pi0/pi1 make the SSH
// call with their key. This makes the wake's un-mute take the same route in
// the same situation (task 5l2), without spreading the key any further.
//
// It returns nil -- keep the SSH verb -- when a key is readable (a run on
// pi0/pi1, including the API's own detached jobs) or when --local asked for
// the homelab to be driven with the API out of the path. Otherwise it returns
// the API route; if the API is not configured either, that route fails with
// the API's own "no API key/URL" error, which says what to set up rather than
// repeating the unreadable key paths.
func gatewaySwitchFor(cfg config.Config, local bool) gogios.GatewaySwitch {
	if local {
		return nil
	}
	if _, err := cfg.ResolveSSHIdentity(); err == nil {
		return nil
	}
	return apiGatewaySwitch{cfg: cfg}
}

// apiGatewaySwitch is the gogios.GatewaySwitch that sets the mute through the
// pi0/pi1 HTTP API, the same way `f3sctl monitoring mute|unmute` does.
type apiGatewaySwitch struct {
	cfg config.Config
}

// SetMute performs the API's mute or unmute and reports each gateway in the
// shape the SSH path (gogios.Monitor.eachGateway) uses, so the wake's log and
// error read the same whichever route was taken -- plus "(via the API)", so
// an operator can tell which one it was.
func (s apiGatewaySwitch) SetMute(ctx context.Context, log io.Writer, mute bool) error {
	verb, past := "gogios-unmute", "un-muted"
	if mute {
		verb, past = "gogios-mute", "muted"
	}

	c, err := s.newClient(log)
	if err != nil {
		return fmt.Errorf("could not %s Gogios via the API: %w", verb, err)
	}
	states, err := c.SetMute(ctx, mute)
	if err != nil {
		fmt.Fprintf(log, "  ! %v\n", err)
		return fmt.Errorf("could not %s Gogios via the API: %w", verb, err)
	}
	return reportGateways(log, states, mute, verb, past)
}

// newClient builds the API client from the same config and environment
// (F3SCTL_URL / F3SCTL_KEY) runRemote uses.
func (s apiGatewaySwitch) newClient(log io.Writer) (*client.Client, error) {
	key, err := s.cfg.ResolveAPIKey()
	if err != nil {
		return nil, err
	}
	return client.New(s.cfg.ResolveAPIURL(), key, s.cfg, log)
}

// reportGateways logs one line per gateway and returns an error naming every
// gateway that is not in the wanted state: unreadable counts as failed, since
// "unknown" is not "un-muted".
func reportGateways(log io.Writer, states []gogios.GatewayMute, mute bool, verb, past string) error {
	var failed []string
	for _, st := range states {
		switch {
		case st.Err != nil:
			fmt.Fprintf(log, "  ! %s: %v\n", st.Name, st.Err)
			failed = append(failed, st.Name)
		case st.Muted != mute:
			fmt.Fprintf(log, "  ! %s: still %s\n", st.Name, muteWord(st.Muted))
			failed = append(failed, st.Name)
		default:
			fmt.Fprintf(log, "  Gogios %s on %s (via the API)\n", past, st.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not %s Gogios on: %v", verb, failed)
	}
	return nil
}

func muteWord(muted bool) string {
	if muted {
		return "muted"
	}
	return "alerting"
}
