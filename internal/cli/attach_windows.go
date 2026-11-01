//go:build windows

package cli

import (
	"golang.org/x/term"
)

// termPixels reads the terminal's grid; x/sys/unix has no TIOCGWINSZ
// here, and pixel geometry is an ssh-on-a-phone nicety anyway, so zeros.
func termPixels(fd int) (cols, rows, wpx, hpx int, err error) {
	w, h, err := term.GetSize(fd)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return w, h, 0, 0, nil
}

// winchSignals: windows has no SIGWINCH, and a resize arrives as the
// next size the program asks for, so there is nothing to watch.
func winchSignals() <-chan struct{} {
	return nil
}
