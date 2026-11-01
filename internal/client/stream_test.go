package client

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"testing"

	"kero/internal/proto"
)

// A stream hands over FrameResized where it is in the output: after the
// output before it is read, before the output after it.
func TestStreamResizedInOrder(t *testing.T) {
	var wire bytes.Buffer
	proto.WriteFrame(&wire, proto.FrameOutput, []byte("before"))
	proto.WriteFrame(&wire, proto.FrameResized, proto.ResizePayload(100, 30))
	proto.WriteFrame(&wire, proto.FrameOutput, []byte("after"))
	s := &Stream{r: bufio.NewReader(&wire), Gone: make(chan struct{})}
	var log []string
	s.OnResized = func(cols, rows int) {
		if cols != 100 || rows != 30 {
			t.Errorf("resized to %dx%d", cols, rows)
		}
		log = append(log, "resized")
	}
	buf := make([]byte, 64)
	for {
		n, err := s.Read(buf)
		if n > 0 {
			log = append(log, string(buf[:n]))
		}
		if err == io.EOF {
			break
		}
	}
	if got := len(log); got != 3 || log[0] != "before" || log[1] != "resized" || log[2] != "after" {
		t.Errorf("read %q, want before, resized, after", log)
	}
}

// Output past what a frame carries, as a terminal's frame with the pixels
// of an image, goes in frames of its own, which a stream reads as one.
func TestStreamLargeOutput(t *testing.T) {
	out := bytes.Repeat([]byte("0123456789abcdef"), 5<<20/16) // 5 MiB
	wire := bytes.NewReader(proto.AppendFrames(nil, proto.FrameOutput, out))
	frames := 0
	for {
		kind, payload, err := proto.ReadFrame(wire)
		if err != nil {
			break
		}
		if kind != proto.FrameOutput || len(payload) > 1<<20 {
			t.Fatalf("a frame of kind %q and %d bytes", kind, len(payload))
		}
		frames++
	}
	if frames < 5 {
		t.Errorf("%d frames", frames)
	}
	s := &Stream{r: bufio.NewReader(bytes.NewReader(proto.AppendFrames(nil, proto.FrameOutput, out))), Gone: make(chan struct{})}
	got, _ := io.ReadAll(s)
	if !bytes.Equal(got, out) {
		t.Errorf("read %d bytes, not the %d written", len(got), len(out))
	}
}

// A raw attach asks for the program's original bytes in its Hello, and the
// Welcome's echo tells it the daemon honors that.
func TestAttachViaRawAsks(t *testing.T) {
	here, there := net.Pipe()
	defer here.Close()
	go func() {
		defer there.Close()
		var h proto.Hello
		if err := json.NewDecoder(there).Decode(&h); err != nil {
			t.Errorf("hello: %v", err)
			return
		}
		if h.Kind != "attach" || h.Session != "p1" || !h.Raw {
			t.Errorf("hello %+v, want a raw attach on p1", h)
		}
		w, _ := json.Marshal(proto.Welcome{Version: proto.Version, Raw: true})
		there.Write(append(w, '\n'))
	}()
	var d netDialer
	d.c = here
	st, err := AttachViaRaw(&d, "p1")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if !st.Welcome.Raw {
		t.Error("the welcome does not echo raw")
	}
}

type netDialer struct{ c net.Conn }

func (d *netDialer) Dial() (net.Conn, error) { return d.c, nil }
