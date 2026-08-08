package power

import (
	"bytes"
	"testing"
)

// TestMagicPacket checks the wire format: six 0xFF bytes then the MAC sixteen
// times. This is worth pinning because a malformed packet fails silently --
// the NIC simply ignores it, and the only symptom is a host that does not wake.
func TestMagicPacket(t *testing.T) {
	pkt, err := magicPacket("e8:ff:1e:d7:1c:ac")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pkt) != 102 {
		t.Fatalf("packet is %d bytes, want 102 (6 sync + 16*6 MAC)", len(pkt))
	}

	if !bytes.Equal(pkt[:6], []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) {
		t.Errorf("packet does not start with the 6-byte sync stream: %x", pkt[:6])
	}

	mac := []byte{0xe8, 0xff, 0x1e, 0xd7, 0x1c, 0xac}
	for i := 0; i < 16; i++ {
		off := 6 + i*6
		if !bytes.Equal(pkt[off:off+6], mac) {
			t.Fatalf("repetition %d is %x, want %x", i, pkt[off:off+6], mac)
		}
	}
}

// TestMagicPacketRejectsBadMAC checks that a typo in the inventory is caught
// rather than producing a packet no NIC will match.
func TestMagicPacketRejectsBadMAC(t *testing.T) {
	for _, bad := range []string{"", "not-a-mac", "e8:ff:1e:d7:1c", "XX:XX:XX:XX:XX:XX"} {
		if _, err := magicPacket(bad); err == nil {
			t.Errorf("MAC %q was accepted", bad)
		}
	}
}

// TestMagicPacketRejectsEUI64 guards against a 20-octet InfiniBand-style
// address slipping through: net.ParseMAC accepts EUI-64, but Wake-on-LAN is
// defined only for EUI-48.
func TestMagicPacketRejectsEUI64(t *testing.T) {
	if _, err := magicPacket("00:00:5e:00:53:00:00:01"); err == nil {
		t.Error("an EUI-64 address was accepted; Wake-on-LAN needs a 6-byte MAC")
	}
}
