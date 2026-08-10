package cli

import (
	"strings"
	"testing"

	"github.com/snonux/f3sctl/internal/power"
)

// TestDescribeSeparatesOffFromUnmeasured pins that the status table does not
// report a host as off on the strength of a probe that never ran.
//
// The PING column says "no" either way, so this line is the only thing telling
// the two apart -- and it is what explains why `f3sctl fans off` then refuses
// on a rack the table appears to show as cold.
func TestDescribeSeparatesOffFromUnmeasured(t *testing.T) {
	off := describe(power.HostStatus{PingKnown: true})
	if !strings.HasPrefix(off, "off") {
		t.Errorf("a probed, silent host is described as %q, want it to start with \"off\"", off)
	}

	unknown := describe(power.HostStatus{})
	if !strings.Contains(unknown, "unknown") {
		t.Errorf("an unprobeable host is described as %q, want it called unknown", unknown)
	}
}
