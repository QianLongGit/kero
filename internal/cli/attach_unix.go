//go:build unix

package cli

import (
	"os"
	"os/signal"

	"golang.org/x/sys/unix"
)

// termPixels reads the terminal's grid and, for the programs that ask,
// its cell pixels — over ssh the pixels are commonly zero, which no CLI
// tool meaningfully uses. TIOCGWINSZ reports the window's whole pixels,
// but the protocol's terms are a cell's — the daemon multiplies them
// back out into the pty's — so divide through by the grid. x/term has no
// pixel API, so the ioctl itself.
func termPixels(fd int) (cols, rows, cellW, cellH int, err error) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return int(ws.Col), int(ws.Row), int(ws.Xpixel) / max(int(ws.Col), 1), int(ws.Ypixel) / max(int(ws.Row), 1), nil
}

// winchSignals reports the terminal changing size, as SIGWINCH does. It
// comes as a struct channel so the attach loop, and the tests driving
// it, speak one shape.
func winchSignals() <-chan struct{} {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, unix.SIGWINCH)
	ch := make(chan struct{}, 1)
	go func() {
		for range sig {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}()
	return ch
}
