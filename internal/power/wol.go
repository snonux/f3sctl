package power

import (
	"fmt"
	"net"

	"github.com/snonux/f3sctl/internal/inventory"
)

// magicPacket builds the Wake-on-LAN payload for mac: six 0xFF bytes followed
// by the target MAC repeated sixteen times (AMD Magic Packet format).
func magicPacket(mac string) ([]byte, error) {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return nil, fmt.Errorf("invalid MAC %q: %w", mac, err)
	}
	if len(hw) != 6 {
		return nil, fmt.Errorf("MAC %q is not 6 bytes (EUI-48)", mac)
	}

	pkt := make([]byte, 0, 6+16*6)
	for i := 0; i < 6; i++ {
		pkt = append(pkt, 0xFF)
	}
	for i := 0; i < 16; i++ {
		pkt = append(pkt, hw...)
	}
	return pkt, nil
}

// Wake sends a magic packet for h to the LAN broadcast address.
//
// Sent natively rather than by shelling out to the pkgsrc `wol` binary, which
// is one less thing to install on the Pis. UDP port 9 (discard) is the
// conventional target; the NIC's firmware matches on the payload, not the
// port, so nothing needs to be listening.
//
// A magic packet is not routed, so this only works from a host on the same
// broadcast domain as the target — pi0/pi1 and earth all sit on
// 192.168.1.0/24 with f0-f3, which is what makes the Pis viable as the API
// hosts.
func (e *Engine) Wake(h inventory.Host) error {
	if !h.Wakeable() {
		return fmt.Errorf("%s has no MAC address configured; cannot wake it", h.Name)
	}

	pkt, err := magicPacket(h.MAC)
	if err != nil {
		return err
	}

	addr := &net.UDPAddr{IP: net.ParseIP(e.cfg.Inventory.Broadcast), Port: 9}
	if addr.IP == nil {
		return fmt.Errorf("invalid broadcast address %q", e.cfg.Inventory.Broadcast)
	}

	// Dialing a broadcast address needs no explicit SO_BROADCAST on the BSDs
	// or Linux for an unconnected UDP socket bound this way, so this works
	// unprivileged — which is what lets the CGI (running as _httpd) wake
	// hosts without any doas rule.
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return fmt.Errorf("opening broadcast socket: %w", err)
	}
	// The socket has done its one job (sending the magic packet); its close
	// error is not actionable, and is explicitly discarded rather than ignored
	// so errcheck (see .golangci.yml) stays a meaningful guardrail.
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write(pkt); err != nil {
		return fmt.Errorf("sending magic packet for %s: %w", h.Name, err)
	}
	return nil
}
