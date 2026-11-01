package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kero/internal/client"
	"kero/internal/proto"

	"golang.org/x/term"
)

// kero new's -s is tmux's habit for naming a session, an alias of -n, and
// kero attach names its target the way the rest of the CLI does.
func TestAttachUsageText(t *testing.T) {
	t.Setenv("KERO_CONFIG", filepath.Join(t.TempDir(), "config"))
	for _, want := range []string{"kero attach", "-t, --target", "-s, --name <name>"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage:\n%s\nlacks %q", usage, want)
		}
	}
}

// pickAttach picks the pane `kero attach` enters: the target's, or the
// most recently active session's focused pane for a bare attach.
func TestPickAttach(t *testing.T) {
	t.Setenv("KERO_CONFIG", filepath.Join(t.TempDir(), "config"))
	panes := func(ids ...string) []proto.Pane {
		out := make([]proto.Pane, len(ids))
		for i, id := range ids {
			out[i] = proto.Pane{ID: id}
		}
		return out
	}
	session := func(id string, activity, since, created int64, panes ...proto.Pane) proto.Session {
		return proto.Session{ID: id, Activity: activity, Since: since, Created: created, Panes: panes}
	}
	for _, tt := range []struct {
		name    string
		st      proto.State
		ref     string
		want    string
		wantErr string
	}{
		{"bare picks the most recently active", proto.State{Sessions: []proto.Session{
			session("sa", 5, 0, 1, panes("pa", "paa")...), session("sb", 3, 0, 2, panes("pb")...)}}, "", "pa", ""},
		{"bare tie goes to the smaller id", proto.State{Sessions: []proto.Session{
			session("s2", 4, 0, 1, panes("p2")...), session("s1", 4, 0, 2, panes("p1")...)}}, "", "p1", ""},
		{"bare flattens activity and since into one key", proto.State{Sessions: []proto.Session{
			session("sa", 0, 7, 1, panes("pa")...), session("sb", 3, 0, 2, panes("pb")...)}}, "", "pa", ""},
		{"bare falls to since", proto.State{Sessions: []proto.Session{
			session("sa", 0, 7, 1, panes("pa")...), session("sb", 0, 2, 9, panes("pb")...)}}, "", "pa", ""},
		{"bare falls to created", proto.State{Sessions: []proto.Session{
			session("sa", 0, 0, 1, panes("pa")...), session("sb", 0, 0, 9, panes("pb")...)}}, "", "pb", ""},
		{"bare empty state", proto.State{}, "", "", "not found"},
		{"by session id", proto.State{Sessions: []proto.Session{
			session("sa", 1, 0, 1, panes("pa")...), session("sb", 9, 0, 2, panes("pb")...)}}, "sa", "pa", ""},
		{"by unique id prefix", proto.State{Sessions: []proto.Session{
			session("sa", 1, 0, 1, panes("pa")...), session("xb", 9, 0, 2, panes("pb")...)}}, "s", "pa", ""},
		{"by pane id", proto.State{Sessions: []proto.Session{
			session("sa", 9, 0, 1, panes("pa", "p2")...)}}, "p2", "p2", ""},
		{"ambiguous prefix", proto.State{Sessions: []proto.Session{
			session("sa", 1, 0, 1, panes("pa")...), session("sb", 2, 0, 2, panes("pb")...)}}, "s", "", "names several"},
		{"by name", func() proto.State {
			s := session("sa", 0, 0, 0, panes("pa")...)
			s.Name = "dev"
			return proto.State{Sessions: []proto.Session{s, session("sb", 9, 0, 2, panes("pb")...)}}
		}(), "dev", "pa", ""},
		{"the focus pane of the session", func() proto.State {
			s := session("sa", 9, 0, 1, panes("p1", "p2")...)
			s.Focus = "p2"
			return proto.State{Sessions: []proto.Session{s}}
		}(), "sa", "p2", ""},
		{"a session with no panes", proto.State{Sessions: []proto.Session{
			session("sa", 9, 0, 1)}}, "sa", "", "no pane"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			paneID, err := pickAttach(tt.st, tt.ref)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("pickAttach(%q) = %q, %v, want an error saying %q", tt.ref, paneID, err, tt.wantErr)
				}
				if !errors.Is(err, errNotFound) {
					t.Fatalf("pickAttach(%q) error %v does not wrap errNotFound", tt.ref, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("pickAttach(%q): %v", tt.ref, err)
			}
			if paneID != tt.want {
				t.Errorf("pickAttach(%q) = %q, want %q", tt.ref, paneID, tt.want)
			}
		})
	}
}

