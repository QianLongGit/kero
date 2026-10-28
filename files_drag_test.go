package main

// What a drag of the tree's rows offers the system: a local host's file
// as itself, a remote host's as the cache's copy of it, and until one
// is down the path as text — with the copy asked for, so the next drag
// of the file is the file.

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/egoist/mygo/transfer"
)

// waits holds until what is of the fake host comes true, or fails the
// test at the deadline.
func waits(t *testing.T, what string, of func() bool) {
	t.Helper()
	for range 200 {
		if of() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestFileDragData(t *testing.T) {
	// A local host's file drags as itself, folder as file.
	a := testApp(t)
	local := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(local, []byte("a"), 0o600)
	d := a.fileDragData(&host{}, local, false)
	if paths, err := d.Files(); err != nil || !slices.Equal(paths, []string{local}) {
		t.Errorf("a local file drags as %v, %v", paths, err)
	}

	// A remote host's drags as the cache's copy of it.
	f := newFilesHost(t)
	f.files["/r/a.txt"] = []byte("a")
	cached := cachePath("remote", "/r/a.txt")
	os.MkdirAll(filepath.Dir(cached), 0o755)
	os.WriteFile(cached, []byte("a"), 0o600)
	f.a.cache.mu.Lock()
	if f.a.cache.entries == nil {
		f.a.cache.entries = map[string]*cacheEntry{}
	}
	f.a.cache.entries[cached] = &cacheEntry{host: "remote", remote: "/r/a.txt"}
	f.a.cache.mu.Unlock()
	d = f.a.fileDragData(f.h, "/r/a.txt", false)
	if paths, err := d.Files(); err != nil || !slices.Equal(paths, []string{cached}) {
		t.Errorf("a cached file drags as %v, %v", paths, err)
	}

	// One the cache does not have offers its path as text, and asks for
	// the copy: the next drag of it is the file.
	f.files["/r/b.txt"] = []byte("b")
	d = f.a.fileDragData(f.h, "/r/b.txt", false)
	if paths, _ := d.Files(); len(paths) != 0 {
		t.Errorf("an uncached file drags as a file: %v", paths)
	}
	if b, err := d.Read(transfer.Text); err != nil || string(b) != "/r/b.txt" {
		t.Errorf("an uncached file offers %q, %v", b, err)
	}
	waits(t, "the drag to fetch the file", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.reads["/r/b.txt"] > 0
	})

	// A remote folder is no file to offer: its path as text.
	d = f.a.fileDragData(f.h, "/r/dir", true)
	if _, ok := d.Preferred(transfer.Text); !ok {
		t.Error("a remote folder offers no text")
	}
	if paths, _ := d.Files(); len(paths) != 0 {
		t.Errorf("a remote folder drags as a file: %v", paths)
	}
}
