//go:build !linux && !darwin

package cli

import "os"

// ttyName names the terminal fd, which kero's other platforms can't:
// none of them has tmux to tell a pane by.
func ttyName(*os.File) string { return "" }
