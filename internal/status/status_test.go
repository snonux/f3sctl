package status

import (
	"encoding/json"
	"testing"
)

// TestJSONShape pins the JSON encoding of the status types byte for byte.
//
// The field names are the same ones the /status host, fans and AC entities
// use (docs/CLIENT.md), and the types moved here from internal/power without
// any change to them. A renamed tag, a dropped field or an added omitempty
// would be a silent wire change for anything marshalling these values, so it
// has to fail here first.
func TestJSONShape(t *testing.T) {
	tests := []struct {
		name string
		v    any
		want string
	}{
		{
			name: "host, full",
			v: HostStatus{Name: "f0", Role: "f", IP: "10.20.0.130", Ping: true,
				PingKnown: true, SSH: true, MS: 1.5},
			want: `{"name":"f0","role":"f","ip":"10.20.0.130","ping":true,"pingKnown":true,"ssh":true,"ms":1.5}`,
		},
		{
			// Zero values are encoded, not omitted: a missing pingKnown reads
			// as "probe completed" to docs/client-reference.js, the opposite
			// of what the zero value means.
			name: "host, zero",
			v:    HostStatus{},
			want: `{"name":"","role":"","ip":"","ping":false,"pingKnown":false,"ssh":false,"ms":0}`,
		},
		{
			name: "fans",
			v:    FansState{On: true, IP: "10.30.0.99"},
			want: `{"on":true,"ip":"10.30.0.99"}`,
		},
		{
			name: "fans, zero",
			v:    FansState{},
			want: `{"on":false,"ip":""}`,
		},
		{
			name: "ac",
			v:    ACState{On: true, IP: "10.30.0.29"},
			want: `{"on":true,"ip":"10.30.0.29"}`,
		},
		{
			name: "ac, zero",
			v:    ACState{},
			want: `{"on":false,"ip":""}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.v)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("Marshal = %s\nwant      %s", got, tt.want)
			}
		})
	}
}

// TestReachable pins that either probe answering makes a host reachable, and
// that an unmeasured ping on its own does not.
func TestReachable(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    HostStatus
		want bool
	}{
		{"ping and ssh", HostStatus{Ping: true, PingKnown: true, SSH: true}, true},
		{"ping only", HostStatus{Ping: true, PingKnown: true}, true},
		{"ssh only (icmp filtered)", HostStatus{PingKnown: true, SSH: true}, true},
		{"silent", HostStatus{PingKnown: true}, false},
		{"unmeasured, no ssh", HostStatus{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.h.Reachable(); got != tc.want {
				t.Errorf("Reachable(%+v) = %v, want %v", tc.h, got, tc.want)
			}
		})
	}
}
