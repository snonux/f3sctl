package gogiosapi

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/snonux/f3sctl/internal/gogios"
	"github.com/snonux/f3sctl/internal/httpapi/contract"
)

// providerFor returns the Fetch sf provides for need, failing the test when
// it provides none.
func providerFor(t *testing.T, sf *Surface, need *contract.Need) contract.Fetch {
	t.Helper()
	for _, p := range sf.Providers() {
		if p.Need == need {
			return p.Fetch
		}
	}
	t.Fatalf("Surface provides no Fetch for Need %v", need)
	return nil
}

// TestProvidersCoverExactlyTheNeedsItsRoutesDeclare pins the surface's side
// of the ownership seam: every Need one of its routes declares -- other than
// the shared contract.NeedPeerBusy, which the composition root provides -- is
// one it provides itself, and it provides nothing its routes never declare,
// each Need once.
func TestProvidersCoverExactlyTheNeedsItsRoutesDeclare(t *testing.T) {
	sf := testSurface()
	var declared contract.Needs
	for _, r := range sf.Routes() {
		for _, n := range r.Needs {
			declared = declared.With(n)
		}
	}

	var provided contract.Needs
	for _, p := range sf.Providers() {
		if provided.Has(p.Need) {
			t.Errorf("Need %v is provided twice", p.Need)
		}
		if p.Fetch == nil {
			t.Errorf("Need %v is provided without a Fetch", p.Need)
		}
		provided = append(provided, p.Need)
	}

	for _, n := range declared {
		if !provided.Has(n) {
			t.Errorf("a route declares Need %v, which this surface does not provide", n)
		}
	}
	for _, n := range provided {
		if !declared.Has(n) {
			t.Errorf("this surface provides Need %v, which none of its routes declares", n)
		}
	}
}

// TestMonitoringFetchReadsThroughTheMonitor pins NeedMonitoring's Fetch: the
// gateways come from the surface's own Monitor, land where Monitoring and the
// mute predicates read them, and leave the rest of the state alone.
func TestMonitoringFetchReadsThroughTheMonitor(t *testing.T) {
	sf := New("test", contract.Hrefs(""), &fakeReports{}, fakeMonitor{}, echoActions{})
	before := contract.State{PeerBusy: true}

	got := providerFor(t, sf, NeedMonitoring)(context.Background(), before, contract.Request{})

	want := fakeMonitor{}.MonitoringStatus(context.Background())
	if gws := Monitoring(got); !slices.Equal(gws, want) {
		t.Errorf("Monitoring after the fetch = %v, want the Monitor's %v", gws, want)
	}
	if !Muted(got) {
		t.Error("Muted after fetching a muted gateway = false")
	}
	if !got.PeerBusy {
		t.Error("the fetch dropped the state it was handed (PeerBusy)")
	}
	if Monitoring(before) != nil {
		t.Error("the fetch wrote into the State it was handed rather than its own copy")
	}
}

// TestReportFetchReadsThroughTheSource pins NeedReport's Fetch: one Fetch of
// the surface's own source per call, its report -- or its error -- kept
// where Report reads it.
func TestReportFetchReadsThroughTheSource(t *testing.T) {
	sample := gogiosSample()
	errDown := errors.New("gogios down")
	for _, tc := range []struct {
		name    string
		src     *fakeReports
		want    *gogios.Report
		wantErr error
	}{
		{"report", &fakeReports{report: sample}, sample, nil},
		{"fetch error", &fakeReports{err: errDown}, nil, errDown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := providerFor(t, reportSurface(tc.src), NeedReport)(context.Background(), contract.State{}, contract.Request{})

			report, err := Report(got)
			if report != tc.want || !errors.Is(err, tc.wantErr) {
				t.Errorf("Report after the fetch = %p, %v; want %p, %v", report, err, tc.want, tc.wantErr)
			}
			if !slices.Equal(tc.src.calls, []string{"fetch"}) {
				t.Errorf("source calls = %v, want exactly one fetch", tc.src.calls)
			}
		})
	}
}

// TestUnfetchedStateReadsAsAbsent is the negative case: a State no Fetch of
// this surface has touched carries no Gogios state at all -- nil gateways
// (neither mute action is offered) and neither a report nor an error -- which
// is exactly what a route not declaring the Need is served with.
func TestUnfetchedStateReadsAsAbsent(t *testing.T) {
	s := contract.State{PeerBusy: true}
	if gws := Monitoring(s); gws != nil {
		t.Errorf("Monitoring of an unfetched state = %v, want nil", gws)
	}
	if Muted(s) || NotAllMuted(s) {
		t.Error("an unfetched mute state offers a mute action; want neither")
	}
	if report, err := Report(s); report != nil || err != nil {
		t.Errorf("Report of an unfetched state = %v, %v; want nil, nil", report, err)
	}
}
