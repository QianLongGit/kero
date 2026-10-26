package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"kero/internal/client"
	"kero/internal/proto"

	"kero/locales"
)

// A remote host's file opens through a local cache: it downloads once,
// any editor's edit to the cache writes back under the hash of what was
// read, and a file the host changed meanwhile asks before it is
// overwritten. The tests stand in for the host's kerod.

// fileSHA hashes what a file read or wrote, as the cache's lock on its
// content does: the sha256 of it, hex.
func fileSHA(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// filesHost is an app with a remote host whose kerod the test answers
// for: its files come from a map, and fs.write records what came.
type filesHost struct {
	a *App
	h *host

	mu       sync.Mutex
	files    map[string][]byte     // the host's, by path
	reads    map[string]int        // fs.read as they came, by path
	writes   []proto.FSWriteParams // fs.write as they came
	writeErr error                 // what fs.write answers instead, nil for ok
	readErr  error                 // what fs.read answers instead, nil for ok
	split    *proto.CreateParams   // the split the pane's open made, nil for none

	// ui stands for the UI thread: the app's updates run one at a time
	// under it, and what a test asks of their state takes it too.
	ui sync.Mutex
}

// newFilesHost makes the app and its remote host, whose kerod the test
// is: the app's updates run inline, as a window would marshal them.
func newFilesHost(t *testing.T) *filesHost {
	t.Helper()
	f := &filesHost{files: map[string][]byte{}, reads: map[string]int{}}
	cl, _ := projectClient(t, f.handle)
	a := testApp(t)
	a.updateFn = func(fn func()) { f.ui.Lock(); defer f.ui.Unlock(); fn() }
	f.a, f.h = a, &host{name: "remote", cl: cl, conn: connReady, loaded: true}
	a.hosts = append(a.hosts, f.h)
	a.owner = map[string]*host{"s1": f.h, "p1": f.h, "a": f.h, "b": f.h}
	return f
}

// handle answers as the host's kerod would, from the test's own files.
func (f *filesHost) handle(method string, raw json.RawMessage) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "fs.stat":
		var p proto.FSStatParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		b, ok := f.files[p.Path]
		if !ok {
			return proto.FSStat{}, nil
		}
		return proto.FSStat{Path: p.Path, Exists: true, Size: int64(len(b))}, nil
	case "fs.read":
		var p proto.FSReadParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		f.reads[p.Path]++
		if err := f.readErr; err != nil {
			return nil, err
		}
		b, ok := f.files[p.Path]
		if !ok {
			return nil, errors.New("no such file")
		}
		return proto.FSRead{Data: b}, nil
	case "fs.write":
		var p proto.FSWriteParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		f.writes = append(f.writes, p)
		if err := f.writeErr; err != nil {
			return nil, err
		}
		f.files[p.Path] = p.Content
		return fileSHA(p.Content), nil
	case "split":
		var p proto.CreateParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		f.split = &p
		return f.a.st.Sessions[0], nil
	}
	return nil, fmt.Errorf("unknown method %s", method)
}

// entry returns the cache's record of a local file, nil for none.
func (f *filesHost) entry(local string) *cacheEntry {
	f.a.cache.mu.Lock()
	defer f.a.cache.mu.Unlock()
	return f.a.cache.entries[local]
}

// writes returns the fs.write calls as they came.
func (f *filesHost) writesOf() []proto.FSWriteParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proto.FSWriteParams(nil), f.writes...)
}

