package agent

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// zusbPool is the removable backup pool checked and exported before shutdown.
const zusbPool = "zusb"

// reexecAsRoot re-runs this binary under doas for a verb that needs
// privileges.
//
// doas.conf on the targets allows exactly these argv vectors and nothing else:
//
//	permit nopass f3sctl as root cmd /usr/local/bin/f3sctl args agent-root poweroff
//
// so the escalation is as narrow as the SSH allowlist it sits behind.
func reexecAsRoot(name string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating this binary for privilege escalation: %w", err)
	}

	cmd := exec.Command("doas", self, "agent-root", name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("doas %s: %w", name, err)
	}
	return nil
}

// runProbe reports what this host is and what it is running. Read-only; it
// exists so the restricted key can be tested without changing anything.
func runProbe() error {
	host, _ := os.Hostname()
	fmt.Printf("host: %s\n", host)

	if out, err := exec.Command("vm", "list").CombinedOutput(); err == nil {
		fmt.Printf("guests:\n%s", out)
	}
	return nil
}

// runZusbStatus reports whether the removable backup pool is currently
// imported here. Reading pool state needs no privileges.
func runZusbStatus() error {
	if err := exec.Command("zpool", "list", "-H", "-o", "name", zusbPool).Run(); err != nil {
		fmt.Println("absent")
		return nil
	}
	fmt.Println("loaded")
	return nil
}

// runZusbUnload snapshots, exports and powers down the zusb disk stack, but
// only if the pool is actually imported here.
//
// The no-op case is not an error: the caller runs this across every host it is
// about to shut down, and the pool is on at most one of them.
func runZusbUnload() error {
	if err := exec.Command("zpool", "list", "-H", "-o", "name", zusbPool).Run(); err != nil {
		fmt.Println("zusb is not imported here; nothing to do")
		return nil
	}

	out, err := exec.Command("/usr/local/bin/zusb-unload").CombinedOutput()
	fmt.Print(string(out))
	if err != nil {
		return fmt.Errorf("zusb-unload: %w", err)
	}
	return nil
}

// trimmed is a small helper for verbs that report a single line of output.
func trimmed(b []byte) string { return strings.TrimSpace(string(b)) }
