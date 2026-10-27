package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"

	"kero/internal/client"
	"kero/internal/proto"
)

// A pane of a cached file is named by the file's own name: the hash the
// cache writes before it is where the file lives, not what it is, and a
// file of this machine keeps its whole name, however like a hash it
// reads.
func TestFilePaneCachedName(t *testing.T) {
	local := cachePath("remote", "/srv/demo/main.go")
	if name := cacheTitle(local); name != "main.go" {
		t.Errorf("a cached file named %q, want main.go", name)
	}
	lookalike := filepath.Join("/tmp", "0123456789abcdef-main.go")
	if name := cacheTitle(lookalike); name != "0123456789abcdef-main.go" {
		t.Errorf("a file of this machine named %q, want its whole name", name)
	}
	if name := cacheTitle("/tmp/demo/main.go"); name != "main.go" {
		t.Errorf("an ordinary file named %q, want main.go", name)
	}
}

// The header of a pane of a cached file shows the file's own name, not
// the hash the cache wrote before it.
func TestFilePaneCachedTitle(t *testing.T) {
	a := testApp(t)
	a.local().cl = &client.Client{}
	s := &a.st.Sessions[0]
	local := cachePath("remote", "/srv/demo/main.go")
	s.Panes = append(s.Panes, proto.Pane{ID: "f", File: local, Dir: "/tmp/demo", Status: proto.Idle, Progress: -1})
	s.Layout = s.Layout.Split("a", "f", "right", 0)
	f := a.filePaneFor(s.PaneByID("f"))
	f.read = true
	b := editor.NewBuffer("package main\n")
	b.SetLanguage(editor.LanguageFor("main.go"))
	f.st = &editor.State{Buffer: b}
	tt := ui.NewTester(a.view, 1280, 720)
	tt.Frame()
	texts := tt.Texts()
	if !slices.Contains(texts, "main.go") {
		t.Errorf("the pane titled itself by no bare name; texts %q", texts)
	}
	for _, tx := range texts {
		if strings.Contains(tx, filepath.Base(local)) {
			t.Errorf("a text shows the cache's hashed name: %q", tx)
		}
	}
}