// waitsFor holds the test until what asks of the app's state holds, a
// second past due saying why it never did: the watcher's own timers run
// real time, half a second of poll and one of quiet. What it asks runs
// under the UI's lock, as one thing at a time.
func (f *filesHost) waitsFor(t *testing.T, ask func() error) {
	t.Helper()
	for start := time.Now(); time.Since(start) < 8*time.Second; {
		f.ui.Lock()
		err := ask()
		f.ui.Unlock()
		if err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.ui.Lock()
	defer f.ui.Unlock()
	t.Fatalf("never: %v", ask())
}

// A remote file comes to the cache once: the second open of it takes the
// first's copy, and a local host's file is its own path, which opens as
// it is.
func TestFilesOpenMaterialize(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/main.go"] = []byte("package main\n")

	local, err := f.a.materialize("remote", "/srv/demo/main.go")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if !strings.HasPrefix(local, filepath.Join(proto.Dir(), "files")+string(os.PathSeparator)) {
		t.Errorf("the cache is at %q, want under %s", local, filepath.Join(proto.Dir(), "files"))
	}
	if base := filepath.Base(local); !strings.HasSuffix(base, "main.go") || len(base) <= len("main.go") {
		t.Errorf("the cache file is %q, want a hash with the file's own name kept", base)
	}
	if b, err := os.ReadFile(local); err != nil || string(b) != "package main\n" {
		t.Errorf("the cache holds %q, %v", b, err)
	}

	// The second materialize of the file reads none: the first's copy
	// answers.
	again, err := f.a.materialize("remote", "/srv/demo/main.go")
	if err != nil {
		t.Fatalf("materialize again: %v", err)
	}
	if again != local {
		t.Errorf("a second materialize gave %q, want the first's %q", again, local)
	}
	if n := func() int { f.mu.Lock(); defer f.mu.Unlock(); return f.reads["/srv/demo/main.go"] }(); n != 1 {
		t.Errorf("fs.read came %d times, want the one", n)
	}

	// A file gone from the cache comes down again.
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	f.files["/srv/demo/main.go"] = []byte("package main\n// changed\n")
	if _, err := f.a.materialize("remote", "/srv/demo/main.go"); err != nil {
		t.Fatalf("materialize after a removal: %v", err)
	}
	if n := func() int { f.mu.Lock(); defer f.mu.Unlock(); return f.reads["/srv/demo/main.go"] }(); n != 2 {
		t.Errorf("fs.read came %d times, want a second for the refetch", n)
	}

	// A local host's file opens as it is: no cache, no calls.
	p, err := f.a.materialize("", "/tmp/here.txt")
	if err != nil || p != "/tmp/here.txt" {
		t.Errorf("a local file materialized to %q, %v", p, err)
	}

	// withLocalFile hands the app's open of a file the path as this
	// machine's: a local host's at once, a remote host's as the cache
	// holds it, brought if it is not there.
	got := ""
	f.a.withLocalFile(f.a.local(), "/tmp/here.txt", func(l string) { got = l })
	if got != "/tmp/here.txt" {
		t.Errorf("a local file opened %q", got)
	}
	ch := make(chan string, 1)
	f.a.withLocalFile(f.h, "/srv/demo/main.go", func(l string) { ch <- l })
	select {
	case l := <-ch:
		if l != local {
			t.Errorf("a remote file opened %q, want its cache %q", l, local)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a remote file never opened")
	}
	if e := f.entry(local); e == nil || e.hash != fileSHA([]byte("package main\n// changed\n")) {
		t.Errorf("the cache's record is %+v", e)
	}
}

// A file larger than what fs.read reads never comes down: it is refused
// before the read.
func TestFilesOpenMaterializeLarge(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/blob.bin"] = make([]byte, cacheMax+1)
	if _, err := f.a.materialize("remote", "/srv/demo/blob.bin"); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Errorf("a 32 MB file materialized: %v", err)
	}
	if n := func() int { f.mu.Lock(); defer f.mu.Unlock(); return f.reads["/srv/demo/blob.bin"] }(); n != 0 {
		t.Errorf("fs.read came %d times, want none", n)
	}
}

// An edit to a cached file writes back once it has kept still a second:
// with the hash of what was read, and the second edit with the hash the
// first write returned.
func TestFilesOpenWatcherUpload(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/main.go"] = []byte("one")
	local, err := f.a.materialize("remote", "/srv/demo/main.go")
	if err != nil {
		t.Fatal(err)
	}
	// The first edit writes back under the hash of what was read.
	if err := os.WriteFile(local, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.waitsFor(t, func() error {
		if w := f.writesOf(); len(w) >= 1 {
			if string(w[0].Content) != "two" {
				return fmt.Errorf("the upload was %q, want two", w[0].Content)
			}
			if w[0].Hash != fileSHA([]byte("one")) {
				return fmt.Errorf("the upload's hash was %s, want the read's", w[0].Hash)
			}
			return nil
		}
		return errors.New("no upload came")
	})
	// The record moved to what the write returned, so the next edit
	// carries it.
	f.waitsFor(t, func() error {
		if e := f.entry(local); e != nil && e.hash == fileSHA([]byte("two")) {
			return nil
		}
		return errors.New("the record kept the old hash")
	})
	// The second edit writes back under the hash of what the first
	// wrote.
	if err := os.WriteFile(local, []byte("three"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.waitsFor(t, func() error {
		w := f.writesOf()
		if len(w) < 2 {
			return fmt.Errorf("only %d uploads came", len(w))
		}
		if last := w[len(w)-1]; string(last.Content) != "three" || last.Hash != fileSHA([]byte("two")) {
			return fmt.Errorf("the upload was %q under %s, want three under two's hash", last.Content, last.Hash)
		}
		return nil
	})
	// A file that keeps still stays uploaded: no third write arrives.
	time.Sleep(1500 * time.Millisecond)
	if w := f.writesOf(); len(w) != 2 {
		t.Errorf("%d uploads came, want the two", len(w))
	}
}

// An editor that saves atomically — a temporary file renamed over the
// cache's — is an edit like any other: the path's own stat, not the
// file's identity, is what the watcher follows.
func TestFilesOpenWatcherAtomicSave(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/main.go"] = []byte("one")
	local, err := f.a.materialize("remote", "/srv/demo/main.go")
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(filepath.Dir(local), ".edit-tmp")
	if err := os.WriteFile(tmp, []byte("saved whole"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, local); err != nil {
		t.Fatal(err)
	}
	f.waitsFor(t, func() error {
		w := f.writesOf()
		if len(w) == 0 {
			return errors.New("no upload came")
		}
		if string(w[len(w)-1].Content) != "saved whole" {
			return fmt.Errorf("the upload was %q", w[len(w)-1].Content)
		}
		return nil
	})
}

// A cache file the user deleted takes its record with it: nothing of it
// syncs again.
func TestFilesOpenWatcherDrop(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/main.go"] = []byte("one")
	local, err := f.a.materialize("remote", "/srv/demo/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	f.a.watchCache()
	if e := f.entry(local); e != nil {
		t.Errorf("a deleted cache file kept its record %+v", e)
	}
}

// A write the host answers with a conflict asks before it overwrites:
// the dialog offers to overwrite or take the host's file again, and
// nothing was written until one is chosen.
func TestFilesOpenConflict(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/main.go"] = []byte("one")
	local, err := f.a.materialize("remote", "/srv/demo/main.go")
	if err != nil {
		t.Fatal(err)
	}
	e := f.entry(local)
	if err := os.WriteFile(local, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.a.cacheUploaded(local, e, "", errors.New("conflict: /srv/demo/main.go changed since it was read"))
	cf := f.a.confirm
	if cf == nil || cf.title != locales.FilesConflictTitle() {
		t.Fatalf("no dialog of the conflict: %+v", f.a.confirm)
	}
	if w := f.writesOf(); len(w) != 0 {
		t.Fatalf("a conflict wrote %d times before anyone chose", len(w))
	}
	if !strings.Contains(cf.detail, "/srv/demo/main.go") {
		t.Errorf("the dialog says %q, want the file's own path", cf.detail)
	}

	// Overwrite writes the edit with no hash: whatever is on the host
	// gives way.
	cf.run()
	f.waitsFor(t, func() error {
		w := f.writesOf()
		if len(w) != 1 {
			return fmt.Errorf("%d writes came", len(w))
		}
		if string(w[0].Content) != "mine" || w[0].Hash != "" {
			return fmt.Errorf("the overwrite was %q under %q", w[0].Content, w[0].Hash)
		}
		return nil
	})
	f.waitsFor(t, func() error {
		if e := f.entry(local); e != nil && e.hash == fileSHA([]byte("mine")) {
			return nil
		}
		return errors.New("the record kept the old hash")
	})
}

// The dialog's other answer takes the host's file again: the cache is
// written over with it and its hash reset, so the watcher does not take
// the fetch for an edit.
func TestFilesOpenConflictRedownload(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/main.go"] = []byte("one")
	local, err := f.a.materialize("remote", "/srv/demo/main.go")
	if err != nil {
		t.Fatal(err)
	}
	e := f.entry(local)
	f.a.cacheUploaded(local, e, "", errors.New("conflict: /srv/demo/main.go changed since it was read"))
	f.files["/srv/demo/main.go"] = []byte("the host's own")
	if cf := f.a.confirm; cf == nil || cf.altRun == nil {
		t.Fatalf("no dialog with another answer: %+v", f.a.confirm)
	} else {
		cf.altRun()
	}
	f.waitsFor(t, func() error {
		b, err := os.ReadFile(local)
		if err != nil {
			return err
		}
		if string(b) != "the host's own" {
			return fmt.Errorf("the cache holds %q", b)
		}
		if e := f.entry(local); e == nil || e.hash != fileSHA([]byte("the host's own")) {
			return errors.New("the record kept the old hash")
		}
		return nil
	})
	// The refetch is no edit: nothing writes back.
	time.Sleep(1500 * time.Millisecond)
	if w := f.writesOf(); len(w) != 0 {
		t.Errorf("%d writes came of a refetch", len(w))
	}
}

// A host's kerod too old for fs.write is told as the files' own hint to
// update it, and the file keeps its record: nothing is lost.
func TestFilesOpenUnknownMethod(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/main.go"] = []byte("one")
	local, err := f.a.materialize("remote", "/srv/demo/main.go")
	if err != nil {
		t.Fatal(err)
	}
	e := f.entry(local)
	f.a.cacheUploaded(local, e, "", errors.New("unknown method fs.write"))
	if f.a.pendingToast != locales.FilesUpdateHost() {
		t.Errorf("the toast is %q, want the update hint", f.a.pendingToast)
	}
	if e := f.entry(local); e == nil {
		t.Error("the record was dropped")
	}
}

// A write the host refuses — the path no file, a symlink say — is told
// of by the file's own path, and dropped: the next edit tries again.
func TestFilesOpenCantWrite(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/link"] = []byte("one")
	local, err := f.a.materialize("remote", "/srv/demo/link")
	if err != nil {
		t.Fatal(err)
	}
	e := f.entry(local)
	f.a.cacheUploaded(local, e, "", errors.New("/srv/demo/link is no file"))
	if toast := f.a.pendingToast; !strings.Contains(toast, locales.FilesCantWrite()) || !strings.Contains(toast, "/srv/demo/link") {
		t.Errorf("the toast is %q, want the host's own refusal by the file's path", toast)
	}
}

// A remote host's file has the Open With menu now: its path is not one
// of this machine, but the cache brings the file here as it opens.
func TestFilesOpenWithRemoteMenu(t *testing.T) {
	// The menu is what is asked of here, not an open run through it: an
	// app whose kerod never answers tells the menu from a click the
	// same, with no update of its own to run.
	a := testApp(t)
	s := &a.st.Sessions[0]
	a.prefs.Side = sideFiles
	h := &host{name: "remote", conn: connReady, cl: &client.Client{}, loaded: true}
	a.hosts = append(a.hosts, h)
	a.owner = map[string]*host{"s1": h, "p1": h, "a": h, "b": h}
	s.Panes[0].ProcessDir = "/srv/demo"
	pane, root := a.filesRoot(s)
	a.tree(pane, root).dirs[root] = &fileDir{paths: []string{"/srv/demo/main.go"}}
	tt := ui.NewTester(a.view, 1280, 720)
	tt.Frame()
	if err := tt.RightClick("main.go"); err != nil {
		t.Fatal(err)
	}
	want := []string{locales.CommonOpen(), locales.FilesOpenVSCode(), locales.FilesChooseApp()}
	if got := tt.Menu(); len(got) < 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("a remote file's menu is %q, want %q", got, want)
	}
	tt.CloseMenu()
}

// What a click on a remote file opens is the file of the cache: the
// preview reads the copy here, and its title stays the host's own path.
func TestFilesOpenRemotePreview(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/main.go"] = []byte("package main\n")
	s := &f.a.st.Sessions[0]
	pane, root := f.a.filesRoot(s)
	tree := f.a.tree(pane, root)
	a := f.a
	a.chooseFile(tree, "/srv/demo/main.go")
	if tree.shown != "/srv/demo/main.go" {
		t.Fatalf("the tree shows %q open", tree.shown)
	}
	f.waitsFor(t, func() error {
		if tree.file == nil || tree.file.path == "" {
			return errors.New("no file came")
		}
		if !strings.HasPrefix(tree.file.path, filepath.Join(proto.Dir(), "files")) {
			return fmt.Errorf("the file opened is %q, want the cache's", tree.file.path)
		}
		if tree.file.src != "/srv/demo/main.go" {
			return fmt.Errorf("the file's own path is %q", tree.file.src)
		}
		if tree.file.st == nil || tree.file.st.Buffer == nil || tree.file.st.Buffer.String() != "package main\n" {
			return errors.New("the preview did not read the cache")
		}
		return nil
	})
	// The read came from the cache: fs.read answered it once, when it
	// materialized, and not again for the preview.
	if n := func() int { f.mu.Lock(); defer f.mu.Unlock(); return f.reads["/srv/demo/main.go"] }(); n != 1 {
		t.Errorf("fs.read came %d times, want the materialize's one", n)
	}
}

// A file pane a click on a remote file makes opens the cache's copy: the
// pane's own host could not read a path of this machine.
func TestFilesOpenRemotePane(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/main.go"] = []byte("package main\n")
	a, s := f.a, &f.a.st.Sessions[0]
	pane, root := a.filesRoot(s)
	tree := a.tree(pane, root)
	a.prefs.FileOpen = "pane"
	a.chooseFile(tree, "/srv/demo/main.go")
	f.waitsFor(t, func() error {
		f.mu.Lock()
		p := f.split
		f.mu.Unlock()
		if p == nil {
			return errors.New("no pane was made")
		}
		if !strings.HasPrefix(p.File, filepath.Join(proto.Dir(), "files")) {
			return fmt.Errorf("the pane shows %q, want the cache's copy", p.File)
		}
		if p.Split != tree.pane {
			return fmt.Errorf("the pane split %q, want beside %s", p.Split, tree.pane)
		}
		return nil
	})
}
