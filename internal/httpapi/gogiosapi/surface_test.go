package gogiosapi

import "testing"

// TestIsFolderPathCoversEveryFolderRender pins IsFolderPath against the real
// route table: exactly the routes whose response is the /gogios folder -- the
// folder itself, and gogios-cache-clear, which re-renders it -- must count,
// because enrichState fetches the gateway mute the folder's mute pair is
// judged on only for those paths. Walking Routes() rather than hardcoding the
// paths is what catches cacheClearPath drifting from the route's own Path.
func TestIsFolderPathCoversEveryFolderRender(t *testing.T) {
	rendersFolder := map[string]bool{"gogios": true, "gogios-cache-clear": true}

	seen := 0
	for _, r := range testSurface().Routes() {
		want := rendersFolder[r.Name]
		if want {
			seen++
		}
		if got := IsFolderPath(r.Path); got != want {
			t.Errorf("IsFolderPath(%q) [route %s] = %v, want %v", r.Path, r.Name, got, want)
		}
	}
	if seen != len(rendersFolder) {
		t.Errorf("found %d of the %d folder-rendering routes in Routes(): a route was renamed", seen, len(rendersFolder))
	}
}

// TestIsFolderPathRejectsLookalikes is the negative half: IsFolderPath is an
// exact match, not a prefix test, so near-misses of the two folder paths must
// not pay for the gateway mute's SSH round trips.
func TestIsFolderPathRejectsLookalikes(t *testing.T) {
	for _, path := range []string{"", "/", "/gogios/", "/gogiosx", "/gogios/cache", "/gogios/cache/clear/", "/monitoring"} {
		if IsFolderPath(path) {
			t.Errorf("IsFolderPath(%q) = true, want false", path)
		}
	}
}
