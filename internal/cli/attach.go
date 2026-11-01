package cli

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"kero/internal/client"
	"kero/internal/proto"

	"golang.org/x/term"
)

// The prefix chord's timings, tmux's own: how long the filter waits for
// the byte after Ctrl+B (repeat-time), and how long a silence ends an
// escape sequence the prefix started (escape-time).
const (
	prefixWindow = 500 * time.Millisecond
	escapeQuiet  = 10 * time.Millisecond
)

// prefixFilter intercepts the detach chord in the stream of keys the user
// types, holding Ctrl+B for the byte that decides it: d detaches, an
// escape drains a whole sequence so an arrow key after the prefix is
// swallowed whole rather than torn apart, anything else is discarded
// alone, and a chord that arrives past the window costs only the prefix
// already eaten — visible, recoverable, and rare, where a longer window
// would make every eaten chord feel sluggish. The lone Ctrl+B a readline
// means as back-char being eaten matches the tmux experience asked for.
type prefixFilter struct {
	window   bool
	windowAt time.Time
	drain    bool
	lastAt   time.Time
}

// feed takes one keyboard read, at the time it arrived, returning what to
// forward to the pane and whether the user asked to detach. A batch is
// one read; intra-batch timing is noise, so every byte of it shares now.
func (f *prefixFilter) feed(p []byte, now time.Time) (out []byte, detach bool) {
	for _, b := range p {
		if f.drain {
			if now.Sub(f.lastAt) <= escapeQuiet {
				f.lastAt = now
				continue
			}
			f.drain = false
		}
		if f.window {
			if now.Sub(f.windowAt) > prefixWindow {
				// The window lapsed: the prefix was already eaten, and
				// this byte is ordinary input.
				f.window = false
			} else {
				switch b {
				case 'd':
					f.window = false
					detach = true
				case 0x1b:
					f.window = false
					f.drain, f.lastAt = true, now
				default:
					// A second Ctrl+B included: eaten, and the window
					// with it.
					f.window = false
				}
				continue
			}
		}
		if b == 0x02 {
			f.window, f.windowAt = true, now
			continue
		}
		out = append(out, b)
	}
	return out, detach
}

// lastActive is when a session last did something, for picking the one a
// bare attach enters: its program's last write, else its status's since,
// else its creation. It is the updatedKey chain of the app's sidebar
// order (order.go), copied rather than imported: the root package imports
// this one, and the reverse import would be a cycle.
func lastActive(s proto.Session) int64 {
	if s.Activity > 0 {
		return s.Activity
	}
	if s.Since > 0 {
		return s.Since
	}
	return s.Created
}

// pickAttach is the pane `kero attach` enters: the target's, or for a bare
// attach the most recently active session's focused pane — a CLI-side
// choice over the state list, no daemon change.
func pickAttach(st proto.State, ref string) (string, error) {
	if ref != "" {
		t, err := find(st, ref)
		if err != nil {
			return "", err
		}
		if t.pane != "" {
			return t.pane, nil
		}
		return focusPane(t.s)
	}
	var best *proto.Session
	for i := range st.Sessions {
		s := &st.Sessions[i]
		// The smaller id wins a tie, whatever order the state lists them.
		if best == nil || lastActive(*s) > lastActive(*best) || lastActive(*s) == lastActive(*best) && s.ID < best.ID {
			best = s
		}
	}
	if best == nil {
		return "", fmt.Errorf("%w: no session to attach to", errNotFound)
	}
	return focusPane(*best)
}

func focusPane(s proto.Session) (string, error) {
	if p := s.FocusPane(); p != nil {
		return p.ID, nil
	}
	return "", fmt.Errorf("%w: session %s has no pane", errNotFound, s.ID)
}

// attachCmd enters a session's terminal from the terminal kero runs in:
// it passes the program's original bytes both ways, the first frame
// replaying the whole screen, so the terminal on the other end — a phone
// over ssh — is the emulator. Ctrl+B d leaves, as tmux's chord does.
func attachCmd(st proto.State, args []string, host string) error {
	f, err := parse(args, map[string]string{"t": "target", "target": "target"}, nil)
	if err != nil {
		return err
	}
	// A piped attach has no renderer and no size: refuse before dialing.
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return usageErr("kero attach needs a terminal")
	}
	paneID, err := pickAttach(st, f.vals["target"])
	if err != nil {
		return err
	}
	// A host's connection is held for the command's life; each attach
	// opens a fresh stream of it, and a reattach after a drop the same.
	var d client.Dialer = client.Local{}
	if host != "" {
		c, err := dialHost(host)
		if err != nil {
			return err
		}
		defer c.Close()
		d = c
	}
	dial := func() (*client.Stream, error) { return client.AttachViaRaw(d, paneID) }
	fd := int(os.Stdin.Fd())
	raw, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	// Every exit path — detach, pane exit, fatal error, a signal from
	// outside — restores the terminal before the shell prompt returns.
	defer term.Restore(fd, raw)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	done := make(chan error, 1)
	go func() {
		done <- attachLoop(dial, os.Stdin, os.Stdout, winchSignals(), func() (int, int, int, int, error) {
			return termPixels(fd)
		})
	}()
	select {
	case err := <-done:
		return err
	case <-sig:
		// The raw-mode terminal hands ^C to the pane as a byte already;
		// a signal here is from outside, and leaves as a detach does.
		return nil
	}
}

