package daemon

import "testing"

func TestTmuxProc(t *testing.T) {
	for _, name := range []string{"tmux", "tmux: client", "tmux: client (0)"} {
		if !tmuxProc(name) {
			t.Errorf("tmuxProc(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "zsh", "tmuxp", "node"} {
		if tmuxProc(name) {
			t.Errorf("tmuxProc(%q) = true, want false", name)
		}
	}
}
