package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/snonux/f3sctl/internal/config"
)

// interfaceAddrsFunc lists this machine's interface addresses. net.InterfaceAddrs
// in production; tests inject a fixed list, so where the test happens to run
// (on the lab LAN or not) cannot change which way a wake is routed.
type interfaceAddrsFunc func() ([]net.Addr, error)

// errWakeOffLAN is what a default-routed wake from off the lab LAN returns
// when there is no API to route it through instead.
var errWakeOffLAN = errors.New("cannot wake from here")

// isWake reports whether args name a valid wake: `power on`, `power all on`
// or `power <host> on`.
//
// Built on parsePowerArgs, the same parse powerActionFor and isShutdown use,
// so the routing decision and the local dispatch cannot disagree about which
// spellings are valid.
func isWake(args []string) bool {
	if len(args) == 0 || args[0] != "power" {
		return false
	}
	sp, ok := parsePowerArgs(args[1:])
	return ok && sp.verb == "on"
}

// onBroadcastLAN reports whether any of addrs sits on the subnet whose
// broadcast address is bcast, i.e. whether a magic packet sent to bcast from
// here leaves on the right wire.
//
// The test is "this interface's own subnet broadcast equals bcast" rather
// than a hard-coded 10.20.0.0/24, so a readdressed inventory (its
// "broadcast" setting) moves the check with it. An interface without a mask
// (a bare *net.IPAddr) cannot answer the question and is skipped. The limited
// broadcast 255.255.255.255 leaves on whatever link the kernel picks, so it
// counts as local; so does an address that does not parse, which Engine.Wake
// reports on its own with a clearer error than a routing decision could.
func onBroadcastLAN(addrs []net.Addr, bcast string) bool {
	want := net.ParseIP(bcast).To4()
	if want == nil || want.Equal(net.IPv4bcast) {
		return true
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP.To4()
		if ip == nil || len(ipn.Mask) != net.IPv4len {
			continue
		}
		if subnetBroadcast(ip, ipn.Mask).Equal(want) {
			return true
		}
	}
	return false
}

// subnetBroadcast returns the last address of ip's subnet under mask: every
// host bit set. Both arguments must be four bytes long.
func subnetBroadcast(ip net.IP, mask net.IPMask) net.IP {
	out := make(net.IP, net.IPv4len)
	for i := range out {
		out[i] = ip[i] | ^mask[i]
	}
	return out
}

// wakeViaAPI decides whether a wake routed locally by default must go through
// the API instead, because this machine is not on the lab LAN.
//
// It returns false (wake locally) when an interface is on the inventory
// broadcast's subnet -- pi0/pi1 always are, so their behaviour is unchanged
// -- and true when none is and an API key is configured. With neither, it
// refuses: the packet would reach nobody and the command used to report
// success anyway (task 584).
//
// An interface list that cannot be read at all also refuses rather than
// guessing: sending blind is the silent no-op this exists to stop, and
// routing a Pi through the API on a guess would change its behaviour.
func wakeViaAPI(cfg config.Config, addrsFn interfaceAddrsFunc) (bool, error) {
	if addrsFn == nil {
		addrsFn = net.InterfaceAddrs
	}
	addrs, err := addrsFn()
	if err != nil {
		return false, fmt.Errorf("%w: listing the local interfaces to see whether this host is on "+
			"the lab LAN failed: %v. Use --remote to wake through the API, or --local to send "+
			"the magic packet anyway", errWakeOffLAN, err)
	}
	if onBroadcastLAN(addrs, cfg.Inventory.Broadcast) {
		return false, nil
	}
	if _, keyErr := cfg.ResolveAPIKey(); keyErr != nil || cfg.ResolveAPIURL() == "" {
		return false, offLANError(cfg, keyErr)
	}
	return true, nil
}

// offLANError explains a refused off-LAN wake: why a local packet would be
// lost, why the API could not be used instead, and the two ways out.
func offLANError(cfg config.Config, keyErr error) error {
	why := "no API URL is configured (api_url or F3SCTL_URL)"
	if keyErr != nil {
		why = "no API key to go through the API instead (" + keyErr.Error() + ")"
	}
	return fmt.Errorf("%w: no local interface is on the lab LAN (the subnet of the "+
		"Wake-on-LAN broadcast %s), and a magic packet is not routed, so it would reach "+
		"nobody; %s. Use --remote with an API key (F3SCTL_KEY or api_key_file), or run "+
		"from the lab LAN (e.g. on pi0/pi1)", errWakeOffLAN, cfg.Inventory.Broadcast, why)
}

// routeOffLANWake handles a wake that the default routing kept local: it
// runs it through the API when this host is off the lab LAN (see wakeViaAPI)
// and reports handled=true, or leaves it to the local path with
// handled=false.
//
// --local is the documented escape hatch and is honoured: the packet is sent
// as asked, with a warning on stderr when it is unlikely to arrive, rather
// than refused -- the interface check is a heuristic and an operator who
// says --local knows something it does not.
func routeOffLANWake(ctx context.Context, cfg config.Config, args []string, flags globalFlags,
	addrsFn interfaceAddrsFunc, stdout, stderr io.Writer) (handled bool, err error) {

	if flags.local {
		warnLocalOffLANWake(cfg, addrsFn, stderr)
		return false, nil
	}
	viaAPI, err := wakeViaAPI(cfg, addrsFn)
	if err != nil || !viaAPI {
		return err != nil, err
	}
	fmt.Fprintf(stderr, "f3sctl: no local interface is on the lab LAN (broadcast %s); "+
		"waking through the API\n", cfg.Inventory.Broadcast)
	return true, runRemote(ctx, cfg, args, flags, stdout, stderr)
}

// warnLocalOffLANWake prints a warning for a --local wake from a host with no
// interface on the broadcast's subnet. A failure to list the interfaces says
// nothing either way and stays quiet.
func warnLocalOffLANWake(cfg config.Config, addrsFn interfaceAddrsFunc, stderr io.Writer) {
	if addrsFn == nil {
		addrsFn = net.InterfaceAddrs
	}
	addrs, err := addrsFn()
	if err != nil || onBroadcastLAN(addrs, cfg.Inventory.Broadcast) {
		return
	}
	fmt.Fprintf(stderr, "f3sctl: warning: no local interface is on the lab LAN (broadcast %s); "+
		"the magic packet will most likely reach nobody (drop --local to wake through the API)\n",
		cfg.Inventory.Broadcast)
}
