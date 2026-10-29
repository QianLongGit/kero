package daemon

import (
	"archive/tar"
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"kero/internal/proto"
)

// transferDaemon is a testDaemon a connection can be served on: its client
// list starts empty, as serveHello writes to it.
func transferDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := testDaemon(t)
	d.clients = map[*proto.ClientInfo]struct{}{}
	return d
}

// transferConn dials d as a client after files does and speaks its Hello
// of the given kind, leaving the stream's conn to write to and reader to
// hear the replies and bodies on.
func transferConn(t *testing.T, d *Daemon, kind string) (net.Conn, *bufio.Reader) {
	t.Helper()
	here, there := net.Pipe()
	go d.serve(there, identity{peer: "uid 501", via: "unix"})
	t.Cleanup(func() { here.Close() })
	writeJSON(t, here, proto.Hello{Kind: kind, Version: proto.Version})
	r := bufio.NewReader(here)
	var welcome proto.Welcome
	readJSON(t, r, &welcome)
	if welcome.Error != "" {
		t.Fatalf("welcome: %s", welcome.Error)
	}
	return here, r
}

// writeJSON writes v as the stream's next line.
func writeJSON(t *testing.T, conn net.Conn, v any) {
	t.Helper()
	line, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
}

// readJSON reads the stream's next line into v.
func readJSON(t *testing.T, r *bufio.Reader, v any) {
	t.Helper()
	line, err := proto.ReadLine(r, proto.MaxLine)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(line, v); err != nil {
		t.Fatalf("line %s: %v", line, err)
	}
}

// resultAs takes a reply's Result as the type the method answers with.
func resultAs[T any](t *testing.T, m proto.Message) T {
	t.Helper()
	b, err := json.Marshal(m.Result)
	if err != nil {
		t.Fatal(err)
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("result %v: %v", m.Result, err)
	}
	return v
}

// A download streams a file whole, which a control connection's JSON lines
// cannot carry: a header says its size and mode, the bytes follow raw.
func TestTransferDownload(t *testing.T) {
	d := transferDaemon(t)
	name := filepath.Join(t.TempDir(), "file.txt")
	body := "a file's worth of bytes"
	if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	conn, r := transferConn(t, d, proto.KindTransfer)
	writeJSON(t, conn, proto.Request{ID: 7, Method: proto.MethodTransferDownload, Params: proto.TransferDownloadParams{Path: name}})
	var m proto.Message
	readJSON(t, r, &m)
	if m.ID != 7 || m.Error != "" {
		t.Fatalf("reply %+v", m)
	}
	h := resultAs[proto.TransferHeader](t, m)
	if h.Dir || h.Size != int64(len(body)) || h.Mode != 0o644 {
		t.Fatalf("header %+v", h)
	}
	got := make([]byte, h.Size)
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("body %q", got)
	}
}

// A path that is not there answers why, as a control method's error does.
func TestTransferDownloadMissing(t *testing.T) {
	d := transferDaemon(t)
	conn, r := transferConn(t, d, proto.KindTransfer)
	writeJSON(t, conn, proto.Request{ID: 1, Method: proto.MethodTransferDownload,
		Params: proto.TransferDownloadParams{Path: filepath.Join(t.TempDir(), "nope")}})
	var m proto.Message
	readJSON(t, r, &m)
	if m.ID != 1 || !strings.Contains(m.Error, "no such file") {
		t.Fatalf("reply %+v", m)
	}
}

// A transfer takes the paths the fs methods do: ~ is the home — the real
// HOME, not the test's KERO_HOME — and reaches the same file as absolute.
func TestTransferDownloadHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	d := transferDaemon(t)
	body := "from the home"
	if err := os.WriteFile(filepath.Join(home, "x"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"~/x", filepath.Join(home, "x")} {
		conn, r := transferConn(t, d, proto.KindTransfer)
		writeJSON(t, conn, proto.Request{ID: 2, Method: proto.MethodTransferDownload, Params: proto.TransferDownloadParams{Path: p}})
		var m proto.Message
		readJSON(t, r, &m)
		if m.Error != "" {
			t.Fatalf("%s: reply %+v", p, m)
		}
		h := resultAs[proto.TransferHeader](t, m)
		if h.Size != int64(len(body)) {
			t.Fatalf("%s: header %+v", p, h)
		}
		got := make([]byte, h.Size)
		if _, err := io.ReadFull(r, got); err != nil {
			t.Fatal(err)
		}
		if string(got) != body {
			t.Fatalf("%s: body %q", p, got)
		}
	}
}

