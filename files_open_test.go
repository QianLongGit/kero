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

// uploadOf is the upload the record's file would fly now, as the
// watcher queues one.
func (f *filesHost) uploadOf(local string) cacheUpload {
	u := cacheUpload{local: local}
	if e := f.entry(local); e != nil {
		u.host, u.remote, u.hash, u.of = e.host, e.remote, e.hash, e
	}
	return u
}

// underCache asks of a record under the cache's own lock, which the
// record's fields are of: the writes that answer an upload hold it, on
// whatever goroutine they run.
func (f *filesHost) underCache(local string, ask func(e *cacheEntry) error) error {
	f.a.cache.mu.Lock()
	defer f.a.cache.mu.Unlock()
	return ask(f.a.cache.entries[local])
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

	local, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/main.go")
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
	again, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/main.go")
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
	if _, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/main.go"); err != nil {
		t.Fatalf("materialize after a removal: %v", err)
	}
	if n := func() int { f.mu.Lock(); defer f.mu.Unlock(); return f.reads["/srv/demo/main.go"] }(); n != 2 {
		t.Errorf("fs.read came %d times, want a second for the refetch", n)
	}

	// A local host's file opens as it is: no cache, no calls.
	p, err := f.a.materialize(nil, "", "/tmp/here.txt")
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
	if err := f.underCache(local, func(e *cacheEntry) error {
		if e == nil || e.hash != fileSHA([]byte("package main\n// changed\n")) {
			return fmt.Errorf("the record is %+v", e)
		}
		return nil
	}); err != nil {
		t.Error(err)
	}
}

// A file larger than what fs.read reads never comes down: it is refused
// before the read.
func TestFilesOpenMaterializeLarge(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/blob.bin"] = make([]byte, cacheMax+1)
	if _, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/blob.bin"); err == nil || !strings.Contains(err.Error(), "larger") {
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
	local, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/main.go")
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
		return f.underCache(local, func(e *cacheEntry) error {
			if e != nil && e.hash == fileSHA([]byte("two")) {
				return nil
			}
			return errors.New("the record kept the old hash")
		})
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
	local, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/main.go")
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
	local, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/main.go")
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

// A host gone before its file's edit writes keeps the edit waiting,
// told of no one: it writes once the host is back.
func TestFilesOpenOfflineQuiet(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/main.go"] = []byte("one")
	local, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/main.go")
	if err != nil {
		t.Fatal(err)
	}
	// The host's connection drops, as the app's own UI thread sees it.
	f.ui.Lock()
	old := f.h.cl
	f.h.cl = nil
	f.ui.Unlock()
	defer func() { f.ui.Lock(); f.h.cl = old; f.ui.Unlock() }()
	if err := os.WriteFile(local, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The watcher queues the edit; with no connection to write by it
	// settles to wait, without a toast or a write. What it asks of the
	// record runs under the cache's own lock.
	waitsQuiet := func() (changed bool) {
		f.a.cache.mu.Lock()
		defer f.a.cache.mu.Unlock()
		e := f.a.cache.entries[local]
		return e != nil && !e.changed.IsZero() && !e.uploading
	}
	for start := time.Now(); time.Since(start) < 8*time.Second && !waitsQuiet(); {
		time.Sleep(10 * time.Millisecond)
	}
	if !waitsQuiet() {
		t.Fatal("the edit never settled to wait")
	}
	f.ui.Lock()
	toast := f.a.pendingToast
	f.ui.Unlock()
	if toast != "" {
		t.Errorf("a host not connected was told of: %q", toast)
	}
	if w := f.writesOf(); len(w) != 0 {
		t.Errorf("%d writes came with no host", len(w))
	}
	// Back, the edit writes at last.
	f.ui.Lock()
	f.h.cl = old
	f.ui.Unlock()
	f.waitsFor(t, func() error {
		if w := f.writesOf(); len(w) == 1 && string(w[0].Content) == "two" && w[0].Hash == fileSHA([]byte("one")) {
			return nil
		}
		return errors.New("the waiting edit never wrote")
	})
}

// A write the host answers with a conflict asks before it overwrites:
// the dialog offers to overwrite or take the host's file again, and
// nothing was written until one is chosen.
func TestFilesOpenConflict(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/main.go"] = []byte("one")
	local, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.a.cacheUploaded(f.uploadOf(local), "", errors.New("conflict: /srv/demo/main.go changed since it was read"))
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
	f.ui.Lock()
	cf.run()
	f.ui.Unlock()
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
		return f.underCache(local, func(e *cacheEntry) error {
			if e != nil && e.hash == fileSHA([]byte("mine")) {
				return nil
			}
			return errors.New("the record kept the old hash")
		})
	})
}