// prefixFilter holds the tmux prefix chord: Ctrl+B then d detaches, a
// Ctrl+B alone is eaten, and an escape after the prefix is swallowed
// whole. The table feeds explicit times; the filter never sleeps.
func TestPrefixFilter(t *testing.T) {
	t.Setenv("KERO_CONFIG", filepath.Join(t.TempDir(), "config"))
	t0 := time.Unix(0, 0)
	ms := func(n int64) time.Time { return t0.Add(time.Duration(n) * time.Millisecond) }
	feeds := func(fs ...any) []struct {
		p  []byte
		at time.Time
	} {
		var out []struct {
			p  []byte
			at time.Time
		}
		for i := 0; i < len(fs); i += 2 {
			out = append(out, struct {
				p  []byte
				at time.Time
			}{[]byte(fs[i].(string)), fs[i+1].(time.Time)})
		}
		return out
	}
	for _, tt := range []struct {
		name  string
		feeds []struct {
			p  []byte
			at time.Time
		}
		want   string
		detach bool
	}{
		{"ordinary bytes pass through", feeds("abc", ms(0)), "abc", false},
		{"ordinary bytes pass split across feeds", feeds("a", ms(0), "bc", ms(1)), "abc", false},
		{"the chord detaches", feeds("\x02", ms(0), "d", ms(300)), "", true},
		{"the chord detaches at the window's edge", feeds("\x02", ms(0), "d", ms(0).Add(prefixWindow)), "", true},
		{"a lone prefix is held", feeds("\x02", ms(0)), "", false},
		{"an invalid chord byte is eaten alone", feeds("\x02", ms(0), "x", ms(100)), "", false},
		{"the filter resets after an invalid chord", feeds("\x02", ms(0), "x", ms(100), "a", ms(150)), "a", false},
		{"a laggy chord forwards the late byte", feeds("\x02", ms(0), "d", ms(600)), "d", false},
		{"a chord past the window's edge is ordinary", feeds("\x02", ms(0), "d", ms(0).Add(prefixWindow+time.Nanosecond)), "d", false},
		{"any laggy byte goes through", feeds("\x02", ms(0), "xy", ms(700)), "xy", false},
		{"a prefix a window later opens a new window", feeds("\x02", ms(0), "\x02", ms(600)), "", false},
		{"a second prefix inside the window is eaten", feeds("\x02", ms(0), "\x02", ms(100), "d", ms(200)), "d", false},
		{"an escape after the prefix drains a sequence", feeds("\x02", ms(0), "\x1b", ms(100), "\x1b[A", ms(105), "a", ms(130)), "a", false},
		{"the drain refreshes on each byte", feeds("\x02", ms(0), "\x1b", ms(100), "\x1b[A", ms(105), "b", ms(108), "a", ms(130)), "a", false},
		{"after the drain ordinary bytes pass", feeds("\x02", ms(0), "\x1b", ms(100), "a", ms(130), "zz", ms(140)), "azz", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var f prefixFilter
			var got []byte
			detach := false
			for _, fd := range tt.feeds {
				out, d := f.feed(fd.p, fd.at)
				got = append(got, out...)
				detach = detach || d
			}
			if string(got) != tt.want || detach != tt.detach {
				t.Errorf("fed -> %q, detach %v; want %q, %v", got, detach, tt.want, tt.detach)
			}
		})
	}
}

// clientFrame is a frame the CLI under test sends, as a fake kerod reads
// it.
type clientFrame struct {
	kind    byte
	payload []byte
}

