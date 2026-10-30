//go:build unix

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// Without a controlling terminal, as a hook runs under a daemon or CI,
// there is no tty to name, and no panic about it either.
func TestControllingTTY(t *testing.T) {
	if out := os.Getenv("KERO_HOOK_TTY_CHILD"); out != "" {
		os.WriteFile(out, []byte(controllingTTY()), 0o600)
		os.Exit(0)
	}
	out := filepath.Join(t.TempDir(), "tty")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run", "TestControllingTTY")
	cmd.Env = append(os.Environ(), "KERO_HOOK_TTY_CHILD="+out)
	// In a session of its own the child holds no controlling terminal,
	// as the hook process does when nothing terminal-like runs it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != "" {
		t.Fatalf("no controlling terminal, got %q", got)
	}
}
