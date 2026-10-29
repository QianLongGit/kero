package main

// The client of a transfer stream, against the wire itself: the tests
// stand in for the host's kerod on the other end of a pipe, speaking the
// exact lines serveTransfer does — the welcome, the reply, the raw body —
// so what is tested is what goes over the connection.

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"kero/internal/client"
	"kero/internal/proto"

	"kero/locales"
)

// xferFake is a kerod of a test, on the far end of a pipe: it answers
// the Hello with Welcome, the stream's one Request with Reply, and
// follows with Body bytes before the stream ends — serveTransfer's
// words, held still for a test to hear. hello and req, when they have
// room, take the lines the client sent.
type xferFake struct {
	welcome proto.Welcome
	reply   proto.Message
	body    string
	hello   chan proto.Hello
	req     chan string
}

func (f *xferFake) Dial() (net.Conn, error) {
	here, there := net.Pipe()
	go func() {
		defer there.Close()
		enc := json.NewEncoder(there)
		r := bufio.NewReader(there)
		line, err := proto.ReadLine(r, 64<<10) // the Hello
		if err != nil {
			return
		}
		if f.hello != nil {
			var h proto.Hello
			json.Unmarshal(line, &h)
			f.hello <- h
		}
		if err := enc.Encode(f.welcome); err != nil {
			return
		}
		if line, err = proto.ReadLine(r, proto.MaxLine); err != nil {
			return
		}
		if f.req != nil {
			f.req <- string(line)
		}
		enc.Encode(f.reply)
		if f.body != "" {
			there.Write([]byte(f.body))
		}
	}()
	return here, nil
}

// fakeXfer is the fake of most tests: a welcome a current kerod gives.
func fakeXfer(reply proto.Message, body string) *xferFake {
	return &xferFake{
		welcome: proto.Welcome{Version: proto.Version, PID: 4242},
		reply:   reply,
		body:    body,
		hello:   make(chan proto.Hello, 1),
	}
}

// A dial answers the stream's Hello with a welcome of its own version,
// and hands back the connection with its reader, through which the
// replies and the bodies come.
func TestTransferDial(t *testing.T) {
	f := fakeXfer(proto.Message{}, "")
	conn, br, err := transferDial(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if br == nil {
		t.Fatal("no reader with the connection")
	}
	select {
	case h := <-f.hello:
		if h.Kind != proto.KindTransfer || h.Version != proto.Version {
			t.Fatalf("hello %+v", h)
		}
	default:
		t.Fatal("no hello read")
	}
}

// A kerod of another version is refused as a control connection's is,
// for the caller to offer the restart.
func TestTransferDialVersion(t *testing.T) {
	f := &xferFake{welcome: proto.Welcome{Version: proto.Version - 1}}
	if _, _, err := transferDial(f); !errors.Is(err, client.ErrVersion) {
		t.Fatalf("err %v, want ErrVersion", err)
	}
}

// A welcome that carries an error is the daemon's word for why not, and
// comes back as it was said.
func TestTransferDialWelcomeError(t *testing.T) {
	f := &xferFake{welcome: proto.Welcome{Version: proto.Version, Error: "kerod speaks protocol 2, the client 3"}}
	_, _, err := transferDial(f)
	if err == nil || err.Error() != "kerod speaks protocol 2, the client 3" {
		t.Fatalf("err %v", err)
	}
}

// A request is one JSON line, its params riding it: the id, the method
// and the path it names, as the daemon's request reader takes them.
func TestTransferRequest(t *testing.T) {
	var b bytes.Buffer
	if err := transferRequest(&b, 7, proto.MethodTransferDownload, proto.TransferDownloadParams{Path: "~/x"}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(b.String(), "\n"); n != 1 {
		t.Fatalf("%d lines: %q", n, b.String())
	}
	var req struct {
		ID     int64                        `json:"id"`
		Method string                       `json:"method"`
		Params proto.TransferDownloadParams `json:"params"`
	}
	if err := json.Unmarshal(b.Bytes(), &req); err != nil {
		t.Fatal(err)
	}
	if req.ID != 7 || req.Method != proto.MethodTransferDownload || req.Params.Path != "~/x" {
		t.Fatalf("request %+v", req)
	}
}

// A reply's result comes back raw, for the caller to take as the type
// the method answers with.
func TestTransferReplyResult(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(`{"id":7,"result":{"size":23,"mode":420}}` + "\n"))
	res, err := transferReply(br)
	if err != nil {
		t.Fatal(err)
	}
	var h proto.TransferHeader
	if err := json.Unmarshal(res, &h); err != nil {
		t.Fatalf("result %s: %v", res, err)
	}
	if h.Size != 23 || h.Mode != 420 {
		t.Fatalf("header %+v", h)
	}
}

// An older kerod, which knows no transfer kind, takes the stream as a
// control connection and answers the method it has not: that error is
// the reply, verbatim, so the caller can tell it from a body.
func TestTransferReplyError(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(`{"id":3,"error":"unknown method \"transfer.download\""}` + "\n"))
	_, err := transferReply(br)
	if err == nil || err.Error() != `unknown method "transfer.download"` {
		t.Fatalf("err %v", err)
	}
}