// attachFake answers dials as kerod does for a raw attach: each
// connection's Hello goes to hellos, the given Welcome answers it, the
// daemon's end of the pipe goes to conns for the test to write frames
// on, and every frame the CLI sends goes to frames. The later attach
// tests script several connections through one fake.
func attachFake(t *testing.T, welcome proto.Welcome) (dial func() (*client.Stream, error), conns chan net.Conn, hellos chan proto.Hello, frames chan clientFrame) {
	t.Helper()
	conns = make(chan net.Conn, 2)
	hellos = make(chan proto.Hello, 4)
	frames = make(chan clientFrame, 64)
	dial = func() (*client.Stream, error) {
		here, there := net.Pipe()
		go func() {
			defer there.Close()
			r := bufio.NewReaderSize(there, 64<<10)
			there.SetReadDeadline(time.Now().Add(10 * time.Second))
			line, err := proto.ReadLine(r, 64<<10)
			if err != nil {
				return
			}
			there.SetReadDeadline(time.Time{})
			var h proto.Hello
			if json.Unmarshal(line, &h) == nil {
				hellos <- h
			}
			if b, err := json.Marshal(welcome); err == nil {
				there.Write(append(b, '\n'))
			}
			// The test writes frames on the daemon's end from here on;
			// handing it over any earlier would let them ahead of the
			// Welcome, whose line the client would then fail to read.
			conns <- there
			// Read what the CLI sends until the connection goes; the
			// test writes what the CLI reads on the same pipe's other
			// direction meanwhile.
			for {
				kind, payload, err := proto.ReadFrame(r)
				if err != nil {
					return
				}
				select {
				case frames <- clientFrame{kind, payload}:
				default:
				}
			}
		}()
		return client.AttachViaRaw(pipeDialer{here}, "p1")
	}
	return dial, conns, hellos, frames
}

type pipeDialer struct{ c net.Conn }

func (d pipeDialer) Dial() (net.Conn, error) { return d.c, nil }

// waitFrame waits for the CLI to send a frame of the kind, returning its
// payload.
func waitFrame(t *testing.T, frames <-chan clientFrame, kind byte) []byte {
	t.Helper()
	for timeout := time.After(5 * time.Second); ; {
		select {
		case f := <-frames:
			if f.kind == kind {
				return f.payload
			}
		case <-timeout:
			t.Fatalf("no frame of kind %d came", kind)
		}
	}
}

// waitConn waits for the fake daemon's next connection, so a loop that
// fails to reconnect fails the test rather than hanging it.
func waitConn(t *testing.T, conns <-chan net.Conn) net.Conn {
	t.Helper()
	select {
	case c := <-conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no connection came")
		return nil
	}
}

var fixedSize = func() (int, int, int, int, error) { return 80, 24, 0, 0, nil }

// A piped attach has no renderer and no size: it refuses before dialing.
func TestAttachRefusesNonTerminal(t *testing.T) {
	t.Setenv("KERO_CONFIG", filepath.Join(t.TempDir(), "config"))
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("stdin is a terminal")
	}
	if err := attachCmd(proto.State{}, nil, ""); !errors.Is(err, errUsage) {
		t.Errorf("attachCmd on a pipe = %v, want a usage error", err)
	}
}

// Frames the daemon streams arrive on stdout verbatim, and the pane's
// end ends the loop cleanly.
func TestAttachLoopWritesFrames(t *testing.T) {
	dial, conns, _, _ := attachFake(t, proto.Welcome{Version: proto.Version, Raw: true})
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inR.Close(); inW.Close() })
	// The scripted frames go out before stdin ends, so the copy of them
	// is not raced by the loop's exit.
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() { done <- attachLoop(dial, inR, &out, nil, fixedSize) }()
	conn := <-conns
	if err := proto.WriteFrame(conn, proto.FrameOutput, []byte("HELLO")); err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteFrame(conn, proto.FrameClosed, nil); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attachLoop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attachLoop did not return")
	}
	if !bytes.Contains(out.Bytes(), []byte("HELLO")) {
		t.Errorf("out %q lacks HELLO", out.String())
	}
	inW.Close()
}

// The detach chord closes the stream with nothing forwarded after it.
func TestAttachLoopDetaches(t *testing.T) {
	dial, conns, _, frames := attachFake(t, proto.Welcome{Version: proto.Version, Raw: true})
	done := make(chan error, 1)
	go func() { done <- attachLoop(dial, bytes.NewReader([]byte{0x02, 0x64, 'z'}), io.Discard, nil, fixedSize) }()
	conn := <-conns
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attachLoop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attachLoop did not detach")
	}
	// The CLI closed its end: a write on ours now fails.
	conn.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte("x")); err == nil {
		t.Error("the CLI left the stream open")
	}
	for {
		select {
		case f := <-frames:
			if f.kind == proto.FrameInput {
				t.Errorf("the pane got %q", f.payload)
			}
		default:
			return
		}
	}
}

