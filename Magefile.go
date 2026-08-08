//go:build mage
// +build mage

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/magefile/mage/mg"
)

// entry is the main package built by every target.
const entry = "cmd/f3sctl/main.go"

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

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runEnv(env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Build builds the f3sctl binary for the current platform.
func Build() error {
	fmt.Println("Building...")
	return run("go", "build", "-o", "f3sctl", entry)
}

// Dev builds the f3sctl binary with race detection.
func Dev() error {
	mg.Deps(Vet, Lint)
	fmt.Println("Building with race detector...")
	return run("go", "build", "-race", "-o", "f3sctl", entry)
}

// Vet runs go vet on all go files.
func Vet() error {
	fmt.Println("Vetting...")
	return run("go", "vet", "./...")
}

// Lint runs golangci-lint.
func Lint() error {
	fmt.Println("Linting...")
	return run("golangci-lint", "run")
}

// LintInstall installs golangci-lint.
func LintInstall() error {
	fmt.Println("Installing golangci-lint...")
	return run("go", "install", "github.com/golangci/golangci-lint/cmd/golangci-lint@latest")
}

// Test runs all unit tests.
func Test() error {
	fmt.Println("Cleaning test cache...")
	if err := run("go", "clean", "-testcache"); err != nil {
		return err
	}
	fmt.Println("Running tests...")
	return run("go", "test", "./...")
}

// Install builds for the host platform and installs into ~/bin, which is how
// f3sctl reaches earth (there is no Fedora package in the f3s repo).
func Install() error {
	mg.Deps(Build)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dst := filepath.Join(home, "bin", "f3sctl")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	fmt.Printf("Installing to %s...\n", dst)
	// Remove first: overwriting a running binary in place fails with ETXTBSY.
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	return run("cp", "f3sctl", dst)
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
		out := filepath.Join("dist", fmt.Sprintf("f3sctl-%s-%s", t.goos, t.goarch))
		fmt.Printf("Cross-compiling %s/%s...\n", t.goos, t.goarch)
		env := []string{"CGO_ENABLED=0", "GOOS=" + t.goos, "GOARCH=" + t.goarch}
		if err := runEnv(env, "go", "build", "-o", out, entry); err != nil {
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
		err := run("make", "-C", pkgDir, target,
			"NAME=f3sctl",
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
