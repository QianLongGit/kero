package main

// The menu's own: the paths a paste is of, and the copy and the paste
// itself — a file crossing hosts on a paste, pasted under its own name
// or a copy beside it — and a delete through the host, which asks first
// (files_ops.go's askDelete).

import (
	"os"
	"path/filepath"
	"testing"

	"kero/internal/proto"
)

func TestParentPath(t *testing.T) {
	for path, want := range map[string]string{
		"/a.txt":          "/",
		"/x/a.txt":        "/x",
		"/x/y/a.txt":      "/x/y",
		`C:\a.txt`:        `C:\`,
		`C:\x\a.txt`:      `C:\x`,
		`C:\x\y\a.txt`:    `C:\x\y`,
		`\\srv\a.txt`:     `\\srv`,
		`\\srv\share\a.b`: `\\srv\share`,
		"a.txt":           "",
	} {
		if got := parentPath(path); got != want {
			t.Errorf("parentPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestPasteName(t *testing.T) {
	for _, c := range []struct {
		base string
		n    int
		want string
	}{
		{"a.txt", 0, "a.txt"},
		{"a.txt", 1, "a copy.txt"},
		{"a.txt", 2, "a copy 2.txt"},
		{"a", 1, "a copy"},
		{".hidden", 1, ".hidden copy"},
		{"a.b.c", 1, "a.b copy.c"},
	} {
		if got := pasteName(c.base, c.n); got != c.want {
			t.Errorf("pasteName(%q, %d) = %q, want %q", c.base, c.n, got, c.want)
		}
	}
}

// A paste reads the copied file where it was copied and writes it into
// the folder the paste is of, under its own name or the next free copy of
// it.
func TestPasteFileInto(t *testing.T) {
	f := newFilesHost(t)
	f.files["/r/a.txt"] = []byte("a")
	f.a.side.files.clip = &fileClip{host: "remote", path: "/r/a.txt"}

	f.a.pasteFileInto("p1", "/dst")
	waits(t, "the paste to write", func() bool { return len(f.writesOf()) > 0 })
	if w := f.writesOf()[0]; w.Path != "/dst/a.txt" || w.Text != "a" {
		t.Errorf("pasted %q of %q", w.Path, w.Text)
	}

	// A file of the name there takes the next free copy beside it, not
	// its place.
	f.mu.Lock()
	f.writes = nil
	f.files["/dst/a.txt"] = []byte("old")
	f.files["/dst/a copy.txt"] = []byte("old")
	f.mu.Unlock()
	f.a.pasteFileInto("p1", "/dst")
	waits(t, "the paste to write again", func() bool { return len(f.writesOf()) > 0 })
	if w := f.writesOf()[0]; w.Path != "/dst/a copy 2.txt" {
		t.Errorf("pasted over a file as %q", w.Path)
	}

	// A copy of this machine reads its own disk, not a host's.
	src := filepath.Join(t.TempDir(), "b.txt")
	if err := os.WriteFile(src, []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.a.side.files.clip = &fileClip{host: "", path: src}
	f.mu.Lock()
	f.writes = nil
	f.mu.Unlock()
	f.a.pasteFileInto("p1", "/dst")
	waits(t, "the local paste to write", func() bool { return len(f.writesOf()) > 0 })
	if w := f.writesOf()[0]; w.Path != "/dst/b.txt" || w.Text != "b" {
		t.Errorf("pasted %q of %q", w.Path, w.Text)
	}

	// A source that is none tells why, and writes nothing.
	f.a.side.files.clip = &fileClip{host: "remote", path: "/r/gone.txt"}
	f.mu.Lock()
	f.writes = nil
	f.mu.Unlock()
	f.a.pasteFileInto("p1", "/dst")
	waits(t, "the failed paste to answer", func() bool {
		f.ui.Lock()
		defer f.ui.Unlock()
		return f.a.pendingToast != ""
	})
	if len(f.writesOf()) > 0 {
		t.Error("a failed paste wrote")
	}
}

// A delete through the host's own menu asks first (TestFilesTabChanges
// walks the asking), then deletes for good and the folders it held list
// anew.
func TestDeleteEntry(t *testing.T) {
	f := newFilesHost(t)
	f.files["/dst/x.txt"] = []byte("x")
	it := fileItem{FSEntry: proto.FSEntry{Name: "x.txt"}, path: "/dst/x.txt", rel: "x.txt", parent: "/dst"}

	f.a.deleteEntry(f.h, it)
	waits(t, "the delete to answer", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		_, there := f.files["/dst/x.txt"]
		return !there
	})
	// The tree lists its folders anew: the listing of the folder that
	// held the file is stale now.
	f.ui.Lock()
	stale := f.a.side.files.dirs[filesKey(f.h, "/dst")] == nil || f.a.side.files.dirs[filesKey(f.h, "/dst")].gen != f.a.side.files.gen
	f.ui.Unlock()
	if !stale {
		t.Error("the delete left the folder's listing as it was")
	}
}