// stdin ending — ssh died — exits the loop rather than holding a session
// nobody can see.
func TestAttachLoopStopsOnStdinEnd(t *testing.T) {
	dial, conns, _, _ := attachFake(t, proto.Welcome{Version: proto.Version, Raw: true})
	done := make(chan error, 1)
	go func() { done <- attachLoop(dial, bytes.NewReader(nil), io.Discard, nil, fixedSize) }()
	<-conns
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attachLoop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attachLoop kept running after stdin ended")
	}
}

// The loop claims the pane on entry, sizes it in pixels, and resizes it
// again when the terminal changes size, asking raw each time it dials.
func TestAttachLoopClaimsAndSizes(t *testing.T) {
	dial, conns, hellos, frames := attachFake(t, proto.Welcome{Version: proto.Version, Raw: true})
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inR.Close(); inW.Close() })
	winch := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- attachLoop(dial, inR, io.Discard, winch, fixedSize) }()
	conn := <-conns
	if h := <-hellos; h.Kind != "attach" || !h.Raw {
		t.Errorf("hello %+v, want a raw attach", h)
	}
	waitFrame(t, frames, proto.FrameClaim)
	if p := waitFrame(t, frames, proto.FrameResize); !bytes.Equal(p, proto.ResizePixelsPayload(80, 24, 0, 0)) {
		t.Errorf("resize payload %v", p)
	}
	winch <- struct{}{}
	if p := waitFrame(t, frames, proto.FrameResize); !bytes.Equal(p, proto.ResizePixelsPayload(80, 24, 0, 0)) {
		t.Errorf("second resize payload %v", p)
	}
	conn.Close()
	inW.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("attachLoop did not return")
	}
}

// A pipe has no window size: asking it is an error, which the loop takes
// as "keep the daemon's last size" — pinned here so the ask itself is
// known to fail before any claim rides on it.
func TestTermPixelsNonTerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if _, _, _, _, err := termPixels(int(r.Fd())); err == nil {
		t.Error("a pipe was said to have a size")
	}
}

// A connection that drops without a FrameClosed — the daemon's
// backpressure killed it, or the link died — reattaches: the new
// connection asks raw again, reclaims, resizes, and its first frame
// repaints the screen.
func TestAttachLoopReattaches(t *testing.T) {
	dial, conns, hellos, frames := attachFake(t, proto.Welcome{Version: proto.Version, Raw: true})
	inR, inW := io.Pipe()
	t.Cleanup(func() { inR.Close(); inW.Close() })
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- attachLoop(dial, inR, &out, nil, fixedSize) }()

	c1 := waitConn(t, conns)
	if h := <-hellos; !h.Raw {
		t.Errorf("first hello %+v not raw", h)
	}
	waitFrame(t, frames, proto.FrameClaim)
	waitFrame(t, frames, proto.FrameResize)
	if err := proto.WriteFrame(c1, proto.FrameOutput, []byte("ONE")); err != nil {
		t.Fatal(err)
	}
	// The drop: no FrameClosed, just the pipe gone.
	c1.Close()

	c2 := waitConn(t, conns)
	if h := <-hellos; !h.Raw {
		t.Errorf("reattach hello %+v not raw", h)
	}
	waitFrame(t, frames, proto.FrameClaim)
	waitFrame(t, frames, proto.FrameResize)
	if err := proto.WriteFrame(c2, proto.FrameOutput, []byte("REDRAW")); err != nil {
		t.Fatal(err)
	}
	// Leave by the chord.
	if _, err := inW.Write([]byte{0x02, 0x64}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attachLoop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attachLoop did not return")
	}
	for _, want := range []string{"ONE", "REDRAW"} {
		if !bytes.Contains(out.Bytes(), []byte(want)) {
			t.Errorf("out %q lacks %q", out.String(), want)
		}
	}
}

