//go:build mage
// +build mage

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
)

// binaryName is the output binary name, used both for the local build and
// the ~/bin install target.
const binaryName = "f3sctl"

// pkgPath is the main package built by every target. It names the package
// directory (not a single file inside it) so `go build` compiles every file
// in cmd/f3sctl, not just main.go.
const pkgPath = "./cmd/f3sctl"

// crossTargets are the platforms shipped through pkgrepo.f3s.buetow.org:
// netbsd/arm64 for pi0/pi1 (CGI + CLI) and freebsd/amd64 for f0-f3 (agent).
//
// The OpenBSD gateways are deliberately absent. They only ever needed to set
// and clear the Gogios mute marker, which is now done by the standalone
// conf/frontends/scripts/f3s-gogios-mute, so no f3sctl binary goes onto the
// two internet-facing hosts.
var crossTargets = []struct{ goos, goarch string }{
	{"netbsd", "arm64"},
	{"freebsd", "amd64"},
}

// Default is what a bare `mage` invocation runs. Without it, mage falls back
// to listing targets instead of doing anything useful.
//
// This must be a package-level var referencing the target function (not a
// func named Default) -- mage's parser only recognizes `var Default = ...`
// when picking the no-args target, per mage/parse.setDefault.
var Default = Build

// Build builds the f3sctl binary for the current platform.
func Build() error {
	fmt.Println("Building...")
	return sh.RunV("go", "build", "-o", binaryName, pkgPath)
}

// Dev builds the f3sctl binary with race detection.
func Dev() error {
	mg.Deps(Vet, Lint)
	fmt.Println("Building with race detector...")
	return sh.RunV("go", "build", "-race", "-o", binaryName, pkgPath)
}

// Vet runs go vet on all go files.
func Vet() error {
	fmt.Println("Vetting...")
	return sh.RunV("go", "vet", "./...")
}

// Lint runs golangci-lint.
func Lint() error {
	fmt.Println("Linting...")
	return sh.RunV("golangci-lint", "run")
}

// LintInstall installs golangci-lint.
func LintInstall() error {
	fmt.Println("Installing golangci-lint...")
	return sh.RunV("go", "install", "github.com/golangci/golangci-lint/cmd/golangci-lint@latest")
}

// Test runs all unit tests under the race detector.
//
// The detector is on for the standard target rather than hidden behind a
// separate TestRace target because the codebase runs real goroutine
// concurrency that plain `go test` never validates: power.Probe and
// RackActivity probe every host in parallel, shutdownTogether runs a
// whole shutdown batch concurrently, and quiesceEach fans a CARP-quiesce
// out across the batch -- none of which the race-free test build so much
// as looks at. A data race here is a silent job.json corruption or a torn
// shutdown progress update, so paying the ~30% the detector costs on every
// test run is the cheaper option.
//
// -race needs cgo and a C compiler, which the dev host has; this is a
// homelab tool built on the machine it runs on, not a hermetic CI matrix.
//
// This deliberately does NOT clean the test cache first: go's own test
// caching is what makes repeated `mage test` runs fast, and there was no
// recorded reason (git blame: added alongside the rest of the file in its
// initial commit, no follow-up explaining a need for forced re-runs) to pay
// that cost on every invocation. Run `go clean -testcache` by hand if a
// forced re-run is ever actually needed.
func Test() error {
	fmt.Println("Running tests with the race detector...")
	return sh.RunV("go", "test", "-race", "./...")
}

// Install builds for the host platform and installs into ~/bin, which is how
// f3sctl reaches earth (there is no Fedora package in the f3s repo).
func Install() error {
	mg.Deps(Build)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dst := filepath.Join(home, "bin", binaryName)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	fmt.Printf("Installing to %s...\n", dst)
	// Remove first: overwriting a running binary in place fails with ETXTBSY.
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	return sh.RunV("cp", binaryName, dst)
}

// Cross cross-compiles for every packaged platform into ./dist.
//
// CGO is disabled so the builds are static and need no matching toolchain;
// nothing in f3sctl requires cgo.
func Cross() error {
	if err := os.MkdirAll("dist", 0o755); err != nil {
		return err
	}
	for _, t := range crossTargets {
		out := filepath.Join("dist", fmt.Sprintf("%s-%s-%s", binaryName, t.goos, t.goarch))
		fmt.Printf("Cross-compiling %s/%s...\n", t.goos, t.goarch)
		env := map[string]string{"CGO_ENABLED": "0", "GOOS": t.goos, "GOARCH": t.goarch}
		if err := sh.RunWithV(env, "go", "build", "-o", out, pkgPath); err != nil {
			return err
		}
	}
	return nil
}

// Publish packages and uploads f3sctl to pkgrepo.f3s.buetow.org for NetBSD
// (the Pis) and FreeBSD (the f-hosts).
//
// The packaging mechanics deliberately live in ~/git/conf/packages/Makefile
// rather than here: that Makefile is shared infrastructure (gogios and dtail
// publish through it too) and owns the repo layout, the build hosts and the
// pkg_summary regeneration. Duplicating any of it here would guarantee drift.
func Publish() error {
	mg.Deps(Test)
	src, err := os.Getwd()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	pkgDir := filepath.Join(home, "git", "conf", "packages")

	for _, target := range []string{"pkg-netbsd", "pkg-freebsd"} {
		fmt.Printf("Running make %s...\n", target)
		err := sh.RunV("make", "-C", pkgDir, target,
			"NAME="+binaryName,
			"SRC="+src,
			"COMMENT=f3s homelab control tool",
			"DESC=Power, status and rack-fan control for the f3s homelab, as a CLI and a self-describing HTTP API.",
		)
		if err != nil {
			return err
		}
	}
	return nil
}