// The reattach backoff: fast at first — a dropped raw attachment is
// invisible at 100 ms — doubling under a daemon that keeps failing,
// capped, with no bound on the attempts: only the pane's end, the user's
// detach, or a terminal of ours gone ends the loop.
const (
	reattachBase = 100 * time.Millisecond
	reattachMax  = 3 * time.Second
)

// leaveBackoff waits out the reattach pause, reporting whether the user
// left while it did. The waiting between dials is where a detach would
// otherwise strand them: in raw mode ^C is a byte for the pane, so the
// chord must carry through the backoff itself or nothing they type gets
// them out.
func leaveBackoff(detach, stdinGone <-chan struct{}, backoff time.Duration) bool {
	select {
	case <-detach:
		return true
	case <-stdinGone:
		return true
	case <-time.After(backoff):
		return false
	}
}

// attachLoop runs the attached terminal: it dials, claims the pane, sizes
// it, and passes bytes both ways until the user detaches or the pane
// ends. The daemon may kill a lagging raw attachment at any time — its
// backpressure is to drop the connection — so a clean EOF means dropped,
// not ended: the loop reattaches, and the new connection's first frame
// repaints the screen wholesale, never garbled. dial closes over the pane
// id; size yields the terminal's grid and cell pixels; winch signals a
// size change (nil on a platform with no such signal). It owns no
// terminal or signal code of its own, so the tests drive it with pipes.
func attachLoop(dial func() (*client.Stream, error), in io.Reader, out io.Writer, winch <-chan struct{}, size func() (int, int, int, int, error)) error {
	// One stdin reader for the whole run, not one per connection: two
	// readers racing for the shared in across a reconnect would lose a
	// keystroke to the dead one. Its bytes go to whichever stream is
	// current through typed; the chord closes detach, stdin's end closes
	// stdinGone, and every connection's select sees them.
	typed := make(chan []byte, 16)
	detach := make(chan struct{})
	stdinGone := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		var f prefixFilter
		for {
			n, err := in.Read(buf)
			if n > 0 {
				p, d := f.feed(buf[:n], time.Now())
				if d {
					close(detach)
					return
				}
				if len(p) > 0 {
					typed <- p
				}
			}
			if err != nil {
				close(stdinGone)
				return
			}
		}
	}()
	firstDial := true
	backoff := reattachBase
	for {
		st, err := dial()
		if err != nil {
			if firstDial {
				return err
			}
			// The daemon is between drops: keep coming back, as long as
			// it takes.
			if leaveBackoff(detach, stdinGone, backoff) {
				return nil
			}
			backoff = min(backoff*2, reattachMax)
			continue
		}
		firstDial = false
		// A connection made resets the backoff: the next drop waits
		// 100 ms again, not the doubling's tail.
		backoff = reattachBase
		// The claim is what arbitrates the pane's size; Hello's Cols
		// and Rows stay advisory, so an unclaimed attach changes
		// nothing.
		st.Claim()
		if cols, rows, w, h, err := size(); err == nil {
			st.ResizePixels(cols, rows, w, h)
		}
		copyDone := make(chan error, 1)
		go func() { _, err := io.Copy(out, st); copyDone <- err }()
	drop:
		for {
			select {
			case <-detach:
				// Leave, closing the stream so the pane knows its
				// terminal went — and waiting the copy out, which may be
				// mid-write: the close ends its read, so it always
				// finishes, never past the caller's terminal restore.
				st.Close()
				<-copyDone
				return nil
			case <-stdinGone:
				// The user is gone (ssh died): nothing to hold the pane
				// for, nothing to draw into.
				st.Close()
				<-copyDone
				return nil
			case p := <-typed:
				// A failing write shows in the copy too; the reattach
				// below is what recovers it.
				st.Write(p)
			case <-winch:
				if cols, rows, w, h, err := size(); err == nil {
					st.ResizePixels(cols, rows, w, h)
				}
			case err := <-copyDone:
				select {
				case <-st.Gone:
					// The pane ended: an exit, not a reconnect — a
					// dead pane would loop forever.
					return nil
				default:
				}
				if err != nil {
					// Writing failed — stdout is gone, as when ssh
					// died. An exit, as a detach's.
					return err
				}
				// A clean EOF with the pane alive: reattach, after the
				// backoff.
				st.Close()
				break drop
			}
		}
		// The chord may land just as the select above takes the copy's
		// end instead: the pause before the redial watches for it too.
		if leaveBackoff(detach, stdinGone, backoff) {
			return nil
		}
		backoff = min(backoff*2, reattachMax)
	}
}