// A detach typed while the loop is between connections — the daemon down,
// every redial failing — still leaves: the wait between attempts must
// watch the chord too, or the retrying itself holds the user, and in raw
// mode no ^C of theirs can break out either.
func TestAttachLoopDetachesInReattachBackoff(t *testing.T) {
	t.Setenv("KERO_CONFIG", filepath.Join(t.TempDir(), "config"))
	dial, conns, _, _ := attachFake(t, proto.Welcome{Version: proto.Version, Raw: true})
	dials := 0
	// fails tells the test the first redial already failed, which only
	// happens once the loop is past the connection's select and waiting
	// between attempts — the one gap the chord has to cross.
	fails := make(chan struct{}, 1)
	failing := func() (*client.Stream, error) {
		dials++
		if dials > 1 {
			select {
			case fails <- struct{}{}:
			default:
			}
			return nil, errors.New("daemon away")
		}
		return dial()
	}
	inR, inW := io.Pipe()
	t.Cleanup(func() { inR.Close(); inW.Close() })
	done := make(chan error, 1)
	go func() { done <- attachLoop(failing, inR, io.Discard, nil, fixedSize) }()
	// The first connection stands; its drop sends the loop into redials
	// that fail. The first failure is the loop inside its backoff sleep —
	// past the connection's own select — so the chord typed after it can
	// be answered only by that sleep watching.
	c1 := waitConn(t, conns)
	c1.Close()
	<-fails
	if _, err := inW.Write([]byte{0x02, 0x64}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attachLoop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attachLoop hung in the reattach backoff, past the chord")
	}
}

// FrameClosed is the pane's end: an exit, never a reconnect — a dead
// pane would loop forever.
func TestAttachLoopExitsOnFrameClosed(t *testing.T) {
	dials := 0
	welcome := proto.Welcome{Version: proto.Version, Raw: true}
	dial, conns, _, _ := attachFake(t, welcome)
	counting := func() (*client.Stream, error) {
		dials++
		if dials > 1 {
			t.Error("reattached a pane that ended")
		}
		return dial()
	}
	// stdin stays open until the loop is done: its end is another exit,
	// and would race the pane's.
	inR, inW := io.Pipe()
	t.Cleanup(func() { inR.Close(); inW.Close() })
	done := make(chan error, 1)
	go func() { done <- attachLoop(counting, inR, io.Discard, nil, fixedSize) }()
	conn := <-conns
	proto.WriteFrame(conn, proto.FrameOutput, []byte("bye"))
	proto.WriteFrame(conn, proto.FrameClosed, nil)
	conn.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attachLoop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attachLoop did not exit on the pane's end")
	}
	inW.Close()
}

// An old daemon that knows nothing of raw answers the ordinary Mirror
// stream; its frames still go to stdout verbatim, and the pane's end
// still exits cleanly.
func TestAttachLoopMirrorFallback(t *testing.T) {
	dial, conns, _, _ := attachFake(t, proto.Welcome{Version: proto.Version})
	inR, inW := io.Pipe()
	t.Cleanup(func() { inR.Close(); inW.Close() })
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() { done <- attachLoop(dial, inR, &out, nil, fixedSize) }()
	conn := <-conns
	proto.WriteFrame(conn, proto.FrameOutput, []byte("\x1b[1Hplain"))
	proto.WriteFrame(conn, proto.FrameClosed, nil)
	conn.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attachLoop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attachLoop did not exit")
	}
	if !bytes.Contains(out.Bytes(), []byte("plain")) {
		t.Errorf("out %q lacks the mirrored bytes", out.String())
	}
	inW.Close()
}

// stdout failing — ssh died under us — ends the loop with the error, not
// a reconnect into a void.
func TestAttachLoopExitsOnStdoutError(t *testing.T) {
	dials := 0
	dial, conns, _, _ := attachFake(t, proto.Welcome{Version: proto.Version, Raw: true})
	counting := func() (*client.Stream, error) {
		dials++
		if dials > 1 {
			t.Error("redialed after stdout died")
		}
		return dial()
	}
	inR, inW := io.Pipe()
	t.Cleanup(func() { inR.Close(); inW.Close() })
	done := make(chan error, 1)
	go func() { done <- attachLoop(counting, inR, failWriter{}, nil, fixedSize) }()
	conn := <-conns
	proto.WriteFrame(conn, proto.FrameOutput, []byte("any"))
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "failWriter") {
			t.Fatalf("attachLoop = %v, want the write error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("attachLoop did not return")
	}
	conn.Close()
	inW.Close()
}

type failWriter struct{}

func (failWriter) Write(p []byte) (int, error) { return 0, errors.New("failWriter: no") }
