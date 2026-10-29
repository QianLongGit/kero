package daemon

import (
	"archive/tar"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"kero/internal/build"
	"kero/internal/proto"
)

// serveTransfer serves a transfer connection, which moves whole files a
// control connection's JSON lines cannot carry. It answers the Hello with
// the same Welcome a control connection gets, then serves one Request a
// file, the body raw on the conn the JSON rides. A download is the
// stream's last word — the client closes once it has the file — so it
// returns after the body rather than waiting for a next line; an upload
// answers twice, ready for the body then done with where it landed, and
// the loop goes on for the client's next file; a method this kerod does
// not know is answered as a control connection's is, and the loop goes
// on for what the client says next.
func (d *Daemon) serveTransfer(conn net.Conn, r *bufio.Reader, id identity) {
	welcome := proto.Welcome{Version: proto.Version, PID: os.Getpid(), Started: d.started.UnixMilli(), App: build.Version(), Build: build.Daemon}
	enc := json.NewEncoder(conn)
	if err := enc.Encode(welcome); err != nil {
		return
	}
	for {
		line, err := proto.ReadLine(r, proto.MaxLine)
		if err != nil {
			return
		}
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &req) != nil {
			continue
		}
		switch req.Method {
		case proto.MethodTransferDownload:
			var p proto.TransferDownloadParams
			if len(req.Params) > 0 {
				if err := json.Unmarshal(req.Params, &p); err != nil {
					enc.Encode(proto.Message{ID: req.ID, Error: err.Error()})
					return
				}
			}
			transferDownload(conn, enc, req.ID, p)
			return
		case proto.MethodTransferUpload:
			var p proto.TransferUploadParams
			if len(req.Params) > 0 {
				if err := json.Unmarshal(req.Params, &p); err != nil {
					enc.Encode(proto.Message{ID: req.ID, Error: err.Error()})
					continue
				}
			}
			if transferUpload(enc, r, req.ID, p) != nil {
				return
			}
		default:
			enc.Encode(proto.Message{ID: req.ID, Error: fmt.Sprintf("unknown method %q", req.Method)})
		}
	}
}

// transferDownload streams the path a download names: a file as a header
// of its size and mode, then its bytes; a directory as a header of what
// its tree holds, then a tar of it.
func transferDownload(conn net.Conn, enc *json.Encoder, id int64, p proto.TransferDownloadParams) {
	path := hostPath(p.Path, "")
	st, err := os.Stat(path)
	if err != nil {
		enc.Encode(proto.Message{ID: id, Error: err.Error()})
		return
	}
	if st.IsDir() {
		transferDir(conn, enc, id, path)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		enc.Encode(proto.Message{ID: id, Error: err.Error()})
		return
	}
	defer f.Close()
	if err := enc.Encode(proto.Message{ID: id, Result: proto.TransferHeader{Size: st.Size(), Mode: uint32(st.Mode().Perm())}}); err != nil {
		return
	}
	io.Copy(conn, f)
}

// transferDir answers a download of a directory: a header of the regular
// files' count and total size for the client's progress, then a tar of
// the tree, entry names relative to the root. The walk runs twice: once
// to size the body — every failure answered before a byte streams, so a
// client never mistakes an error for a short body — and once writing,
// where a failure can only end the stream.
func transferDir(conn net.Conn, enc *json.Encoder, id int64, root string) {
	// The stat resolved a symlink into the directory it names; the walk
	// would not, and a tar of the link alone is no download of a folder.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		enc.Encode(proto.Message{ID: id, Error: err.Error()})
		return
	}
	size, count, err := transferTotals(root)
	if err != nil {
		enc.Encode(proto.Message{ID: id, Error: err.Error()})
		return
	}
	if err := enc.Encode(proto.Message{ID: id, Result: proto.TransferHeader{Dir: true, Size: size, Count: count}}); err != nil {
		return
	}
	tw := tar.NewWriter(conn)
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		name := filepath.ToSlash(rel)
		if skippedByDownload(name) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		return err
	})
	if walkErr != nil {
		// Half a tar, no footer: the client's reader meets an early end
		// and calls the download what it is, incomplete.
		return
	}
	tw.Close()
}

// transferTotals walks a tree as the tar that follows does and totals
// the regular files a body will carry — a symlink is its link, not its
// target's bytes.
func transferTotals(root string) (int64, int, error) {
	var size int64
	var count int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if skippedByDownload(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		size += info.Size()
		count++
		return nil
	})
	return size, count, err
}

// skippedByDownload says what a download's walks pass over: a .git at
// any depth, as the view hides it too. The walk prunes a skipped
// directory's children, so an entry's own name is all there is to test.
func skippedByDownload(name string) bool {
	return path.Base(name) == ".git"
}

// transferUpload writes the body a client sends once its ready hop
// answers: exactly Size bytes of it into a temp file of Dir, chmod'd to
// the params' mode and renamed to the first free name beside Name — a
// file lands whole or not at all, as fsWrite lands an edit. What cannot
// take the body is answered before the ready, so a client never sends
// what would be lost; past the ready a failure ends the stream, for what
// follows a short body is no request line. The body reads through the
// stream's own reader — the request line and the body's first bytes
// share its buffer.
func transferUpload(enc *json.Encoder, r *bufio.Reader, id int64, p proto.TransferUploadParams) error {
	dir := hostPath(p.Dir, "")
	st, err := os.Stat(dir)
	switch {
	case err != nil:
	case st.IsDir():
	default:
		err = fmt.Errorf("%s is no directory", p.Dir)
	}
	if err == nil && (p.Name == "" || filepath.Base(p.Name) != p.Name) {
		err = fmt.Errorf("%q is no file name", p.Name)
	}
	if err != nil {
		enc.Encode(proto.Message{ID: id, Error: err.Error()})
		return nil
	}
	if err := enc.Encode(proto.Message{ID: id, Result: proto.TransferHeader{}}); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".kero-*")
	if err != nil {
		enc.Encode(proto.Message{ID: id, Error: err.Error()})
		return err
	}
	name := tmp.Name()
	_, err = io.CopyN(tmp, r, p.Size)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, os.FileMode(p.Mode).Perm())
	}
	final := filepath.Join(dir, dedupName(dir, p.Name))
	if err == nil {
		err = os.Rename(name, final)
	}
	if err != nil {
		// Half a body, or no place it could land: no file of it, and the
		// stream ends with it — the client that fell silent mid-body has
		// nothing more to say.
		os.Remove(name)
		enc.Encode(proto.Message{ID: id, Error: err.Error()})
		return err
	}
	enc.Encode(proto.Message{ID: id, Result: proto.TransferUploadDone{Path: final}})
	return nil
}

// dedupName is the name an upload takes beside what its dir already
// holds: its own first, then "x copy.ext", then "x copy 2.ext", … as the
// app's paste names a file pasted beside its own — the ext splits at the
// last dot, which a leading dot is not (".profile" has none to split).
func dedupName(dir, name string) string {
	stem, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		stem, ext = name[:i], name[i:]
	}
	for n := 0; ; n++ {
		next := name
		switch n {
		case 0:
		case 1:
			next = stem + " copy" + ext
		default:
			next = stem + " copy " + strconv.Itoa(n) + ext
		}
		if _, err := os.Stat(filepath.Join(dir, next)); err != nil {
			return next // nothing by that name: it is free
		}
	}
}
