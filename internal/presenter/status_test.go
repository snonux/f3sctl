package presenter

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/snonux/f3sctl/internal/status"
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
	off := Describe(status.HostStatus{PingKnown: true})
	if !strings.HasPrefix(off, "off") {
		t.Errorf("a probed, silent host is described as %q, want it to start with \"off\"", off)
	}

	unknown := Describe(status.HostStatus{})
	if !strings.Contains(unknown, "unknown") {
		t.Errorf("an unprobeable host is described as %q, want it called unknown", unknown)
	}
}

// TestDescribeEveryProbeCombination pins the whole mapping, in particular
// that a host answering SSH is "up" whatever ICMP says: from the laptop over
// wg2 the r-nodes' firewall drops ICMP echo by design, and those hosts used to
// read "off (or hung in single-user)" while serving SSH.
func TestDescribeEveryProbeCombination(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   status.HostStatus
		want string
	}{
		{"ping and ssh", status.HostStatus{Ping: true, PingKnown: true, SSH: true}, "up"},
		{"ping only", status.HostStatus{Ping: true, PingKnown: true}, "in transition"},
		{"ssh only, ping filtered", status.HostStatus{PingKnown: true, SSH: true}, "up (ping filtered)"},
		{"ssh only, ping unmeasured", status.HostStatus{SSH: true}, "up (ping not measured)"},
		{"silent", status.HostStatus{PingKnown: true}, "off (or hung in single-user)"},
		{"unmeasured, no ssh", status.HostStatus{}, "unknown (the ping probe could not run)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Describe(tc.st); got != tc.want {
				t.Errorf("Describe(%+v) = %q, want %q", tc.st, got, tc.want)
			}
		})
	}
}

// TestStatusRendersSSHOnlyHostAsUp is the table-level regression test for
// the laptop view of r0-r2: PING no, SSH yes must render as up, never off.
func TestStatusRendersSSHOnlyHostAsUp(t *testing.T) {
	statuses := []status.HostStatus{
		{Name: "r0", IP: "10.20.0.120", PingKnown: true, SSH: true},
	}
	var buf bytes.Buffer
	if err := Status(&buf, statuses, Options{}, status.FansState{On: true, IP: "10.30.0.28"}, nil,
		status.ACState{On: true, IP: "10.30.0.29"}, nil); err != nil {
		t.Fatalf("Status: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "up (ping filtered)") {
		t.Errorf("output = %q, want the SSH-only host reported as up", out)
	}
	if strings.Contains(out, "off (or hung") {
		t.Errorf("output = %q, an SSH-reachable host must never be reported as off", out)
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
	statuses := []status.HostStatus{
		{Name: "f3", IP: "10.20.0.13", Ping: false, PingKnown: false, SSH: false},
	}

	var buf bytes.Buffer
	if err := Status(&buf, statuses, Options{ShowRole: false}, status.FansState{On: true, IP: "10.30.0.99"}, nil, status.ACState{On: true, IP: "10.30.0.29"}, nil); err != nil {
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
	statuses := []status.HostStatus{
		{Name: "f0", Role: "power", IP: "10.20.0.10", Ping: true, PingKnown: true, SSH: true},
	}

	var withRole bytes.Buffer
	if err := Status(&withRole, statuses, Options{ShowRole: true}, status.FansState{}, nil, status.ACState{}, nil); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !strings.Contains(withRole.String(), "ROLE") {
		t.Errorf("output = %q, want a ROLE header when ShowRole is true", withRole.String())
	}
	if !strings.Contains(withRole.String(), "power") {
		t.Errorf("output = %q, want the role value rendered", withRole.String())
	}

	var withoutRole bytes.Buffer
	if err := Status(&withoutRole, statuses, Options{ShowRole: false}, status.FansState{}, nil, status.ACState{}, nil); err != nil {
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
	err := Status(&buf, nil, Options{}, status.FansState{}, errors.New("dial tcp: timeout"), status.ACState{}, nil)
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
