package main

// The client of a transfer stream, which moves whole files between a
// host and this machine — a volume a control connection's JSON lines
// cannot carry. The stream opens as a control connection does, with a
// Hello of the transfer kind, and speaks the same lines after: one
// proto.Request names the download, its proto.Message reply carries the
// header, and the body the header sized follows raw. A body's file
// lands through a temp of its destination and is renamed whole, so a
// download that fails partway leaves the directory as it was.

import (
	"archive/tar"
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"kero/internal/client"
	"kero/internal/proto"

	"kero/locales"
)

// transferDial opens a transfer stream through d and answers its hello:
// the welcome is taken as a control connection's is — under a deadline,
// of the client's own version, its error the daemon's word for why not.
func transferDial(d client.Dialer) (net.Conn, *bufio.Reader, error) {
	conn, err := d.Dial()
	if err != nil {
		return nil, nil, err
	}
	if err := json.NewEncoder(conn).Encode(proto.Hello{Kind: proto.KindTransfer, Version: proto.Version}); err != nil {
		conn.Close()
		return nil, nil, err
	}
	br := bufio.NewReaderSize(conn, 64<<10)
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	line, err := proto.ReadLine(br, 64<<10)
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	var w proto.Welcome
	if err := json.Unmarshal(line, &w); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if w.Version != proto.Version {
		conn.Close()
		return nil, nil, client.ErrVersion
	}
	if w.Error != "" {
		conn.Close()
		return nil, nil, errors.New(w.Error)
	}
	return conn, br, nil
}

// transferRequest writes the stream's one request, a proto.Request line
// with its params riding it, as a control connection's Call writes its.
func transferRequest(w io.Writer, id int64, method string, params any) error {
	return json.NewEncoder(w).Encode(proto.Request{ID: id, Method: method, Params: params})
}

