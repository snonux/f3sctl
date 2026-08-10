package infra

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPingArgsPickTheRightDeadlineFlag pins the per-OS ping(8) flag choice
// for every platform this fleet actually runs on, not just whichever one
// happens to run `go test`. Before pingArgs was split out of Ping, the
// switch could only be exercised for the host OS; cross-compiling the
// package for FreeBSD/OpenBSD/NetBSD/macOS catches it failing to build, not
// this switch choosing the wrong flag.
func TestPingArgsPickTheRightDeadlineFlag(t *testing.T) {
	for _, tc := range []struct {
		goos, flag string
	}{
		{"linux", "-w"},
		{"netbsd", "-w"},
		{"freebsd", "-t"},
		{"openbsd", "-t"},
		{"darwin", "-t"},
		// An OS this switch has never heard of falls into the same bucket as
		// netbsd/linux (see the "default" case's comment in pingArgs) rather
		// than failing outright -- ping(8) with the wrong deadline flag is
		// still safer than no deadline flag at all.
		{"plan9", "-w"},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			args := pingArgs(tc.goos, "192.0.2.1", 5*time.Second)
			want := []string{"-c", "1", tc.flag, "5", "192.0.2.1"}
			if !equalStrings(args, want) {
				t.Errorf("pingArgs(%q, ...) = %v, want %v", tc.goos, args, want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPingSeparatesSilenceFromAFailedProbe covers the tri-state contract Ping
// must never weaken: a hard failure with no "packets transmitted" summary
// reads as unknown, not down, because a probe that could not be carried out
// must not be confused with a host that stayed silent -- confusing them once
// already caused this fleet to switch off the rack fans under a live load.
// See probeRan's doc for the full incident. This test used to live in
// internal/power (as part of TestPingSeparatesSilenceFromAFailedProbe there,
// driven through Engine.pingWith); it moved here with the logic it drives,
// pingWith is now a one-line seam over this.
func TestPingSeparatesSilenceFromAFailedProbe(t *testing.T) {
	dir := t.TempDir()
	n := 0
	stub := func(exit int, output string) string {
		n++
		p := filepath.Join(dir, fmt.Sprintf("ping%d", n))
		script := fmt.Sprintf("#!/bin/sh\ncat <<'EOF'\n%s\nEOF\nexit %d\n", output, exit)
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatalf("writing the ping stub: %v", err)
		}
		return p
	}

	const stats = "--- 192.0.2.1 ping statistics ---\n" +
		"1 packets transmitted, 0 received, 100% packet loss, time 0ms"

	for _, tc := range []struct {
		name      string
		bin       string
		cancel    bool
		up, known bool
	}{
		{name: "answered", bin: stub(0, "64 bytes from 192.0.2.1"), up: true, known: true},
		{name: "no reply (linux code)", bin: stub(1, stats), known: true},
		{name: "no reply (bsd code)", bin: stub(2, stats), known: true},

		// The gap this closes. A hard failure exits non-zero and prints no
		// statistics: it never transmitted, so it says nothing about the
		// host. Counting it as a confirmed silence is how an unroutable
		// network, a switch dropping ICMP, or a ping(8) that cannot open its
		// socket used to report the entire rack down -- on every probe,
		// identically -- and take the fan plug with it.
		{name: "name resolution failed",
			bin: stub(2, "ping: no.such.host.invalid: Name or service not known")},
		{name: "socket not permitted",
			bin: stub(2, "ping: socket: Operation not permitted")},
		{name: "no route to host", bin: stub(1, "ping: connect: Network is unreachable")},

		{name: "no ping(8) at all", bin: ""},
		{name: "ping(8) cannot be run", bin: filepath.Join(dir, "absent")},
		{name: "probe cut short", bin: stub(0, ""), cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}

			up, known := Ping(ctx, tc.bin, "192.0.2.1", 2*time.Second)
			if up != tc.up || known != tc.known {
				t.Errorf("Ping = (up=%v, known=%v), want (up=%v, known=%v)",
					up, known, tc.up, tc.known)
			}
		})
	}
}

func TestProbeRanRequiresTheStatisticsLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want bool
	}{
		{"has the summary", "1 packets transmitted, 0 received", true},
		{"no summary at all", "ping: socket: Operation not permitted", false},
		{"empty output", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := probeRan([]byte(tc.out)); got != tc.want {
				t.Errorf("probeRan(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}
