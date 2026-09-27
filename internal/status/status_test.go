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
			v: HostStatus{Name: "f0", Role: "f", IP: "192.168.1.130", Ping: true,
				PingKnown: true, SSH: true, MS: 1.5},
			want: `{"name":"f0","role":"f","ip":"192.168.1.130","ping":true,"pingKnown":true,"ssh":true,"ms":1.5}`,
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
			v:    FansState{On: true, IP: "192.168.1.99"},
			want: `{"on":true,"ip":"192.168.1.99"}`,
		},
		{
			name: "fans, zero",
			v:    FansState{},
			want: `{"on":false,"ip":""}`,
		},
		{
			name: "ac",
			v:    ACState{On: true, IP: "192.168.1.29"},
			want: `{"on":true,"ip":"192.168.1.29"}`,
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