// transferReply reads the stream's next line as a reply: the daemon's
// word for why not — an older kerod, which knows no transfer kind,
// answers the method itself, so the client can tell that from a body —
// or the raw result, for the caller to take as its type.
func transferReply(br *bufio.Reader) (json.RawMessage, error) {
	line, err := proto.ReadLine(br, proto.MaxLine)
	if err != nil {
		return nil, err
	}
	var m struct {
		ID     int64           `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, err
	}
	if m.Error != "" {
		return nil, errors.New(m.Error)
	}
	return m.Result, nil
}

// downloadToFile takes the body a reply sized into destDir: size bytes
// to a temp of the directory, renamed to name once the last of them
// landed. progress hears the bytes as they come, cumulative; a body
// that ends short, or a write that fails, takes the temp with it, and
// the name it would have taken never knows it came.
func downloadToFile(br *bufio.Reader, size int64, destDir, name string, progress func(int64)) (string, error) {
	tmp, err := os.CreateTemp(destDir, ".kero-dl-*")
	if err != nil {
		return "", err
	}
	var total int64
	pw := progressWriter{w: tmp, fn: func(n int64) {
		total += n
		progress(total)
	}}
	// A 256KB chunk per copy step, so progress — and the update hop it
	// rides — is heard once a chunk, not once per the copy's 32KB
	// default. CopyBuffer alone would call a body that ends short a
	// clean end, so CopyN's word for it is kept by hand.
	n, err := io.CopyBuffer(pw, io.LimitReader(br, size), make([]byte, 256<<10))
	if err == nil && n < size {
		err = io.EOF
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == io.EOF {
		err = fmt.Errorf("transfer ended after %d of %d bytes", total, size)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	final := filepath.Join(destDir, name)
	if err := os.Rename(tmp.Name(), final); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return final, nil
}

// progressWriter is the writer a download paces through: every write
// tells fn how many bytes have landed in all, the number a progress bar
// fills by.
type progressWriter struct {
	w  io.Writer
	fn func(int64)
}

func (p progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if n > 0 {
		p.fn(int64(n))
	}
	return n, err
}

// uploadFile sends one file the stream's other way: a request that
// sizes it, a ready reply that takes it, the body raw, and a done reply
// that says where the daemon landed it — the dedup's name, not the one
// asked. The size and mode are the file's own, taken of the open file
// itself so the size the request promises is the size the body sends: a
// size that lied would leave the stream mid-body. A ready that answers
// with an error — a dir not there, an old kerod that knows no upload —
// comes back before a byte streams, so nothing goes toward a failure
// already decided.
func uploadFile(conn net.Conn, br *bufio.Reader, src, dir string, progress func(int64)) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	p := proto.TransferUploadParams{Dir: dir, Name: filepath.Base(src), Size: st.Size(), Mode: uint32(st.Mode().Perm())}
	// the id is the same every file: an upload stream speaks one file at
	// a time, in lockstep, so no reply needs telling from another
	if err := transferRequest(conn, 1, proto.MethodTransferUpload, p); err != nil {
		return "", err
	}
	if _, err := transferReply(br); err != nil { // the ready hop
		return "", err
	}
	var total int64
	pw := progressWriter{w: conn, fn: func(n int64) {
		total += n
		progress(total)
	}}
	// A 256KB chunk per copy step, as a download's, so progress rides
	// once a chunk. CopyBuffer alone would call a file that shrank
	// past its stat a clean end, so CopyN's word for it is kept by
	// hand.
	n, err := io.CopyBuffer(pw, io.LimitReader(f, st.Size()), make([]byte, 256<<10))
	if err == nil && n < st.Size() {
		err = io.EOF
	}
	if err != nil {
		return "", err
	}
	done, err := transferReply(br)
	if err != nil {
		return "", err
	}
	if done == nil {
		return "", errors.New("upload done reply carried no result")
	}
	var landed proto.TransferUploadDone
	if err := json.Unmarshal(done, &landed); err != nil {
		return "", err
	}
	return landed.Path, nil
}

// downloadDirTo untars the body a folder's download sized into
// destDir/name — its own name, or the "name copy" a paste takes beside
// one of its own — and returns the folder it filled. progress hears the
// body's bytes as they come, never more than the total the header
// sized, for the tar's own headers and padding ride the body beside the
// files that were counted. An entry that would not stay under the
// folder — a name that climbs out, a path that runs through a symlink —
// or a tar that ends before it is done, takes the whole folder with it:
// the directory as it was.
func downloadDirTo(br *bufio.Reader, total int64, destDir, name string, progress func(int64)) (string, error) {
	root := ""
	for n := 0; ; n++ {
		next := pasteName(name, n)
		if _, err := os.Lstat(filepath.Join(destDir, next)); err != nil {
			root = filepath.Join(destDir, next)
			break
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	done := false
	defer func() {
		if !done {
			os.RemoveAll(root)
		}
	}()
	var folders []extractFolder
	tr := tar.NewReader(&tarBody{r: br, total: total, fn: progress})
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		rel, err := safeTarPath(hdr.Name)
		if err != nil {
			return "", err
		}
		if tarThroughSymlink(root, rel) {
			return "", fmt.Errorf("tar entry %q escapes the download through a symlink", hdr.Name)
		}
		target := filepath.Join(root, filepath.FromSlash(rel))
		mtime := hdr.ModTime.Truncate(time.Second)
		switch hdr.Typeflag {
		case tar.TypeDir:
			// plain 0o755 until the extract ends, so a read-only tree
			// still extracts into itself; the mode it carried waits in
			// folders with its mtime
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
			folders = append(folders, extractFolder{path: target, mode: hdr.FileInfo().Mode().Perm(), mtime: mtime})
		case tar.TypeReg:
			dir := filepath.Dir(target)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
			tmp, err := os.CreateTemp(dir, ".kero-dl-*")
			if err != nil {
				return "", err
			}
			_, err = io.Copy(tmp, tr)
			if cerr := tmp.Close(); err == nil {
				err = cerr
			}
			if err == nil {
				err = os.Chmod(tmp.Name(), hdr.FileInfo().Mode().Perm())
			}
			if err == nil {
				err = os.Chtimes(tmp.Name(), mtime, mtime)
			}
			if err == nil {
				err = os.Rename(tmp.Name(), target)
			}
			if err != nil {
				os.Remove(tmp.Name())
				return "", err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			// only on a name nothing holds: a symlink over a path the tar
			// already gave — a folder it recorded for its mode and mtime,
			// the extract's own root — swaps that path for a door out, and
			// the daemon's walk never revisits a path anyway
			if _, err := os.Lstat(target); err == nil {
				return "", fmt.Errorf("tar entry %q is a symlink over an existing path", hdr.Name)
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return "", err
			}
		default:
			// a hard link, a device, a fifo: what a download of files and
			// folders cannot hold, and the tar walks past
		}
	}
	for _, f := range folders {
		// a folder's children set its mtime as they land, so the mode and
		// mtime it carried are given back once the last of them has. The
		// look first is depth on the fresh-name rule: what is no folder
		// now — a link, nothing — takes nothing through it.
		if st, err := os.Lstat(f.path); err != nil || !st.IsDir() {
			continue
		}
		os.Chmod(f.path, f.mode)
		os.Chtimes(f.path, f.mtime, f.mtime)
	}
	done = true
	return root, nil
}

// extractFolder is a folder of the tar, held to the extract's end for
// the mode and mtime it carried.
type extractFolder struct {
	path  string
	mode  os.FileMode
	mtime time.Time
}

// safeTarPath is the name a tar entry takes under the folder a download
// fills: cleaned, and refused when it would not stay there — absolute,
// or climbing out with a leading ... The daemon's names are
// slash-separated whatever OS it runs, so the test of them is the path
// package's, not the local filepath's.
func safeTarPath(name string) (string, error) {
	rel := path.Clean(name)
	if path.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("tar entry %q escapes the download", name)
	}
	return rel, nil
}

// tarThroughSymlink says whether an entry's path meets a symlink under
// the folder a download fills — running through one, to write where it
// points, or landing on one, for the extract's own chmod follows it and
// would set a mode and mtime outside. The folder is fresh, so every
// symlink a path can meet is one the tar itself made.
func tarThroughSymlink(root, rel string) bool {
	under := root
	for _, part := range strings.Split(rel, "/") {
		under = filepath.Join(under, part)
		if st, err := os.Lstat(under); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// tarBody is the reader a folder download's tar reads through: it tells
// progress of the body's bytes as they pass, never past the total the
// header sized.
type tarBody struct {
	r          io.Reader
	total      int64
	read, told int64
	fn         func(int64)
}

func (b *tarBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		b.read += int64(n)
		at := min(b.read, b.total)
		if at > b.told {
			b.told = at
			b.fn(at)
		}
	}
	return n, err
}

// -- the app's own: the flows, the one slot, the row --

// The kinds of transfer the one slot holds.
const (
	xferDownload = "download"
	xferUpload   = "upload"
)

// errXferCancelled marks a transfer the ✕ ended, so its error path says
// cancelled, not failed: the closed stream's own error is only the how.
var errXferCancelled = errors.New("cancelled")

// transferState is the one transfer the app runs at a time, as the
// Files view's row and the panel's header show it: what it is and
// where, and the bytes done of its total — 0 while the size is yet
// unknown, a spinner then. The slot itself, in filesState, is the busy
// gate every flow takes; err is the ✕'s mark on it. cancel, err and the
// fields a repaint reads belong to the UI thread alone: what runs on
// the transfer's goroutine goes through update.
type transferState struct {
	kind   string // xferDownload | xferUpload
	name   string // the file or folder under way
	host   string
	done   int64
	total  int64
	cancel func() // closes the stream; nil until one answers
	err    error  // errXferCancelled once the ✕ is taken
}

// downloadFrom runs the Download… flow of a file or folder of a remote
// host: a Save dialog (a file) or a folder picker (a folder) says where
// it lands, and a transfer stream of its own carries it. The claim on
// the one transfer is taken here, at once on the UI thread the menu ran
// on, so no second stream can start beside the first — a dialog walked
// away from gives the claim back, as every end does.
func (a *App) downloadFrom(h *host, path string, isDir bool) {
	if a.side.files.xfer != nil {
		a.toast(locales.FilesTransferBusy())
		return
	}
	name := proto.DirName(path)
	x := &transferState{kind: xferDownload, name: name, host: h.name}
	a.side.files.xfer = x
	a.toast(locales.FilesDownloadToast(name))
	go func() {
		// Where it lands: a file under the name the Save took, a folder
		// the picker chose — into which downloadDirTo takes the name it
		// needs, "name copy" beside one of its own.
		dest, file := "", name
		if isDir {
			dirs, err := a.xferPick(true, false)
			switch {
			case err != nil:
				a.xferFailed(x, err)
				return
			case len(dirs) == 0:
				a.update(func() { a.xferRelease(x) })
				return
			}
			dest = dirs[0]
		} else {
			p, err := a.xferSave(name)
			switch {
			case err != nil:
				a.xferFailed(x, err)
				return
			case p == "":
				a.update(func() { a.xferRelease(x) })
				return
			}
			dest, file = filepath.Dir(p), filepath.Base(p)
		}
		conn, br, err := a.xferDial(h)
		if err != nil {
			a.xferFailed(x, err)
			return
		}
		defer conn.Close()
		a.xferCancel(x, conn)
		if err := transferRequest(conn, 1, proto.MethodTransferDownload, proto.TransferDownloadParams{Path: path}); err != nil {
			a.xferFailed(x, err)
			return
		}
		res, err := transferReply(br)
		if err != nil {
			a.xferFailed(x, err)
			return
		}
		var hd proto.TransferHeader
		if err := json.Unmarshal(res, &hd); err != nil {
			a.xferFailed(x, err)
			return
		}
		a.update(func() { x.total = hd.Size })
		// The header says which it is, file or folder, and sizes the
		// body either way: the folder's total counts its files, its tar
		// rides the body beside them.
		progress := func(n int64) { a.update(func() { x.done = n }) }
		var landed string
		if hd.Dir {
			landed, err = downloadDirTo(br, hd.Size, dest, file, progress)
		} else {
			landed, err = downloadToFile(br, hd.Size, dest, file, progress)
		}
		if err != nil {
			a.xferFailed(x, err)
			return
		}
		a.update(func() {
			a.xferRelease(x)
			a.toast(locales.FilesDownloadDone(proto.DirName(landed)))
		})
	}()
}

// xferUp is one file of an upload: where it is here, the name it takes
// there, and its size, the share of the whole its progress fills.
type xferUp struct {
	path string
	base string
	size int64
}

// uploadTo runs the Upload Here… flow into a folder of a remote host: a
// picker of files says what goes — as many as it holds — and one
// transfer stream carries them, a file at a time in lockstep. The claim
// is as a download's, taken at once and given back by every end; the
// folder lists again when the last file lands.
func (a *App) uploadTo(h *host, dir string) {
	if a.side.files.xfer != nil {
		a.toast(locales.FilesTransferBusy())
		return
	}
	x := &transferState{kind: xferUpload, name: proto.DirName(dir), host: h.name}
	a.side.files.xfer = x
	a.toast(locales.FilesUploadToast(h.name))
	go func() {
		files, err := a.xferPick(false, true)
		switch {
		case err != nil:
			a.xferFailed(x, err)
			return
		case len(files) == 0:
			a.update(func() { a.xferRelease(x) })
			return
		}
		// What goes up, and the total it comes to, for the arc that
		// fills by it.
		var ups []xferUp
		var total int64
		for _, f := range files {
			st, err := os.Stat(f)
			if err != nil {
				a.xferFailed(x, err)
				return
			}
			ups = append(ups, xferUp{path: f, base: filepath.Base(f), size: st.Size()})
			total += st.Size()
		}
		conn, br, err := a.xferDial(h)
		if err != nil {
			a.xferFailed(x, err)
			return
		}
		defer conn.Close()
		a.xferCancel(x, conn)
		a.update(func() { x.total = total })
		// The files go one at a time, each's progress filling past the
		// ones before it; at is that base, the goroutine's own.
		var paths []string
		var sent int64
		for _, u := range ups {
			base, at := u.base, sent
			a.update(func() { x.name, x.done = base, at })
			progress := func(n int64) {
				n += at
				a.update(func() { x.done = n })
			}
			landed, err := uploadFile(conn, br, u.path, dir, progress)
			if err != nil {
				a.xferFailed(x, err)
				return
			}
			paths = append(paths, landed)
			sent += u.size
		}
		a.update(func() {
			a.xferRelease(x)
			a.toast(locales.FilesUploadDone(len(paths), h.name))
			// The folder that took them lists again, in whatever tree
			// shows it, as a paste's landing does.
			if t := a.xferTreeOf(h, dir); t != nil {
				a.fetchDir(t, dir)
			}
		})
	}()
}

// xferCancel arms a transfer's ✕ once its stream exists: the close of
// the conn is the cancel, marked so the end reads cancelled, not
// failed. Published through update, for the UI thread the button reads
// it on.
func (a *App) xferCancel(x *transferState, conn net.Conn) {
	a.update(func() {
		x.cancel = func() {
			x.err = errXferCancelled
			conn.Close()
		}
	})
}

// xferFailed ends a transfer that did not land, on the UI thread. The
// ✕'s mark on it says cancelled; any other error says failed, as the
// host's own word — mapped where it is one an older kerod gives, whose
// unknown method reads as the advice to update it, not the raw text.
func (a *App) xferFailed(x *transferState, err error) {
	a.update(func() {
		if x.err != nil {
			a.toast(locales.FilesTransferCancelled())
		} else {
			a.toast(locales.FilesTransferFailed(remoteError(err)))
		}
		a.xferRelease(x)
	})
}

// xferRelease gives the one transfer's claim back, on the UI thread:
// every end of a transfer lands here — the landing, the failure, the
// cancel, the dialog walked away from.
func (a *App) xferRelease(x *transferState) {
	if a.side.files.xfer == x {
		a.side.files.xfer = nil
	}
}

// xferTreeOf is the tree that shows a folder of a host — the folder as
// its root, or one open under the tree's — for the listing a transfer's
// landing asks of it again; nil when none does, for the folder an
// upload filled may be nowhere on screen.
func (a *App) xferTreeOf(h *host, dir string) *fileTree {
	for _, t := range a.side.files.trees {
		if a.hostOf(t.pane) == h && (t.root == dir || t.isDir[dir]) {
			return t
		}
	}
	return nil
}

// defaultXferDial opens a transfer stream as the app does: through the
// host's own dialer.
func defaultXferDial(h *host) (net.Conn, *bufio.Reader, error) {
	return transferDial(h.dialer())
}

// defaultXferSave asks where a file download lands, the file's own name
// to hand; "" is the dialog walked away from.
func defaultXferSave(defaultPath string) (string, error) {
	return mygo.Dialog.Save(mygo.SaveDialogOptions{DefaultPath: defaultPath, CreateDirectories: true})
}

// defaultXferPick asks what the user picked: a folder, or files as many
// as multiple allows; nil is the dialog walked away from.
func defaultXferPick(dir, multiple bool) ([]string, error) {
	return mygo.Dialog.Open(mygo.OpenDialogOptions{Directory: dir, Multiple: multiple, CreateDirectories: dir})
}

// transferLabel is the row's word of how far a transfer is: the bytes
// done of the total, both in the total's measure — "12.3 / 45.6 MB" —
// the bytes alone while the size is yet unknown.
func transferLabel(done, total int64) string {
	if total <= 0 {
		return xferBytes(done)
	}
	// Clamped as the arc clamps: a file that grew between stats cannot
	// show past its whole, "50.1 / 50.0 MB".
	done = min(done, total)
	d, f := float64(done), float64(total)
	unit := "B"
	for _, u := range []string{"KB", "MB", "GB", "TB"} {
		if f < 1024 {
			break
		}
		d, f = d/1024, f/1024
		unit = u
	}
	if unit == "B" {
		return strconv.FormatInt(done, 10) + " / " + strconv.FormatInt(total, 10) + " B"
	}
	return strconv.FormatFloat(d, 'f', 1, 64) + " / " + strconv.FormatFloat(f, 'f', 1, 64) + " " + unit
}

// xferBytes writes a count of bytes with its unit: whole bytes under a
// kilobyte, one digit past it, as a progress row reads.
func xferBytes(n int64) string {
	f, unit := float64(n), "B"
	for _, u := range []string{"KB", "MB", "GB", "TB"} {
		if f < 1024 {
			break
		}
		f /= 1024
		unit = u
	}
	if unit == "B" {
		return strconv.FormatInt(n, 10) + " B"
	}
	return strconv.FormatFloat(f, 'f', 1, 64) + " " + unit
}

// xferRow is the transfer's row at the top of the Files view: the arc
// of its progress, its name and how far, and the ✕ that ends it.
// Nothing while none runs.
func (a *App) xferRow(c *ui.Context, pal *palette) {
	x := a.side.files.xfer
	if x == nil {
		return
	}
	ui.Row(c.Key("xfer")).Shrink(0).Padding(6, 8, 4, 8).Gap(8).AlignItems(ui.Center).Children(func() {
		xferArc(c, pal, x, 15)
		ui.Text(c, x.name).FontSize(12.5).FontWeight(600).TextColor(pal.text).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
		ui.Text(c, transferLabel(x.done, x.total)).FontSize(12).FontFeatures("tnum").TextColor(pal.textMuted).SingleLine().Shrink(0)
		xferStop(c, pal, "xfer-stop", x)
	})
}

// xferHeader is the panel header's word of a transfer under way, beside
// the folder the panel shows: the arc and the ✕ alone, for the Changes
// tab and other trees have no row to carry them. Nothing while none
// runs.
func (a *App) xferHeader(c *ui.Context, pal *palette) {
	x := a.side.files.xfer
	if x == nil {
		return
	}
	xferArc(c, pal, x, 13)
	xferStop(c, pal, "xfer-stop-hdr", x)
}

// xferStop is the ✕ of a transfer: it closes the stream, which the
// transfer takes as its end. Nothing before the stream exists — the
// dialogs that lead to one have nothing to close.
func xferStop(c *ui.Context, pal *palette, key string, x *transferState) {
	if x.cancel == nil {
		return
	}
	b := ui.ButtonBase(c.Key(key)).Size(22, 22).Radius(6).Center().TextColor(pal.textMuted).
		Label(locales.FilesTransferCancelled()).Tooltip(locales.FilesTransferCancelled())
	if b.Hovered() || b.Pressed() {
		b.Background(pal.rowHover).TextColor(pal.text)
	}
	b.Children(func() { ui.Icon(c, closeIcon).Size(12, 12) })
	if b.Clicked() {
		x.cancel()
	}
}

// xferArc is a transfer's progress as the sidebar's status ring is: a
// track with an arc filling by the bytes done of the total — while the
// size is yet unknown, a ring turning, as a working session's does.
func xferArc(c *ui.Context, pal *palette, x *transferState, size float32) {
	box := ui.Box(c).Size(size, size).Shrink(0).Label(locales.FilesTransferBusy())
	box.Draw(func(p *ui.Painter, r ui.Rect) {
		cx, cy := r.X+r.W/2, r.Y+r.H/2
		rad := min(r.W, r.H)/2 - 1
		var track ui.Path
		track.Circle(cx, cy, rad)
		p.StrokePath(&track, 1.7, pal.text.Alpha(0.13))
		switch {
		case x.total <= 0:
			p.AnimationFrame()
			deg := float32(p.Now().UnixMilli()%1000) / 1000 * 360
			p.Transform(ui.Rotation(deg), cx, cy, func() {
				var arc ui.Path
				arc.Circle(cx, cy, rad)
				p.StrokePath(arc.Trim(0, 0.3), 1.7, pal.text)
			})
		default:
			// From the top, clockwise: the circle starts at 3 o'clock.
			// Clamped, for a total the files grew past would draw past
			// the ring's end.
			frac := min(1, float32(x.done)/float32(x.total))
			p.Transform(ui.Rotation(-90), cx, cy, func() {
				var arc ui.Path
				arc.Circle(cx, cy, rad)
				p.StrokePath(arc.Trim(0, max(0.02, frac)), 1.7, pal.text)
			})
		}
	})
}