// A download lands whole: the header's bytes come off the stream into a
// file of the directory and name asked, progress hearing them as they
// come, and nothing of the temp it arrived through is left behind.
func TestTransferDownloadToFile(t *testing.T) {
	body := "a file's worth of bytes"
	f := fakeXfer(proto.Message{ID: 7, Result: proto.TransferHeader{Size: int64(len(body)), Mode: 0o644}}, body)
	f.req = make(chan string, 1)
	conn, br, err := transferDial(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := transferRequest(conn, 7, proto.MethodTransferDownload, proto.TransferDownloadParams{Path: "/srv/file.txt"}); err != nil {
		t.Fatal(err)
	}
	res, err := transferReply(br)
	if err != nil {
		t.Fatal(err)
	}
	var h proto.TransferHeader
	if err := json.Unmarshal(res, &h); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	var totals []int64
	path, err := downloadToFile(br, h.Size, dest, "file.txt", func(n int64) { totals = append(totals, n) })
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dest, "file.txt")
	if path != want {
		t.Fatalf("path %s, want %s", path, want)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("body %q", got)
	}
	if len(totals) == 0 || totals[len(totals)-1] != int64(len(body)) {
		t.Fatalf("progress %v, want it to end at %d", totals, len(body))
	}
	select {
	case line := <-f.req:
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
			Params struct {
				Path string `json:"path"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Fatalf("request %s: %v", line, err)
		}
		if req.ID != 7 || req.Method != proto.MethodTransferDownload || req.Params.Path != "/srv/file.txt" {
			t.Fatalf("request %s", line)
		}
	default:
		t.Fatal("no request reached the stream")
	}
	// the temp it arrived through is gone: the file alone is in the dir
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "file.txt" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("dir holds %v", names)
	}
}

// A body is heard a chunk at a time — 256KB of it, not the copy's 32KB
// default — so the bar's update hops the UI thread once a chunk, not
// every few writes. A body of a chunk and a bit is two calls: the
// chunk, then the rest.
func TestDownloadToFileChunkedProgress(t *testing.T) {
	body := strings.Repeat("x", 300<<10)
	f := fakeXfer(proto.Message{ID: 7, Result: proto.TransferHeader{Size: int64(len(body))}}, body)
	conn, br, err := transferDial(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := transferRequest(conn, 7, proto.MethodTransferDownload, proto.TransferDownloadParams{Path: "/srv/big.bin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := transferReply(br); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	var totals []int64
	path, err := downloadToFile(br, int64(len(body)), dest, "big.bin", func(n int64) { totals = append(totals, n) })
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || len(got) != len(body) {
		t.Fatalf("body %d bytes (err %v), want %d", len(got), err, len(body))
	}
	if want := []int64{256 << 10, int64(len(body))}; !slices.Equal(totals, want) {
		t.Fatalf("progress %v, want %v", totals, want)
	}
}

// A body that ends short leaves nothing behind: no file of the half
// that came, no temp of it either — the directory as it was.
func TestDownloadToFileShortBody(t *testing.T) {
	f := fakeXfer(proto.Message{ID: 1, Result: proto.TransferHeader{Size: 10}}, "three")
	conn, br, err := transferDial(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := transferRequest(conn, 1, proto.MethodTransferDownload, proto.TransferDownloadParams{Path: "/srv/half.txt"}); err != nil {
		t.Fatal(err)
	}
	res, err := transferReply(br)
	if err != nil {
		t.Fatal(err)
	}
	var h proto.TransferHeader
	if err := json.Unmarshal(res, &h); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if _, err := downloadToFile(br, h.Size, dest, "half.txt", func(int64) {}); err == nil {
		t.Fatal("short body: no error")
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("leftovers %v", names)
	}
}

// An empty file is a body of no bytes: the file lands empty, and
// progress, which fills by the bytes that come, hears none.
func TestDownloadToFileEmpty(t *testing.T) {
	f := fakeXfer(proto.Message{ID: 4, Result: proto.TransferHeader{}}, "")
	conn, br, err := transferDial(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := transferRequest(conn, 4, proto.MethodTransferDownload, proto.TransferDownloadParams{Path: "/srv/empty"}); err != nil {
		t.Fatal(err)
	}
	res, err := transferReply(br)
	if err != nil {
		t.Fatal(err)
	}
	var h proto.TransferHeader
	if err := json.Unmarshal(res, &h); err != nil {
		t.Fatal(err)
	}
	if h.Size != 0 {
		t.Fatalf("header %+v", h)
	}
	dest := t.TempDir()
	var calls int
	path, err := downloadToFile(br, h.Size, dest, "empty", func(int64) { calls++ })
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dest, "empty")
	if path != want {
		t.Fatalf("path %s, want %s", path, want)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("body %q", got)
	}
	if calls != 0 {
		t.Fatalf("%d progress calls for no bytes", calls)
	}
}

// -- An upload: the ready hop, the body raw, the done --

// uploadFake is a kerod of an upload test, on the far end of a pipe: it
// answers the Hello with Welcome and the stream's Request with the ready
// reply — the empty header a body waits at, or the error that refuses
// one — then reads the body the request sized and answers done with
// where it landed. closeAfterReady hangs up where a daemon dying
// mid-body would, and heard carries the bytes a client sent past a
// refused ready: none, or the wire is out of order.
type uploadFake struct {
	welcome         proto.Welcome
	ready           proto.Message
	done            proto.Message
	closeAfterReady bool
	req             chan string
	body            chan []byte
	heard           chan int
}

func (f *uploadFake) Dial() (net.Conn, error) {
	here, there := net.Pipe()
	go func() {
		defer there.Close()
		enc := json.NewEncoder(there)
		r := bufio.NewReader(there)
		line, err := proto.ReadLine(r, 64<<10) // the Hello
		if err != nil {
			return
		}
		if err := enc.Encode(f.welcome); err != nil {
			return
		}
		if line, err = proto.ReadLine(r, proto.MaxLine); err != nil {
			return
		}
		if f.req != nil {
			f.req <- string(line)
		}
		if err := enc.Encode(f.ready); err != nil {
			return
		}
		var req struct {
			Params proto.TransferUploadParams `json:"params"`
		}
		json.Unmarshal(line, &req)
		if f.ready.Error != "" {
			// the refusal owes no body, so none is read: what is heard
			// past it, within a bound, is a client speaking out of turn
			there.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			buf := make([]byte, 4096)
			n, _ := r.Read(buf)
			if f.heard != nil {
				f.heard <- n
			}
			return
		}
		if f.closeAfterReady {
			return
		}
		body := make([]byte, req.Params.Size)
		if _, err := io.ReadFull(r, body); err != nil {
			return
		}
		if f.body != nil {
			f.body <- body
		}
		enc.Encode(f.done)
	}()
	return here, nil
}

// fakeUpload is the fake of an upload's happy shape: a welcome a
// current kerod gives, a ready that takes the body, and a done that says
// where the daemon landed it.
func fakeUpload(done proto.Message) *uploadFake {
	return &uploadFake{
		welcome: proto.Welcome{Version: proto.Version, PID: 4242},
		ready:   proto.Message{Result: proto.TransferHeader{}},
		done:    done,
		req:     make(chan string, 1),
		body:    make(chan []byte, 1),
	}
}

// boundedUpload runs an upload with a bound: a client wedged on a
// stream that died under it — or on a fake owed bytes that never come —
// fails within the bound instead of hanging the suite out.
func boundedUpload(conn net.Conn, br *bufio.Reader, src, dir string, progress func(int64)) (string, error) {
	type landed struct {
		path string
		err  error
	}
	res := make(chan landed, 1)
	go func() {
		path, err := uploadFile(conn, br, src, dir, progress)
		res <- landed{path, err}
	}()
	select {
	case got := <-res:
		return got.path, got.err
	case <-time.After(5 * time.Second):
		return "", errors.New("upload never returned")
	}
}

// An upload speaks its wire in order: a request whose size and mode are
// the file's own — a stat's, not a guess — and whose name is the last of
// its path; then the body raw, the bytes the file holds; then the done
// reply's path back, progress hearing the bytes as they go.
func TestUploadFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "up.txt")
	body := "an upload's worth of bytes"
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f := fakeUpload(proto.Message{Result: proto.TransferUploadDone{Path: "/srv/up.txt"}})
	conn, br, err := transferDial(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var totals []int64
	path, err := boundedUpload(conn, br, src, "/srv", func(n int64) { totals = append(totals, n) })
	if err != nil {
		t.Fatal(err)
	}
	if path != "/srv/up.txt" {
		t.Fatalf("path %s, want /srv/up.txt", path)
	}
	if len(totals) == 0 || totals[len(totals)-1] != int64(len(body)) {
		t.Fatalf("progress %v, want it to end at %d", totals, len(body))
	}
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-f.req:
		var req struct {
			Method string                      `json:"method"`
			Params proto.TransferUploadParams `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Fatalf("request %s: %v", line, err)
		}
		want := proto.TransferUploadParams{Dir: "/srv", Name: "up.txt", Size: st.Size(), Mode: uint32(st.Mode().Perm())}
		if req.Method != proto.MethodTransferUpload || req.Params != want {
			t.Fatalf("request %s, want method %s params %+v", line, proto.MethodTransferUpload, want)
		}
	default:
		t.Fatal("no request reached the stream")
	}
	select {
	case got := <-f.body:
		if string(got) != body {
			t.Fatalf("body %q, want %q", got, body)
		}
	default:
		t.Fatal("no body reached the stream")
	}
}

