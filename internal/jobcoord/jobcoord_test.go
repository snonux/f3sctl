package jobcoord

import (
	"slices"
	"testing"
	"time"

	"github.com/snonux/f3sctl/internal/config"
	"github.com/snonux/f3sctl/internal/power"
)

// TestManagerForDerivesTheStalenessCeilingFromConfig pins the one thing the
// three callers must agree on: a job's staleness ceiling tracks both
// UnmuteTimeout and the shutdown worst case, so raising either raises it.
func TestManagerForDerivesTheStalenessCeilingFromConfig(t *testing.T) {
	cfg := config.Default()
	cfg.StateDir = t.TempDir()
	base := ManagerFor(cfg).StaleCeiling()

	cfg.UnmuteTimeout = config.Duration(cfg.UnmuteTimeout.D() + time.Hour)
	if got := ManagerFor(cfg).StaleCeiling(); got != base+time.Hour {
		t.Errorf("ceiling after +1h UnmuteTimeout = %s, want %s", got, base+time.Hour)
	}
	if power.ShutdownWorstCase(cfg) >= base {
		t.Errorf("ceiling %s does not exceed the shutdown worst case %s", base, power.ShutdownWorstCase(cfg))
	}
}

// TestPeersForAsksTheConfiguredNodesAtTheDerivedPath: the API passes its
// SCRIPT_NAME, the local plug guard nothing, and peer_job_path wins for both.
func TestPeersForAsksTheConfiguredNodesAtTheDerivedPath(t *testing.T) {
	cfg := config.Default()
	cfg.PeerNodes = []string{"192.0.2.10", "192.0.2.11"}

	tests := []struct {
		name, explicit, base, want string
	}{
		{"CGI mount", "", "/api", "/api/job"},
		{"no mount", "", "", "/cgi-bin/f3sctl/job"},
		{"explicit", "/x/job", "/api", "/x/job"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg.PeerJobPath = tt.explicit
			ps := PeersFor(cfg, tt.base)
			if ps.JobPath != tt.want || !slices.Equal(ps.Nodes, cfg.PeerNodes) {
				t.Errorf("PeersFor = %v at %q, want %v at %q", ps.Nodes, ps.JobPath, cfg.PeerNodes, tt.want)
			}
		})
	}
}
