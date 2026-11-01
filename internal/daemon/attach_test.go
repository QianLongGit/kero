package daemon

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"kero/internal/proto"
)

// attachConn attaches to a pane as a client does, raw or not, leaving the
// connection to write input on and the reader to hear the frames of.
func attachConn(t *testing.T, d *Daemon, paneID string, raw bool) (net.Conn, *bufio.Reader, proto.Welcome) {
	t.Helper()
	// A testDaemon's client list starts empty, as serveHello writes to it.
	if d.clients == nil {
		d.clients = map[*proto.ClientInfo]struct{}{}
	}
	here, there := net.Pipe()
	go d.serve(there, identity{peer: "uid 501", via: "unix"})
	t.Cleanup(func() { here.Close() })
	writeJSON(t, here, proto.Hello{Kind: "attach", Version: proto.Version, Session: paneID, Raw: raw})
	r := bufio.NewReader(here)
	var welcome proto.Welcome
	readJSON(t, r, &welcome)
	return here, r, welcome
}

// catPane starts a pane that prints BEFORE-ATTACH and then echoes, and
// waits for the mark to reach the emulator.
func catPane(t *testing.T, d *Daemon) *pane {
	t.Helper()
	s, err := d.create(proto.CreateParams{Command: []string{"sh", "-c", `printf 'BEFORE-ATTACH'; cat`}})
	if err != nil {
		t.Fatal(err)
	}
	p := d.findPane(s.Panes[0].ID)
	if p == nil {
		t.Fatal("no pane")
	}
	for timeout := time.After(10 * time.Second); ; {
		if strings.Contains(p.readText(true, 0), "BEFORE-ATTACH") {
			return p
		}
		select {
		case <-timeout:
			t.Fatal("the pane never printed BEFORE-ATTACH")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// readOutput reads frames until the output they carry satisfies want,
// returning that output. The deadline keeps a pane that never answers from
// hanging the test.
func readOutput(t *testing.T, conn net.Conn, r *bufio.Reader, want func(got string) bool) string {
	t.Helper()
	var all strings.Builder
	for {
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		kind, payload, err := proto.ReadFrame(r)
		if err != nil {
			t.Fatalf("reading frames after %q: %v", all.String(), err)
		}
		if kind != proto.FrameOutput {
			continue
		}
		all.Write(payload)
		if want(all.String()) {
			return all.String()
		}
	}
}

// readFirstFrame reads the attach's first frame, the pane's restart.
func readFirstFrame(t *testing.T, conn net.Conn, r *bufio.Reader) []byte {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	kind, payload, err := proto.ReadFrame(r)
	if err != nil {
		t.Fatal(err)
	}
	if kind != proto.FrameOutput {
		t.Fatalf("a first frame of kind %q", kind)
	}
	return payload
}

// A raw attach's Welcome echoes raw, and its first frame is the Mirror
// restart: the scrollback so far, and the clear-home sequence that starts
// the screen over.
func TestRawAttachFirstFrameAndWelcome(t *testing.T) {
	d := testDaemon(t)
	p := catPane(t, d)
	conn, r, welcome := attachConn(t, d, p.info.ID, true)
	if welcome.Error != "" {
		t.Fatalf("welcome: %s", welcome.Error)
	}
	if !welcome.Raw {
		t.Error("the welcome does not echo raw")
	}
	if first := readFirstFrame(t, conn, r); !strings.Contains(string(first), "BEFORE-ATTACH") || !strings.Contains(string(first), "\x1b[H\x1b[2J") {
		t.Errorf("first frame %q lacks the scrollback or the restart sequence", first)
	}
}

// After the first frame a raw attachment gets the program's original
// bytes: one line in comes back twice — the tty's echo and cat's — with
// nothing else between them, no cursor addressing a diff would carry.
func TestRawAttachBytesVerbatim(t *testing.T) {
	d := testDaemon(t)
	p := catPane(t, d)
	conn, r, welcome := attachConn(t, d, p.info.ID, true)
	if welcome.Error != "" {
		t.Fatalf("welcome: %s", welcome.Error)
	}
	readFirstFrame(t, conn, r)
	if err := proto.WriteFrame(conn, proto.FrameInput, []byte("X\n")); err != nil {
		t.Fatal(err)
	}
	if got := readOutput(t, conn, r, func(got string) bool { return got == "X\r\nX\r\n" }); got != "X\r\nX\r\n" {
		t.Errorf("raw attach read %q, want the verbatim X\r\nX\r\n", got)
	}
}

// An ordinary attach's Welcome carries no raw echo, and its diff stream
// still carries the pane's output.
func TestAttachWelcomeNoRawEcho(t *testing.T) {
	d := testDaemon(t)
	p := catPane(t, d)
	conn, r, welcome := attachConn(t, d, p.info.ID, false)
	if welcome.Error != "" {
		t.Fatalf("welcome: %s", welcome.Error)
	}
	if welcome.Raw {
		t.Error("an ordinary attach got a raw echo")
	}
	readFirstFrame(t, conn, r)
	if err := proto.WriteFrame(conn, proto.FrameInput, []byte("Y\n")); err != nil {
		t.Fatal(err)
	}
	readOutput(t, conn, r, func(got string) bool { return strings.Contains(got, "Y") })
}

// A raw attachment riding the pane's output leaves a concurrent ordinary
// attachment's diff stream alone.
func TestRawDoesNotDisturbMirror(t *testing.T) {
	d := testDaemon(t)
	p := catPane(t, d)
	a, ra, wa := attachConn(t, d, p.info.ID, true)
	b, rb, wb := attachConn(t, d, p.info.ID, false)
	if wa.Error != "" || wb.Error != "" {
		t.Fatalf("welcomes: %q, %q", wa.Error, wb.Error)
	}
	readFirstFrame(t, a, ra)
	readFirstFrame(t, b, rb)
	if err := proto.WriteFrame(a, proto.FrameInput, []byte("X\n")); err != nil {
		t.Fatal(err)
	}
	if got := readOutput(t, a, ra, func(got string) bool { return got == "X\r\nX\r\n" }); got != "X\r\nX\r\n" {
		t.Errorf("raw attachment read %q, want the verbatim X\r\nX\r\n", got)
	}
	readOutput(t, b, rb, func(got string) bool { return strings.Contains(got, "X") })
}

// The raw tee rides an attachment's send, so the drop-kill contract — too
// far behind ends the attachment, it attaches again — covers raw overflow.
func TestAttachmentSendOverflowFinishes(t *testing.T) {
	a := &attachment{out: make(chan []byte, 1), done: make(chan struct{})}
	a.out <- []byte("queued")
	a.send(proto.FrameOutput, []byte("x"))
	select {
	case <-a.done:
	default:
		t.Fatal("a full out queue did not finish the attachment")
	}
}

// The full walk a phone does: attach to a pane that already ran, see the
// scrollback replayed as the first frame, type and get the program's
// bytes back, drop the connection, and attach again to the same state.
func TestAttachRawEndToEnd(t *testing.T) {
	d := testDaemon(t)
	s, err := d.create(proto.CreateParams{Command: []string{"sh", "-c", `printf 'SCROLL-MARK\n'; cat`}})
	if err != nil {
		t.Fatal(err)
	}
	p := d.findPane(s.Panes[0].ID)
	for timeout := time.After(10 * time.Second); ; {
		if strings.Contains(p.readText(true, 0), "SCROLL-MARK") {
			break
		}
		select {
		case <-timeout:
			t.Fatal("the pane never printed SCROLL-MARK")
		case <-time.After(10 * time.Millisecond):
		}
	}

	conn, r, welcome := attachConn(t, d, p.info.ID, true)
	if welcome.Error != "" {
		t.Fatalf("welcome: %s", welcome.Error)
	}
	if !welcome.Raw {
		t.Fatal("the welcome does not echo raw")
	}
	if first := readFirstFrame(t, conn, r); !strings.Contains(string(first), "SCROLL-MARK") || !strings.Contains(string(first), "\x1b[H\x1b[2J") {
		t.Fatalf("first frame %q lacks the scrollback or the restart sequence", first)
	}
	if err := proto.WriteFrame(conn, proto.FrameInput, []byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	if got := readOutput(t, conn, r, func(got string) bool { return got == "ping\r\nping\r\n" }); got != "ping\r\nping\r\n" {
		t.Fatalf("the pane's bytes came as %q, want ping twice verbatim", got)
	}
	// Drop, as a phone's link does, and come back: the second attach's
	// first frame has everything, the mark and what came since.
	conn.Close()

	conn2, r2, welcome2 := attachConn(t, d, p.info.ID, true)
	if welcome2.Error != "" {
		t.Fatalf("reattach welcome: %s", welcome2.Error)
	}
	if !welcome2.Raw {
		t.Fatal("the reattach welcome does not echo raw")
	}
	if first := readFirstFrame(t, conn2, r2); !strings.Contains(string(first), "SCROLL-MARK") || !strings.Contains(string(first), "ping") {
		t.Fatalf("reattach first frame %q lacks the scrollback or the input since", first)
	}
}