// The body goes out a chunk at a time too — 256KB, as a download comes
// in — so the upload's bar hops the UI thread once a chunk.
func TestUploadFileChunkedProgress(t *testing.T) {
	src := filepath.Join(t.TempDir(), "big.bin")
	body := strings.Repeat("x", 300<<10)
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f := fakeUpload(proto.Message{Result: proto.TransferUploadDone{Path: "/srv/big.bin"}})
	conn, br, err := transferDial(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var totals []int64
	if _, err := boundedUpload(conn, br, src, "/srv", func(n int64) { totals = append(totals, n) }); err != nil {
		t.Fatal(err)
	}
	if want := []int64{256 << 10, int64(len(body))}; !slices.Equal(totals, want) {
		t.Fatalf("progress %v, want %v", totals, want)
	}
}

// A ready that answers with an error — the dir not there, an old kerod
// that knows no upload — takes the whole file with it: the error comes
// back before a byte of the body streams, so nothing goes toward a
// failure already decided.
func TestUploadFileRefused(t *testing.T) {
	src := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(src, []byte("nowhere to land"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &uploadFake{
		welcome: proto.Welcome{Version: proto.Version, PID: 4242},
		ready:   proto.Message{Error: "/srv/nope is no directory"},
		req:     make(chan string, 1),
		heard:   make(chan int, 1),
	}
	conn, br, err := transferDial(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := boundedUpload(conn, br, src, "/srv/nope", func(int64) {}); err == nil || err.Error() != "/srv/nope is no directory" {
		t.Fatalf("err %v, want the ready's refusal", err)
	}
	select {
	case n := <-f.heard:
		if n != 0 {
			t.Fatalf("%d body bytes streamed past the refusal", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the fake never listened past the refusal")
	}
}

// An empty file is a body of no bytes: no write, no progress, and the
// done reply still comes — an empty file lands, not one gone missing.
func TestUploadFileEmpty(t *testing.T) {
	src := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(src, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f := fakeUpload(proto.Message{Result: proto.TransferUploadDone{Path: "/srv/empty"}})
	conn, br, err := transferDial(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var calls int
	path, err := boundedUpload(conn, br, src, "/srv", func(int64) { calls++ })
	if err != nil {
		t.Fatal(err)
	}
	if path != "/srv/empty" {
		t.Fatalf("path %s, want /srv/empty", path)
	}
	if calls != 0 {
		t.Fatalf("%d progress calls for no bytes", calls)
	}
	select {
	case line := <-f.req:
		var req struct {
			Params proto.TransferUploadParams `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Fatalf("request %s: %v", line, err)
		}
		if req.Params.Size != 0 || req.Params.Name != "empty" {
			t.Fatalf("params %+v", req.Params)
		}
	default:
		t.Fatal("no request reached the stream")
	}
}

// A stream that hangs up past its ready surfaces the body's write
// error: the upload ends with it, within a bound, rather than hanging
// on a conn that will never take another byte.
func TestUploadFileClosedAfterReady(t *testing.T) {
	src := filepath.Join(t.TempDir(), "lost.txt")
	if err := os.WriteFile(src, []byte("bytes with nowhere to go"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := fakeUpload(proto.Message{Result: proto.TransferUploadDone{Path: "/srv/lost.txt"}})
	f.closeAfterReady = true
	conn, br, err := transferDial(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var calls int
	_, err = boundedUpload(conn, br, src, "/srv", func(int64) { calls++ })
	if err == nil {
		t.Fatal("stream closed past the ready: no error")
	}
	if calls != 0 {
		t.Fatalf("%d progress calls for a body that never left", calls)
	}
}

// -- A folder's download: the tar its body carries, untarred --

// tarEntry is one entry of a tar a test writes in memory: the header's
// parts a test cares to vary — a typeflag left zero is a file — beside
// the bytes a file carries and the target a link names.
type tarEntry struct {
	name     string
	typeflag byte
	mode     int64
	mtime    time.Time
	body     string
	link     string
}

// writeTar is a folder's body as the daemon's walk would write it: the
// entries as a tar, in memory, whole.
func writeTar(t *testing.T, entries ...tarEntry) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		mode, mtime := e.mode, e.mtime
		if mode == 0 {
			mode = 0o644
		}
		if mtime.IsZero() {
			// a whole second, a USTAR's own precision, so a header stays
			// its one fixed 512 bytes
			mtime = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
		}
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: mode, ModTime: mtime, Linkname: e.link}
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if mtime.Nanosecond() != 0 {
			// a tar of the writer's own choosing rounds to the second;
			// forced to PAX it carries what the extract must give up
			hdr.Format = tar.FormatPAX
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// tarStream stands a tar's bytes where a transfer body runs: a pipe the
// bytes cross as they would a connection, so an untar reads what a
// download reads. The cleanup's close of the near end ends a writer
// still writing.
func tarStream(t *testing.T, body []byte) *bufio.Reader {
	near, far := net.Pipe()
	go func() {
		defer far.Close()
		far.Write(body)
	}()
	t.Cleanup(func() { near.Close() })
	return bufio.NewReaderSize(near, 64<<10)
}

// A folder's download lands as the tree it left: folders with their
// modes, files with their bytes and modes, symlinks as links, empty
// folders too, and the mtimes the tar carried — truncated to the
// second, for the tar may carry more than the file system keeps.
// Progress fills to the header's total and no further: the tar's own
// headers and padding ride the body beside the files that were counted.
func TestDownloadDirToTree(t *testing.T) {
	// half a second in: a PAX tar carries nanoseconds a USTAR one could
	// not, and the extract gives up the fraction
	at := time.Date(2026, 10, 29, 12, 30, 5, 500000000, time.UTC)
	body := writeTar(t,
		tarEntry{name: ".", typeflag: tar.TypeDir, mode: 0o755, mtime: at},
		tarEntry{name: "top.txt", mode: 0o600, mtime: at, body: "top level"},
		tarEntry{name: "sub", typeflag: tar.TypeDir, mode: 0o755, mtime: at.Add(time.Minute)},
		tarEntry{name: "sub/nested.txt", mode: 0o644, mtime: at, body: "nested level"},
		tarEntry{name: "empty", typeflag: tar.TypeDir, mode: 0o755, mtime: at},
		tarEntry{name: "link", typeflag: tar.TypeSymlink, mode: 0o777, mtime: at, link: "top.txt"},
	)
	dest := t.TempDir()
	total := int64(len("top level") + len("nested level"))
	var last int64
	root, err := downloadDirTo(tarStream(t, body), total, dest, "proj", func(n int64) { last = n })
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dest, "proj"); root != want {
		t.Fatalf("root %s, want %s", root, want)
	}
	if last != total {
		t.Fatalf("progress ended at %d, want %d", last, total)
	}
	for name, want := range map[string]string{"top.txt": "top level", "sub/nested.txt": "nested level"} {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s: body %q, want %q", name, got, want)
		}
	}
	link, err := os.Readlink(filepath.Join(root, "link"))
	if err != nil || link != "top.txt" {
		t.Errorf("link %q, err %v: want top.txt", link, err)
	}
	if st, err := os.Lstat(filepath.Join(root, "empty")); err != nil || !st.IsDir() {
		t.Errorf("empty folder: %v", err)
	}
	for name, want := range map[string]os.FileMode{
		".": 0o755, "top.txt": 0o600, "sub": 0o755, "sub/nested.txt": 0o644, "empty": 0o755,
	} {
		st, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != want {
			t.Errorf("%s: mode %o, want %o", name, st.Mode().Perm(), want)
		}
	}
	for name, want := range map[string]time.Time{
		".": at, "top.txt": at, "sub": at.Add(time.Minute), "sub/nested.txt": at, "empty": at,
	} {
		st, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if got := st.ModTime(); !got.Equal(want.Truncate(time.Second)) {
			t.Errorf("%s: mtime %v, want %v", name, got, want.Truncate(time.Second))
		}
	}
}

// An entry that would climb out of the folder it downloads into is the
// tar's whole answer rejected: the transfer aborts and nothing of it
// stays — no evil outside the folder it fills, not the folder it had
// begun to fill either.
func TestDownloadDirToTraversal(t *testing.T) {
	body := writeTar(t,
		tarEntry{name: ".", typeflag: tar.TypeDir, mode: 0o755},
		tarEntry{name: "../evil", body: "outside"},
	)
	dest := t.TempDir()
	if _, err := downloadDirTo(tarStream(t, body), int64(len("outside")), dest, "proj", func(int64) {}); err == nil {
		t.Fatal("../evil entry: no error")
	}
	// ../ from the folder the download fills lands in the destination
	// itself, a level up from the folder
	if _, err := os.Lstat(filepath.Join(dest, "evil")); !os.IsNotExist(err) {
		t.Fatalf("evil outside the folder: %v", err)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("leftovers %v", names)
	}
}

// A symlink an earlier entry made is a door out of the folder a
// download fills: a later entry whose path runs through it writes where
// the link points, outside the destination, with no error to show for
// it. Such an entry is the tar's whole answer refused — nothing lands
// outside, and the folder itself goes as it came.
func TestDownloadDirToSymlinkEscape(t *testing.T) {
	body := writeTar(t,
		tarEntry{name: ".", typeflag: tar.TypeDir, mode: 0o755},
		tarEntry{name: "x", typeflag: tar.TypeSymlink, mode: 0o777, link: "../.."},
		tarEntry{name: "x/pwned.txt", body: "gotcha"},
	)
	dest := t.TempDir()
	if _, err := downloadDirTo(tarStream(t, body), int64(len("gotcha")), dest, "proj", func(int64) {}); err == nil {
		t.Fatal("entry through a symlink: no error")
	}
	// the link points at the destination's parent: that is where the
	// entry would have landed
	if _, err := os.Lstat(filepath.Join(filepath.Dir(dest), "pwned.txt")); !os.IsNotExist(err) {
		t.Fatalf("pwned.txt outside the destination: %v", err)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("leftovers %v", names)
	}
}

// An entry that lands on a symlink an earlier entry made is refused
// too: a folder there would take its mode and mtime through the link —
// os.Chmod follows it — and write both outside the destination.
func TestDownloadDirToDirOnSymlink(t *testing.T) {
	body := writeTar(t,
		tarEntry{name: ".", typeflag: tar.TypeDir, mode: 0o755},
		tarEntry{name: "x", typeflag: tar.TypeSymlink, mode: 0o777, link: "../.."},
		tarEntry{name: "x", typeflag: tar.TypeDir, mode: 0o755},
	)
	dest := t.TempDir()
	if _, err := downloadDirTo(tarStream(t, body), 0, dest, "proj", func(int64) {}); err == nil {
		t.Fatal("entry onto a symlink: no error")
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("leftovers %v", names)
	}
}

// A symlink over the extract's own root — the "." entry, made a link —
// would carry every later entry out of the destination, for the extract
// writes through a root that no longer is one. A symlink may only take
// a name nothing holds, the root's own least of all.
func TestDownloadDirToSymlinkRootSwap(t *testing.T) {
	body := writeTar(t,
		tarEntry{name: ".", typeflag: tar.TypeDir, mode: 0o755},
		tarEntry{name: ".", typeflag: tar.TypeSymlink, mode: 0o777, link: "../.."},
		tarEntry{name: "pwned.txt", body: "gotcha"},
	)
	dest := t.TempDir()
	if _, err := downloadDirTo(tarStream(t, body), int64(len("gotcha")), dest, "proj", func(int64) {}); err == nil {
		t.Fatal("symlink over the root: no error")
	}
	// the swapped root pointed at the destination's parent: that is
	// where the file would have landed
	if _, err := os.Lstat(filepath.Join(filepath.Dir(dest), "pwned.txt")); !os.IsNotExist(err) {
		t.Fatalf("pwned.txt outside the destination: %v", err)
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("leftovers %v", names)
	}
}

// A folder the tar gave, then a symlink over it: the mode and mtime the
// extract gives back at its end would follow the link out of the
// destination onto a path of the attacker's choosing, no error to show.
// The folder stays a folder — a symlink may only take a name nothing
// holds — and nothing outside the destination moves.
func TestDownloadDirToSymlinkOverFolder(t *testing.T) {
	// the door out: a folder of the test's own, past the destination
	outside := t.TempDir()
	before, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	body := writeTar(t,
		tarEntry{name: ".", typeflag: tar.TypeDir, mode: 0o755},
		tarEntry{name: "d", typeflag: tar.TypeDir, mode: 0o707, mtime: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)},
		tarEntry{name: "d", typeflag: tar.TypeSymlink, mode: 0o777, link: outside},
	)
	dest := t.TempDir()
	if _, err := downloadDirTo(tarStream(t, body), 0, dest, "proj", func(int64) {}); err == nil {
		t.Fatal("symlink over a folder the tar gave: no error")
	}
	after, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("mode outside the destination %o, was %o", after.Mode().Perm(), before.Mode().Perm())
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("mtime outside the destination %v, was %v", after.ModTime(), before.ModTime())
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("leftovers %v", names)
	}
}

// An absolute entry name is refused the same on every platform: the
// daemon's names are slash-separated, so the test of them is the path
// package's — an absolute that a Windows filepath would not call one
// still escapes.
func TestDownloadDirToAbsolute(t *testing.T) {
	body := writeTar(t,
		tarEntry{name: ".", typeflag: tar.TypeDir, mode: 0o755},
		tarEntry{name: "/abs", body: "absolute"},
	)
	dest := t.TempDir()
	if _, err := downloadDirTo(tarStream(t, body), int64(len("absolute")), dest, "proj", func(int64) {}); err == nil {
		t.Fatal("/abs entry: no error")
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("leftovers %v", names)
	}
}

// A folder a download would create beside one of its own name takes the
// name a paste would — "proj copy" — and what the first proj held stays
// as it was.
func TestDownloadDirToExistingName(t *testing.T) {
	dest := t.TempDir()
	old := filepath.Join(dest, "proj", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := writeTar(t,
		tarEntry{name: ".", typeflag: tar.TypeDir, mode: 0o755},
		tarEntry{name: "new.txt", body: "new"},
	)
	root, err := downloadDirTo(tarStream(t, body), int64(len("new")), dest, "proj", func(int64) {})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dest, "proj copy"); root != want {
		t.Fatalf("root %s, want %s", root, want)
	}
	got, err := os.ReadFile(filepath.Join(root, "new.txt"))
	if err != nil || string(got) != "new" {
		t.Fatalf("new.txt %q, err %v", got, err)
	}
	if got, err := os.ReadFile(old); err != nil || string(got) != "kept" {
		t.Fatalf("the taken folder's keep.txt %q, err %v", got, err)
	}
}

// A tar that ends before it is done — the daemon's walk failed, the
// stream dropped — leaves nothing behind: no half file under any name
// the download would take, no temp of one, not the folder itself.
func TestDownloadDirToTruncated(t *testing.T) {
	full := writeTar(t, tarEntry{name: "half.txt", body: strings.Repeat("x", 600)})
	cut := full[:512+300] // the header whole, 300 of the file's 600 bytes
	dest := t.TempDir()
	if _, err := downloadDirTo(tarStream(t, cut), 600, dest, "proj", func(int64) {}); err == nil {
		t.Fatal("truncated tar: no error")
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("leftovers %v", names)
	}
}

// A hard link — or any entry a download of files and folders cannot
// hold — is walked past silently: no error, and nothing of it lands.
func TestDownloadDirToSkipsHardLink(t *testing.T) {
	body := writeTar(t,
		tarEntry{name: ".", typeflag: tar.TypeDir, mode: 0o755},
		tarEntry{name: "target.txt", body: "the target"},
		tarEntry{name: "hard", typeflag: tar.TypeLink, mode: 0o644, link: "target.txt"},
	)
	dest := t.TempDir()
	root, err := downloadDirTo(tarStream(t, body), int64(len("the target")), dest, "proj", func(int64) {})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "hard")); !os.IsNotExist(err) {
		t.Fatalf("hard link landed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "target.txt"))
	if err != nil || string(got) != "the target" {
		t.Fatalf("target.txt %q, err %v", got, err)
	}
}

// A header that sizes its body small — or the tar's own overhead, which
// rides the body beside the files that were counted — never shows past
// its total: progress is clamped, for a bar that overran its whole
// would look broken, not merely wrong.
func TestDownloadDirToClampedProgress(t *testing.T) {
	body := writeTar(t,
		tarEntry{name: ".", typeflag: tar.TypeDir, mode: 0o755},
		tarEntry{name: "f.txt", body: "0123456789"},
	)
	var peak, last int64
	if _, err := downloadDirTo(tarStream(t, body), 5, t.TempDir(), "proj", func(n int64) {
		last = n
		if n > peak {
			peak = n
		}
	}); err != nil {
		t.Fatal(err)
	}
	if peak != 5 || last != 5 {
		t.Fatalf("progress peaked at %d, ended at %d: want 5 and 5", peak, last)
	}
}

// -- the app's own: the flows, the one slot, the menus --

// An upload sends what the picker chose over the wire and lists the
// folder it landed in again, so the tree that shows the folder holds
// what the host holds, not what it held.
func TestUploadToRefreshesTree(t *testing.T) {
	f := newFilesHost(t)
	tree := &fileTree{pane: "p1", root: "/srv", dirs: map[string]*fileDir{}, isDir: map[string]bool{"/srv": true}}
	f.a.side.files.trees = map[string]*fileTree{"remote\x00/srv": tree}
	src := filepath.Join(t.TempDir(), "up.txt")
	if err := os.WriteFile(src, []byte("an upload's worth"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := fakeUpload(proto.Message{Result: proto.TransferUploadDone{Path: "/srv/up.txt"}})
	f.a.xferDial = func(h *host) (net.Conn, *bufio.Reader, error) { return transferDial(fake) }
	f.a.xferPick = func(dir, multiple bool) ([]string, error) {
		if dir || !multiple {
			t.Errorf("the picker asked dir=%v multiple=%v", dir, multiple)
		}
		return []string{src}, nil
	}

	f.a.uploadTo(f.h, "/srv")
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the transfer never ended")
		}
		if d := tree.dirs["/srv"]; d == nil || d.paths == nil {
			return errors.New("the folder did not list again")
		}
		if f.a.pendingToast != locales.FilesUploadDone(1, "remote") {
			return errors.New("toast " + f.a.pendingToast)
		}
		return nil
	})
	select {
	case got := <-fake.body:
		if string(got) != "an upload's worth" {
			t.Fatalf("body %q, want the file's", got)
		}
	default:
		t.Fatal("no body reached the stream")
	}
}

// A transfer under way takes no second: the menu's actions return at
// the gate, before a dialog opens or a stream dials, and say why.
func TestTransferBusyGate(t *testing.T) {
	f := newFilesHost(t)
	f.a.side.files.xfer = &transferState{kind: "download", name: "busy"}
	leaked := make(chan string, 2)
	f.a.xferDial = func(h *host) (net.Conn, *bufio.Reader, error) {
		leaked <- "dial"
		return nil, nil, errors.New("no dial")
	}
	f.a.xferSave = func(string) (string, error) { leaked <- "save"; return "", nil }
	f.a.xferPick = func(bool, bool) ([]string, error) { leaked <- "pick"; return nil, nil }

	f.a.downloadFrom(f.h, "/r/x.txt", false)
	f.a.uploadTo(f.h, "/r")
	select {
	case what := <-leaked:
		t.Fatalf("a busy gate let a second transfer start: the %s ran", what)
	case <-time.After(100 * time.Millisecond):
	}
	f.ui.Lock()
	toast, held := f.a.pendingToast, f.a.side.files.xfer
	f.ui.Unlock()
	if toast != locales.FilesTransferBusy() {
		t.Fatalf("toast %q, want the busy one", toast)
	}
	if held == nil || held.name != "busy" {
		t.Fatal("the gate took the transfer under way")
	}
}

// stallDialer is a kerod that sizes a megabyte and gives ten bytes of
// it, then holds the stream open: a body that never finishes, for a
// cancel to close.
type stallDialer struct {
	stall chan struct{}
}

func (d stallDialer) Dial() (net.Conn, error) {
	here, there := net.Pipe()
	go func() {
		defer there.Close()
		enc := json.NewEncoder(there)
		r := bufio.NewReader(there)
		if _, err := proto.ReadLine(r, 64<<10); err != nil {
			return // the Hello
		}
		if err := enc.Encode(proto.Welcome{Version: proto.Version, PID: 4242}); err != nil {
			return
		}
		if _, err := proto.ReadLine(r, proto.MaxLine); err != nil {
			return // the request
		}
		if err := enc.Encode(proto.Message{ID: 1, Result: proto.TransferHeader{Size: 1 << 20}}); err != nil {
			return
		}
		there.Write([]byte("ten bytes"))
		<-d.stall
	}()
	return here, nil
}

// The ✕ of a transfer under way ends it: the stream closes, the slot
// clears, and the toast says cancelled — not failed — though the body
// the header sized never finished coming, and nothing of it lands.
func TestTransferCancel(t *testing.T) {
	f := newFilesHost(t)
	dest := t.TempDir()
	f.a.xferSave = func(string) (string, error) { return filepath.Join(dest, "big.bin"), nil }
	// The far end: a welcome, a header sizing a megabyte, ten bytes of
	// it — then nothing, the stream held open until the cancel's close.
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	f.a.xferDial = func(h *host) (net.Conn, *bufio.Reader, error) {
		return transferDial(stallDialer{stall: stall})
	}

	f.a.downloadFrom(f.h, "/r/big.bin", false)
	var cancel func()
	f.waitsFor(t, func() error {
		if x := f.a.side.files.xfer; x != nil && x.cancel != nil {
			cancel = x.cancel
			return nil
		}
		return errors.New("no transfer under way to cancel")
	})
	f.ui.Lock()
	cancel()
	f.ui.Unlock()
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the slot never cleared")
		}
		if f.a.pendingToast != locales.FilesTransferCancelled() {
			return errors.New("toast " + f.a.pendingToast)
		}
		return nil
	})
	if entries, err := os.ReadDir(dest); err != nil || len(entries) != 0 {
		t.Fatalf("the cancelled download left %v behind (err %v)", entries, err)
	}
}

// The row's text is the bytes done of the total, "12.3 / 45.6 MB" — the
// bytes alone while the size is yet unknown.
func TestTransferLabel(t *testing.T) {
	if got := transferLabel(12_897_752, 47_815_066); got != "12.3 / 45.6 MB" {
		t.Errorf("transferLabel(12.9 MB, 45.6 MB) = %q, want %q", got, "12.3 / 45.6 MB")
	}
	if got := transferLabel(47_815_066, 0); got != "45.6 MB" {
		t.Errorf("transferLabel(45.6 MB, 0) = %q, want %q", got, "45.6 MB")
	}
	// A file that grew between stats shows no past its whole, the text
	// as clamped as the arc beside it.
	if got := transferLabel(47_815_066+1<<20, 47_815_066); got != "45.6 / 45.6 MB" {
		t.Errorf("transferLabel(past the total) = %q, want %q", got, "45.6 / 45.6 MB")
	}
	if got := transferLabel(1500, 900); got != "900 / 900 B" {
		t.Errorf("transferLabel(past the total, bytes) = %q, want %q", got, "900 / 900 B")
	}
}

// The menu's shape: a file's Download… past its Open With and before
// its Copy; a folder's Download… and Upload Here… before its Paste — a
// remote host's only, for this machine has its files already.
func TestFileMenuEntries(t *testing.T) {
	remote := &host{name: "r"}
	file := fileMenuEntries(remote, "x.txt", false)
	open, dl, cp := slices.Index(file, locales.FilesOpenVSCode()), slices.Index(file, locales.FilesDownload()), slices.Index(file, locales.FilesCopy())
	if open < 0 || cp < 0 || dl <= open || dl >= cp {
		t.Fatalf("a remote file's menu %v: Download… at %d", file, dl)
	}
	folder := fileMenuEntries(remote, "d", true)
	dl, up, paste := slices.Index(folder, locales.FilesDownload()), slices.Index(folder, locales.FilesUploadHere()), slices.Index(folder, locales.FilesPaste())
	if dl < 0 || up < 0 || paste < 0 || !(dl < up && up < paste) {
		t.Fatalf("a remote folder's menu %v: Download… %d, Upload Here… %d, Paste %d", folder, dl, up, paste)
	}
	if entries := fileMenuEntries(&host{}, "x.txt", false); slices.Contains(entries, locales.FilesDownload()) {
		t.Errorf("a local file's menu %v has Download…", entries)
	}
	if entries := fileMenuEntries(&host{}, "d", true); slices.Contains(entries, locales.FilesDownload()) || slices.Contains(entries, locales.FilesUploadHere()) {
		t.Errorf("a local folder's menu %v has a transfer", entries)
	}
}

// The transfer's row rides at the top of the Files view and the panel
// header carries its arc beside the folder: one state, two places, and
// the ✕ of either closes the stream.
func TestTransferRow(t *testing.T) {
	a := testApp(t)
	s := &a.st.Sessions[0]
	a.prefs.Side = sideFiles
	a.local().cl = &client.Client{}
	s.Panes[0].ProcessDir = "/tmp/demo"
	pane, root := a.filesRoot(s)
	tree := a.tree(pane, root)
	tree.dirs[root] = &fileDir{paths: []string{"/tmp/demo/main.go"}}
	closed := false
	a.side.files.xfer = &transferState{
		kind: xferDownload, name: "main.go", host: "remote",
		done: 12_897_752, total: 47_815_066,
		cancel: func() { closed = true },
	}
	tt := ui.NewTester(a.view, 1280, 720)
	tt.Frame()
	for _, want := range []string{"main.go", "12.3 / 45.6 MB"} {
		if !tt.HasText(want) {
			t.Fatalf("no %q of the transfer; texts %q", want, tt.Texts())
		}
	}
	if !tt.HasText(locales.FilesTransferBusy()) {
		t.Fatalf("no arc of the transfer in the header; texts %q", tt.Texts())
	}
	shot(t, tt, "files-transfer-row")
	// The ✕ of it closes the stream.
	if err := tt.Click(locales.FilesTransferCancelled()); err != nil {
		t.Fatalf("click the ✕: %v", err)
	}
	tt.Frame()
	if !closed {
		t.Error("the ✕ did not close the stream")
	}
	// A size yet unknown is a spinner still, and the bytes done alone.
	a.side.files.xfer.total = 0
	tt.Frame()
	if !tt.HasText("12.3 MB") {
		t.Fatalf("no count of the bytes alone; texts %q", tt.Texts())
	}
}
