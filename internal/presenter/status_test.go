package presenter

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/snonux/f3sctl/internal/power"
)

// TestDescribeSeparatesOffFromUnmeasured pins that the status table does not
// report a host as off on the strength of a probe that never ran.
//
// The PING column says "no" either way, so this line is the only thing
// telling the two apart -- and it is what explains why `f3sctl fans off`
// then refuses on a rack the table appears to show as cold. Formerly two
// copies of this test lived in internal/cli and internal/client, testing two
// independently-maintained describe() functions; now there is one
// implementation and one test (see ry0/hz0).
func TestDescribeSeparatesOffFromUnmeasured(t *testing.T) {
	off := Describe(power.HostStatus{PingKnown: true})
	if !strings.HasPrefix(off, "off") {
		t.Errorf("a probed, silent host is described as %q, want it to start with \"off\"", off)
	}

	unknown := Describe(power.HostStatus{})
	if !strings.Contains(unknown, "unknown") {
		t.Errorf("an unprobeable host is described as %q, want it called unknown", unknown)
	}
}

// TestStatusRendersUnknownForUnmeasuredHost is the regression test for hz0:
// the remote client's own describe(ping, ssh) never read PingKnown, so an
// unmeasured host rendered as "off (or hung in single-user)" there even
// though the server still treated it as possibly running. Exercising the
// full Status() table (as the remote client now calls it, with
// ShowRole: false) rather than Describe() alone pins the fix at the point
// that actually reaches an operator's terminal.
func TestStatusRendersUnknownForUnmeasuredHost(t *testing.T) {
	statuses := []power.HostStatus{
		{Name: "f3", IP: "192.168.1.13", Ping: false, PingKnown: false, SSH: false},
	}

	var buf bytes.Buffer
	if err := Status(&buf, statuses, Options{ShowRole: false}, power.FansState{On: true, IP: "192.168.1.99"}, nil); err != nil {
		t.Fatalf("Status: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "unknown") {
		t.Errorf("output = %q, want the unmeasured host reported as unknown", out)
	}
	if strings.Contains(out, "off (or hung") {
		t.Errorf("output = %q, an unmeasured host must never be reported as off", out)
	}
}

// TestStatusShowsRoleColumnOnlyWhenAsked pins the ry0 decision: ROLE is a
// parameter, not something either caller gets by accident. The local CLI
// passes ShowRole: true (power.Engine.ProbeAll always populates Role); the
// remote client passes ShowRole: false, because the API's documented stable
// host properties (docs/CLIENT.md section 11) do not include role.
func TestStatusShowsRoleColumnOnlyWhenAsked(t *testing.T) {
	statuses := []power.HostStatus{
		{Name: "f0", Role: "power", IP: "192.168.1.10", Ping: true, PingKnown: true, SSH: true},
	}

	var withRole bytes.Buffer
	if err := Status(&withRole, statuses, Options{ShowRole: true}, power.FansState{}, nil); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(withRole.String(), "ROLE") {
		t.Errorf("output = %q, want a ROLE header when ShowRole is true", withRole.String())
	}
	if !strings.Contains(withRole.String(), "power") {
		t.Errorf("output = %q, want the role value rendered", withRole.String())
	}

	var withoutRole bytes.Buffer
	if err := Status(&withoutRole, statuses, Options{ShowRole: false}, power.FansState{}, nil); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if strings.Contains(withoutRole.String(), "ROLE") {
		t.Errorf("output = %q, want no ROLE header when ShowRole is false", withoutRole.String())
	}
}

// TestStatusReportsAnUnreachableFanPlugAsUnknown pins that a fan-read error
// is shown as "unknown", never as "off" -- an unreachable plug is not
// evidence that it is switched off.
func TestStatusReportsAnUnreachableFanPlugAsUnknown(t *testing.T) {
	var buf bytes.Buffer
	err := Status(&buf, nil, Options{}, power.FansState{}, errors.New("dial tcp: timeout"))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(buf.String(), "rack fans: unknown (dial tcp: timeout)") {
		t.Errorf("output = %q, want the fan line to say unknown with the reason", buf.String())
	}
}

func TestYesNoAndOnOff(t *testing.T) {
	if got := YesNo(true); got != "yes" {
		t.Errorf("YesNo(true) = %q, want \"yes\"", got)
	}
	if got := YesNo(false); got != "no" {
		t.Errorf("YesNo(false) = %q, want \"no\"", got)
	}
	if got := OnOff(true); got != "on" {
		t.Errorf("OnOff(true) = %q, want \"on\"", got)
	}
	if got := OnOff(false); got != "off" {
		t.Errorf("OnOff(false) = %q, want \"off\"", got)
	}
}