// The dialog's other answer takes the host's file again: the cache is
// written over with it and its hash reset, so the watcher does not take
// the fetch for an edit.
func TestFilesOpenConflictRedownload(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/main.go"] = []byte("one")
	local, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/main.go")
	if err != nil {
		t.Fatal(err)
	}
	f.a.cacheUploaded(f.uploadOf(local), "", errors.New("conflict: /srv/demo/main.go changed since it was read"))
	f.files["/srv/demo/main.go"] = []byte("the host's own")
	if cf := f.a.confirm; cf == nil || cf.altRun == nil {
		t.Fatalf("no dialog with another answer: %+v", f.a.confirm)
	} else {
		f.ui.Lock()
		cf.altRun()
		f.ui.Unlock()
	}
	f.waitsFor(t, func() error {
		b, err := os.ReadFile(local)
		if err != nil {
			return err
		}
		if string(b) != "the host's own" {
			return fmt.Errorf("the cache holds %q", b)
		}
		if err := f.underCache(local, func(e *cacheEntry) error {
			if e == nil || e.hash != fileSHA([]byte("the host's own")) {
				return errors.New("the record kept the old hash")
			}
			return nil
		}); err != nil {
			return err
		}
		return nil
	})
	// The refetch is no edit: nothing writes back.
	time.Sleep(1500 * time.Millisecond)
	if w := f.writesOf(); len(w) != 0 {
		t.Errorf("%d writes came of a refetch", len(w))
	}
}

// An upload's answer belongs to the record it flew from: a
// re-materialize that put another in its place keeps that one's state
// — the stale answer neither drops its mapping nor resets its hash.
func TestFilesOpenUploadedAnotherRecord(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/main.go"] = []byte("one")
	local, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/main.go")
	if err != nil {
		t.Fatal(err)
	}
	// The upload of the first record is still flying when a refetch
	// replaces the record wholesale.
	stale := f.uploadOf(local)
	f.files["/srv/demo/main.go"] = []byte("two")
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/main.go"); err != nil {
		t.Fatal(err)
	}
	// The stale upload finds its file gone: the new record stays.
	f.a.cacheUploaded(stale, "", fmt.Errorf("gone: %w", os.ErrNotExist))
	if e := f.entry(local); e == nil {
		t.Fatal("a stale upload dropped the record that took its place")
	}
	// A stale success writes its sum nowhere but its own record.
	f.a.cacheUploaded(stale, "a sum of another read", nil)
	if err := f.underCache(local, func(e *cacheEntry) error {
		if e == nil || e.hash != fileSHA([]byte("two")) {
			return fmt.Errorf("the record's hash is %q, want the refetch's", e.hash)
		}
		return nil
	}); err != nil {
		t.Error(err)
	}
}

