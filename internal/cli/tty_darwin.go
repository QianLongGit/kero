//go:build darwin

package cli

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"
)

// ttyName names the terminal fd: on macOS, fcntl's F_GETPATH fills the
// path of the file the descriptor is open on.
func ttyName(f *os.File) string {
	var path [1024]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_GETPATH, uintptr(unsafe.Pointer(&path[0]))); errno != 0 {
		return ""
	}
	if i := bytes.IndexByte(path[:], 0); i > 0 {
		return string(path[:i])
	}
	return ""
}
