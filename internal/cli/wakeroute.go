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

// errWakeOffLAN is what a refused wake returns: a default-routed wake from off
// the lab LAN with no API to route it through, any --local wake from off the
// lab LAN, and any wake whose interface list could not be read.
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
func wakeViaAPI(cfg config.Config, addrsFn interfaceAddrsFunc) (bool, error) {
	onLAN, err := onLabLAN(cfg, addrsFn)
	if err != nil {
		return false, interfaceListError(err, "Use --remote to wake through the API, or run "+
			"from the lab LAN (e.g. on pi0/pi1)")
	}
	if onLAN {
		return false, nil
	}
	if _, keyErr := cfg.ResolveAPIKey(); keyErr != nil || cfg.ResolveAPIURL() == "" {
		return false, offLANError(cfg, keyErr)
	}
	return true, nil
}

// onLabLAN reports whether any local interface is on the subnet of the
// inventory's Wake-on-LAN broadcast. err is non-nil only when the interface
// list cannot be read at all; every caller refuses the wake then rather than
// guessing, because sending blind is the silent no-op task 584 exists to stop.
func onLabLAN(cfg config.Config, addrsFn interfaceAddrsFunc) (bool, error) {
	if addrsFn == nil {
		addrsFn = net.InterfaceAddrs
	}
	addrs, err := addrsFn()
	if err != nil {
		return false, err
	}
	return onBroadcastLAN(addrs, cfg.Inventory.Broadcast), nil
}

// interfaceListError refuses a wake whose interface list could not be read,
// naming the cause and the way out (hint).
func interfaceListError(cause error, hint string) error {
	return fmt.Errorf("%w: listing the local interfaces to see whether this host is on "+
		"the lab LAN failed: %v. %s", errWakeOffLAN, cause, hint)
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
// handled=false. A refused wake reports handled=true with the error.
//
// --local never takes the API detour, but it no longer sends a packet from
// off the lab LAN either: up to v0.11.5 it did, with a warning, as an escape
// hatch for an operator who knew better than the interface check. In practice
// that packet only ever reached nobody, so --local now refuses the same way
// (localWakeError). The Pis are on the lab LAN, so a --local wake there --
// including the API's own job child, which runs with --local -- is unchanged.
func routeOffLANWake(ctx context.Context, cfg config.Config, args []string, flags globalFlags,
	addrsFn interfaceAddrsFunc, stdout, stderr io.Writer) (handled bool, err error) {

	if flags.local {
		err := localWakeError(cfg, addrsFn)
		return err != nil, err
	}
	viaAPI, err := wakeViaAPI(cfg, addrsFn)
	if err != nil || !viaAPI {
		return err != nil, err
	}
	fmt.Fprintf(stderr, "f3sctl: no local interface is on the lab LAN (broadcast %s); "+
		"waking through the API\n", cfg.Inventory.Broadcast)
	return true, runRemote(ctx, cfg, args, flags, stdout, stderr)
}

// localWakeError returns nil when a --local wake may send its magic packet
// here, i.e. an interface is on the inventory broadcast's subnet, and an
// errWakeOffLAN refusal otherwise, including when the interface list cannot
// be read. Nothing is sent on a refusal.
func localWakeError(cfg config.Config, addrsFn interfaceAddrsFunc) error {
	const hint = "Drop --local to wake through the API (needs api_url and an API key), " +
		"or run from the lab LAN (e.g. on pi0/pi1)"
	onLAN, err := onLabLAN(cfg, addrsFn)
	if err != nil {
		return interfaceListError(err, hint)
	}
	if onLAN {
		return nil
	}
	return fmt.Errorf("%w: --local was given, but no local interface is on the lab LAN "+
		"(the subnet of the Wake-on-LAN broadcast %s), and a magic packet is not routed, so "+
		"it would reach nobody; no packet was sent. %s",
		errWakeOffLAN, cfg.Inventory.Broadcast, hint)
}