// An edit made while a conflict's dialog stands is told of in its turn:
// the write flies on the hash the record had, meets the same conflict,
// and asks again — the edit is never carried in behind the dialog's
// answer.
func TestFilesOpenConflictPendingEdit(t *testing.T) {
	f := newFilesHost(t)
	f.mu.Lock()
	f.writeErr = errors.New("conflict: /srv/demo/main.go changed since it was read")
	f.mu.Unlock()
	f.files["/srv/demo/main.go"] = []byte("one")
	local, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The first edit meets its conflict and asks. (waitsFor holds the
	// UI's lock for its ask.)
	f.waitsFor(t, func() error {
		if f.a.confirm != nil {
			return nil
		}
		return errors.New("no dialog of the first conflict")
	})
	// With the dialog standing, another edit: it flies too, meets the
	// same conflict, and the asking stays.
	if err := os.WriteFile(local, []byte("more"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.waitsFor(t, func() error {
		w := f.writesOf()
		if len(w) != 2 {
			return fmt.Errorf("%d writes came", len(w))
		}
		if u := w[1]; string(u.Content) != "more" || u.Hash != fileSHA([]byte("one")) {
			return fmt.Errorf("the second write was %q under %s", u.Content, u.Hash)
		}
		return nil
	})
	f.ui.Lock()
	asked, toast := f.a.confirm != nil, f.a.pendingToast
	f.ui.Unlock()
	if !asked {
		t.Error("the second conflict did not ask")
	}
	if toast != "" {
		t.Errorf("a conflict was toasted: %q", toast)
	}
}

// A host's kerod too old for fs.write is told as the files' own hint to
// update it, and the file keeps its record: nothing is lost.
func TestFilesOpenUnknownMethod(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/main.go"] = []byte("one")
	local, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/main.go")
	if err != nil {
		t.Fatal(err)
	}
	f.a.cacheUploaded(f.uploadOf(local), "", errors.New("unknown method fs.write"))
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
	local, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/link")
	if err != nil {
		t.Fatal(err)
	}
	f.a.cacheUploaded(f.uploadOf(local), "", errors.New("/srv/demo/link is no file"))
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

// The cache's mappings live past the app: a pane whose file is one of
// the cache's — saved with its local path — reads the copy here again
// after a restart, not a path of this machine through the host, where it
// is no file.
func TestFilesOpenPaneAfterRestart(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/main.go"] = []byte("package main\n")
	local, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/main.go")
	if err != nil {
		t.Fatal(err)
	}
	// The host's copy gone, the cache's is the only one to read.
	f.mu.Lock()
	delete(f.files, "/srv/demo/main.go")
	f.mu.Unlock()

	// A new app over the same home is what a restart is; its pane holds
	// the local path the open left saved. (newApp, not testApp: a testApp
	// would move the home to a directory of its own.)
	a := newApp()
	a.updateFn = func(fn func()) { f.ui.Lock(); defer f.ui.Unlock(); fn() }
	h := &host{name: "remote", cl: f.h.cl, conn: connReady, loaded: true}
	a.hosts = append(a.hosts, h)
	a.owner = map[string]*host{"f": h}
	p := &proto.Pane{ID: "f", File: local}
	fp := a.filePaneFor(p)
	a.readFilePane(p, fp)
	f.waitsFor(t, func() error {
		if fp.err != "" {
			return fmt.Errorf("the pane erred: %s", fp.err)
		}
		if fp.st == nil || fp.st.Buffer == nil || fp.st.Buffer.String() != "package main\n" {
			return errors.New("the pane did not read the cache's copy")
		}
		return nil
	})
	// The pane's read was the cache's alone: the host answered one
	// fs.read, the materialize's.
	if n := func() int { f.mu.Lock(); defer f.mu.Unlock(); return f.reads["/srv/demo/main.go"] }(); n != 1 {
		t.Errorf("fs.read came %d times, want the materialize's one", n)
	}
}

// The cache's index holds its mappings across restarts: written whole on
// each change, read back tolerant, each file's hash taken from its own
// content — so nothing uploads that did not change, and a mapping whose
// file went stays gone.
func TestFilesOpenIndex(t *testing.T) {
	f := newFilesHost(t)
	f.files["/srv/demo/main.go"] = []byte("one")
	local, err := f.a.materialize(f.h.cl, "remote", "/srv/demo/main.go")
	if err != nil {
		t.Fatal(err)
	}
	// A materialize writes the index.
	ix := map[string]cacheIndexEntry{}
	b, err := os.ReadFile(cacheIndexPath())
	if err != nil {
		t.Fatalf("no index came of a materialize: %v", err)
	}
	if err := json.Unmarshal(b, &ix); err != nil || ix[local] != (cacheIndexEntry{Host: "remote", Remote: "/srv/demo/main.go"}) {
		t.Errorf("the index holds %v, %v", ix, err)
	}

	// A restart reads the mappings back: the hash is of the file's own
	// content, and what did not change does not write back.
	a := newApp()
	e := a.cacheRecord(local)
	if e == nil || e.host != "remote" || e.remote != "/srv/demo/main.go" || e.hash != fileSHA([]byte("one")) {
		t.Errorf("a restart left the record %+v", e)
	}
	time.Sleep(1600 * time.Millisecond)
	if w := f.writesOf(); len(w) != 0 {
		t.Errorf("%d writes came of a file that did not change", len(w))
	}

	// A mapping whose file went does not come back, on either side: the
	// watcher drops it from the map and the index holds it no more.
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	f.a.watchCache()
	ix = map[string]cacheIndexEntry{}
	if b, err := os.ReadFile(cacheIndexPath()); err != nil || json.Unmarshal(b, &ix) != nil || ix[local] != (cacheIndexEntry{}) {
		t.Errorf("the index after the drop holds %v, %v", ix, err)
	}
	if a2 := newApp(); a2.cacheRecord(local) != nil {
		t.Error("a dropped mapping came back with a restart")
	}

	// A corrupt index is nothing: the cache starts empty, the app stands.
	if err := os.WriteFile(cacheIndexPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if a3 := newApp(); len(a3.cache.entries) != 0 {
		t.Errorf("a corrupt index loaded %d entries", len(a3.cache.entries))
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
