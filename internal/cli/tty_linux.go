//go:build linux

package cli

import (
	"fmt"
	"os"
)

// ttyName names the terminal fd: on Linux, the symlink /proc/self/fd
// keeps of it.
func ttyName(f *os.File) string {
	name, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
	if err != nil {
		return ""
	}
	return name
}