// An older kerod, which knows no transfer kind, takes the stream as a
// control connection and answers the method it has not, so a client can
// tell the two apart rather than wait for a body that never comes.
func TestTransferOldDaemon(t *testing.T) {
	d := transferDaemon(t)
	conn, r := transferConn(t, d, "control")
	writeJSON(t, conn, proto.Request{ID: 3, Method: proto.MethodTransferDownload, Params: proto.TransferDownloadParams{Path: "~/x"}})
	var m proto.Message
	readJSON(t, r, &m)
	if m.ID != 3 || !strings.Contains(m.Error, "unknown method") {
		t.Fatalf("reply %+v", m)
	}
}

// An empty file is a body of its own — a header of no bytes, no body to
// read — and the stream then ends cleanly, so a taker of the body waits on
// nothing.
func TestTransferDownloadEmpty(t *testing.T) {
	d := transferDaemon(t)
	name := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(name, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	conn, r := transferConn(t, d, proto.KindTransfer)
	writeJSON(t, conn, proto.Request{ID: 4, Method: proto.MethodTransferDownload, Params: proto.TransferDownloadParams{Path: name}})
	var m proto.Message
	readJSON(t, r, &m)
	if m.Error != "" {
		t.Fatalf("reply %+v", m)
	}
	h := resultAs[proto.TransferHeader](t, m)
	if h.Size != 0 {
		t.Fatalf("header %+v", h)
	}
	if _, err := r.ReadByte(); err != io.EOF {
		t.Fatalf("after the body: %v", err)
	}
}

// A folder downloads as a tar of its tree: a header of the regular files'
// count and total bytes for the client to pace progress against, then
// every entry named relative to the root — dirs, files, a symlink with
// its target, each carrying its lstat mode and mtime. A .git is left
// behind, as the view leaves it hidden.
func TestTransferDownloadDir(t *testing.T) {
	d := transferDaemon(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "top.txt"), []byte("top level"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "nested.txt"), []byte("nested level"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "deep", "leaf.txt"), []byte("deep level"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("top.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a .git at depth, not only the root's own
	if err := os.Mkdir(filepath.Join(root, "sub", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", ".git", "config"), []byte("[core]"), 0o644); err != nil {
		t.Fatal(err)
	}
	conn, r := transferConn(t, d, proto.KindTransfer)
	writeJSON(t, conn, proto.Request{ID: 8, Method: proto.MethodTransferDownload, Params: proto.TransferDownloadParams{Path: root}})
	var m proto.Message
	readJSON(t, r, &m)
	if m.ID != 8 || m.Error != "" {
		t.Fatalf("reply %+v", m)
	}
	h := resultAs[proto.TransferHeader](t, m)
	if !h.Dir || h.Count != 3 || h.Size != int64(len("top level")+len("nested level")+len("deep level")) {
		t.Fatalf("header %+v", h)
	}
	got, bodies := map[string]*tar.Header{}, map[string]string{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("%s: %v", hdr.Name, err)
			}
			bodies[hdr.Name] = string(b)
		}
		got[hdr.Name] = hdr
	}
	// the tree's own root is "."; .git and what is in it never stream
	names := []string{".", "empty", "link", "sub", "sub/deep", "sub/deep/leaf.txt", "sub/nested.txt", "top.txt"}
	for _, name := range names {
		if got[name] == nil {
			t.Errorf("no entry %s", name)
		}
	}
	if len(got) != len(names) {
		have := make([]string, 0, len(got))
		for name := range got {
			have = append(have, name)
		}
		slices.Sort(have)
		t.Fatalf("entries %v", have)
	}
	if hdr := got["link"]; hdr.Typeflag != tar.TypeSymlink || hdr.Linkname != "top.txt" {
		t.Errorf("link %+v", hdr)
	}
	for name, body := range map[string]string{"top.txt": "top level", "sub/nested.txt": "nested level", "sub/deep/leaf.txt": "deep level"} {
		if bodies[name] != body {
			t.Errorf("%s: body %q", name, bodies[name])
		}
		if got[name].Size != int64(len(body)) {
			t.Errorf("%s: size %d", name, got[name].Size)
		}
	}
	// modes are the lstat ones, so a symlink is its link, not its target;
	// mtimes carry as far as a tar's default format takes them — the
	// writer rounds to the second rather than grow every header a
	// kilobyte of PAX
	for _, name := range names {
		st, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		hdr := got[name]
		if hdr.Mode != int64(st.Mode().Perm()) {
			t.Errorf("%s: mode %o, want %o", name, hdr.Mode, st.Mode().Perm())
		}
		if want := st.ModTime().Round(time.Second); !hdr.ModTime.Equal(want) {
			t.Errorf("%s: mtime %v, want %v", name, hdr.ModTime, want)
		}
	}
	if _, err := r.ReadByte(); err != io.EOF {
		t.Fatalf("after the tar: %v", err)
	}
}

// A symlink a folder download names stands for the folder it points at —
// the view lists such a link among folders — so the tar walks the tree
// the stat resolved into, not the link alone.
func TestTransferDownloadDirSymlinkRoot(t *testing.T) {
	d := transferDaemon(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("in the tree"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	conn, r := transferConn(t, d, proto.KindTransfer)
	writeJSON(t, conn, proto.Request{ID: 10, Method: proto.MethodTransferDownload, Params: proto.TransferDownloadParams{Path: link}})
	var m proto.Message
	readJSON(t, r, &m)
	if m.ID != 10 || m.Error != "" {
		t.Fatalf("reply %+v", m)
	}
	h := resultAs[proto.TransferHeader](t, m)
	if !h.Dir || h.Count != 1 || h.Size != int64(len("in the tree")) {
		t.Fatalf("header %+v", h)
	}
	var names []string
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
	if !slices.Equal(names, []string{".", "f"}) {
		t.Fatalf("entries %v", names)
	}
}

// A tree the walk cannot read is an answer, not a body: the error goes
// back before a tar byte streams, so a client never mistakes a failure
// for a short download. Root and Windows pass permission bits by, so the
// walk does not fail there.
func TestTransferDownloadDirWalkError(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits stop neither root nor Windows")
	}
	d := transferDaemon(t)
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "x"), []byte("hidden"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	conn, r := transferConn(t, d, proto.KindTransfer)
	writeJSON(t, conn, proto.Request{ID: 9, Method: proto.MethodTransferDownload, Params: proto.TransferDownloadParams{Path: root}})
	var m proto.Message
	readJSON(t, r, &m)
	if m.ID != 9 || !strings.Contains(m.Error, "permission denied") {
		t.Fatalf("reply %+v", m)
	}
	if _, err := r.ReadByte(); err != io.EOF {
		t.Fatalf("after the error: %v", err)
	}
}

// uploadFile speaks an upload whole — the request, the ready hop, the
// body, the done — and says where the body landed.
func uploadFile(t *testing.T, conn net.Conn, r *bufio.Reader, id int64, p proto.TransferUploadParams, body string) string {
	t.Helper()
	p.Size = int64(len(body))
	writeJSON(t, conn, proto.Request{ID: id, Method: proto.MethodTransferUpload, Params: p})
	var m proto.Message
	readJSON(t, r, &m)
	if m.ID != id || m.Error != "" {
		t.Fatalf("ready %+v", m)
	}
	if h := resultAs[proto.TransferHeader](t, m); h != (proto.TransferHeader{}) {
		t.Fatalf("ready %+v", h)
	}
	// A body of no bytes is no write: a net.Pipe holds a zero-length one
	// for a read that never comes, where a real client simply sends
	// nothing.
	if len(body) > 0 {
		if _, err := conn.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	readJSON(t, r, &m)
	if m.ID != id || m.Error != "" {
		t.Fatalf("done %+v", m)
	}
	return resultAs[proto.TransferUploadDone](t, m).Path
}

// waitServed waits until d serves no connection: a stream whose client
// hung up mid-body takes its temp file with it only once its goroutine
// runs to its end, and the dir speaks final only then.
func waitServed(t *testing.T, d *Daemon) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		n := d.conns
		d.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a connection still served")
		}
		time.Sleep(time.Millisecond)
	}
}

// An upload writes the body its ready hop takes: the bytes land at the
// dir and name the params say, with the mode they carry, and the answer
// says where. The stream then serves the next file, as a control
// connection serves the next call.
func TestTransferUpload(t *testing.T) {
	d := transferDaemon(t)
	dir := t.TempDir()
	conn, r := transferConn(t, d, proto.KindTransfer)
	if got := uploadFile(t, conn, r, 11, proto.TransferUploadParams{Dir: dir, Name: "up.txt", Mode: 0o640}, "an upload's worth of bytes"); got != filepath.Join(dir, "up.txt") {
		t.Fatalf("path %s", got)
	}
	body, err := os.ReadFile(filepath.Join(dir, "up.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "an upload's worth of bytes" {
		t.Fatalf("body %q", body)
	}
	st, err := os.Stat(filepath.Join(dir, "up.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	uploadFile(t, conn, r, 16, proto.TransferUploadParams{Dir: dir, Name: "next.txt", Mode: 0o644}, "the next file on the same stream")
	if _, err := os.Stat(filepath.Join(dir, "next.txt")); err != nil {
		t.Fatal(err)
	}
	// A mode with a setuid bit riding it lands as its permission bits
	// alone: the chmod masks, so a client's word sets no bit of ours.
	uploadFile(t, conn, r, 21, proto.TransferUploadParams{Dir: dir, Name: "setuid.txt", Mode: 0o4755}, "a bit riding it")
	if st, err := os.Stat(filepath.Join(dir, "setuid.txt")); err != nil || st.Mode().Perm() != 0o755 || st.Mode()&os.ModeSetuid != 0 {
		t.Fatalf("mode %v (err %v), want 0755 alone", st.Mode(), err)
	}
}

// An upload beside a file of its own name takes the next free copy of
// it, as the app's paste does: "note copy.txt" past a "note.txt", then
// "note copy 2.txt" past them both — and the first keeps its own bytes.
func TestTransferUploadCollision(t *testing.T) {
	d := transferDaemon(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	conn, r := transferConn(t, d, proto.KindTransfer)
	got := uploadFile(t, conn, r, 17, proto.TransferUploadParams{Dir: dir, Name: "note.txt", Mode: 0o644}, "second")
	if want := filepath.Join(dir, "note copy.txt"); got != want {
		t.Fatalf("path %s, want %s", got, want)
	}
	got = uploadFile(t, conn, r, 18, proto.TransferUploadParams{Dir: dir, Name: "note.txt", Mode: 0o644}, "third")
	if want := filepath.Join(dir, "note copy 2.txt"); got != want {
		t.Fatalf("path %s, want %s", got, want)
	}
	for name, want := range map[string]string{"note.txt": "first", "note copy.txt": "second", "note copy 2.txt": "third"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(body) != want {
			t.Fatalf("%s: body %q, %v", name, body, err)
		}
	}
}

// A dir that is not there, or a name with a path in it, is answered
// before any ready, so the client never sends the body. The stream stays
// in step: an upload after the errors lands.
func TestTransferUploadBadDir(t *testing.T) {
	d := transferDaemon(t)
	conn, r := transferConn(t, d, proto.KindTransfer)
	writeJSON(t, conn, proto.Request{ID: 12, Method: proto.MethodTransferUpload,
		Params: proto.TransferUploadParams{Dir: filepath.Join(t.TempDir(), "nope"), Name: "x.txt", Size: 3, Mode: 0o644}})
	var m proto.Message
	readJSON(t, r, &m)
	if m.ID != 12 || m.Error == "" || m.Result != nil {
		t.Fatalf("reply %+v", m)
	}
	writeJSON(t, conn, proto.Request{ID: 13, Method: proto.MethodTransferUpload,
		Params: proto.TransferUploadParams{Dir: t.TempDir(), Name: "sub/x.txt", Size: 3, Mode: 0o644}})
	readJSON(t, r, &m)
	if m.ID != 13 || m.Error == "" || m.Result != nil {
		t.Fatalf("reply %+v", m)
	}
	dir := t.TempDir()
	if got := uploadFile(t, conn, r, 14, proto.TransferUploadParams{Dir: dir, Name: "x.txt", Mode: 0o644}, "yes"); got != filepath.Join(dir, "x.txt") {
		t.Fatalf("path %s", got)
	}
}

// A body that ends short leaves nothing behind: no file of the half that
// came, no temp of it either — the dir as it was. The client hung up
// mid-body, so the stream ends with it.
func TestTransferUploadTruncated(t *testing.T) {
	d := transferDaemon(t)
	dir := t.TempDir()
	conn, r := transferConn(t, d, proto.KindTransfer)
	writeJSON(t, conn, proto.Request{ID: 15, Method: proto.MethodTransferUpload,
		Params: proto.TransferUploadParams{Dir: dir, Name: "half.txt", Size: 10, Mode: 0o644}})
	var m proto.Message
	readJSON(t, r, &m)
	if m.Error != "" {
		t.Fatalf("ready %+v", m)
	}
	if _, err := conn.Write([]byte("half")); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	waitServed(t, d)
	entries, err := os.ReadDir(dir)
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

// An empty upload is a body of no bytes: the ready goes out, no byte
// follows, and the file lands empty — not missing.
func TestTransferUploadEmpty(t *testing.T) {
	d := transferDaemon(t)
	dir := t.TempDir()
	conn, r := transferConn(t, d, proto.KindTransfer)
	if got := uploadFile(t, conn, r, 19, proto.TransferUploadParams{Dir: dir, Name: "empty", Mode: 0o644}, ""); got != filepath.Join(dir, "empty") {
		t.Fatalf("path %s", got)
	}
	body, err := os.ReadFile(filepath.Join(dir, "empty"))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Fatalf("body %q", body)
	}
}
