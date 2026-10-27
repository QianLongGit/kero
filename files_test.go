package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"

	"kero/internal/client"
	"kero/internal/proto"
	"kero/internal/settings"
)

// The Files view shows a session's folder as a tree, its folders opening
// on a click, and the file chosen in it under the tree: its text, or why
// it shows none.
func TestFilesView(t *testing.T) {
	a := testApp(t)
	s := &a.st.Sessions[0]
	a.prefs.Side = sideFiles
	// A host connected, whose folders never come: the tree shows what it
	// was given.
	a.local().cl = &client.Client{}

	// What kerod would answer: the pane's own folder, which is the tree's.
	s.Panes[0].ProcessDir = "/tmp/demo"
	pane, root := a.filesRoot(s)
	tree := a.tree(pane, root)
	tree.dirs[root] = &fileDir{paths: []string{"/tmp/demo/src", "/tmp/demo/main.go", "/tmp/demo/logo.png", "/tmp/demo/blob.bin"}}
	for _, p := range tree.dirs[root].paths {
		tree.isDir[p] = p == "/tmp/demo/src"
	}
	tree.dirs["/tmp/demo/src"] = &fileDir{paths: []string{"/tmp/demo/src/a.go"}}

	tt := ui.NewTester(a.view, 1280, 720)
	tt.Frame()
	for _, want := range []string{"Files", "Changes", "demo", "src", "main.go", "logo.png"} {
		if !tt.HasText(want) {
			t.Fatalf("no %q; texts %q", want, tt.Texts())
		}
	}
	shot(t, tt, "files-tree")

	// A folder opens on a click.
	if err := tt.Click("src"); err != nil {
		t.Fatalf("click src: %v", err)
	}
	tt.Frame()
	if !tt.HasText("a.go") {
		t.Errorf("a folder did not open; texts %q", tt.Texts())
	}

	// A file opens under the tree.
	f := &openFile{path: "/tmp/demo/main.go"}
	b := editor.NewBuffer("package main\n\nfunc main() {}\n")
	b.SetLanguage(editor.LanguageFor("main.go"))
	f.st = &editor.State{Buffer: b}
	tree.shown, tree.file = "/tmp/demo/main.go", f
	tt.Frame()
	if !tt.HasText("Code editor") {
		t.Errorf("no text of the file; texts %q", tt.Texts())
	}
	shot(t, tt, "files-open")

	// A binary file is told of, not shown.
	tree.shown, tree.file = "/tmp/demo/blob.bin", &openFile{path: "/tmp/demo/blob.bin", binary: true}
	tt.Frame()
	if !tt.HasText("Binary file") {
		t.Errorf("no note of a binary file; texts %q", tt.Texts())
	}

	// A session with no folder says why.
	s.Panes[0].Dir, s.Panes[0].ProcessDir, s.Panes[1].Dir = "", "", ""
	tt.Frame()
	if !tt.HasText("No folder") {
		t.Errorf("no note of a session without a folder; texts %q", tt.Texts())
	}
}

// What a click on a file of the tree does: it chooses the file — the
// keyboard's choice too — and with the preview on shows it under the
// tree, while a double click opens it the way Settings say.
func TestFileOpenChoice(t *testing.T) {
	a := testApp(t)
	s := &a.st.Sessions[0]
	a.prefs.Side = sideFiles
	a.local().cl, _ = projectClient(t, func(method string, raw json.RawMessage) (any, error) {
		return nil, errors.New("not in a test")
	})
	s.Panes[0].ProcessDir = "/tmp/demo"
	pane, root := a.filesRoot(s)
	tree := a.tree(pane, root)
	tree.dirs[root] = &fileDir{paths: []string{"/tmp/demo/a.txt", "/tmp/demo/main.go"}}
	tt := ui.NewTester(a.view, 1280, 720)
	tt.Frame()
	// The row above the tree has the Preview toggle alone: what a double
	// click opens is Settings' to say, not the tree's.
	if !tt.HasText("Preview") {
		t.Fatalf("no Preview toggle; texts %q", tt.Texts())
	}
	for _, gone := range []string{"Open with", "File Pane", "Editor"} {
		if tt.HasText(gone) {
			t.Errorf("the row above the tree still offers %q", gone)
		}
	}

	// A click chooses the file, the keyboard's choice too, and shows it
	// under the tree.
	if err := tt.Click("a.txt"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if tree.chosen < 0 || tree.outline.Item(tree.chosen) != "/tmp/demo/a.txt" {
		t.Errorf("a click chose row %d, want a.txt", tree.chosen)
	}
	if tree.shown != "/tmp/demo/a.txt" {
		t.Errorf("a click showed %q, want the file under the tree", tree.shown)
	}

	// The preview off, a click only chooses: what showed keeps showing.
	off := false
	a.prefs.FilesPreview = &off
	tt.Frame()
	if err := tt.Click("main.go"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if tree.chosen < 0 || tree.outline.Item(tree.chosen) != "/tmp/demo/main.go" {
		t.Errorf("a click chose row %d, want main.go", tree.chosen)
	}
	if tree.shown != "/tmp/demo/a.txt" {
		t.Errorf("with the preview off, a click showed %q", tree.shown)
	}

	// A double click opens the file the way Settings say: a file pane by
	// default, beside the tree's pane. The handler runs on the client's
	// goroutine, so the raw call travels a channel and unmarshals on this
	// one, read once here.
	calls := make(chan json.RawMessage, 2)
	a.local().cl, _ = projectClient(t, func(method string, raw json.RawMessage) (any, error) {
		if method != "split" {
			return nil, errors.New("not in a test")
		}
		select {
		case calls <- raw:
		default:
		}
		return s, nil
	})
	if err := tt.Click("main.go"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("main.go"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	var called proto.CreateParams
	for i := 0; i < 100 && called.File == ""; i++ {
		tt.Frame()
		select {
		case raw := <-calls:
			if err := json.Unmarshal(raw, &called); err != nil {
				t.Fatal(err)
			}
		default:
		}
	}
	if called.File != "/tmp/demo/main.go" || called.Split != tree.pane {
		t.Errorf("split %+v, want a file pane on main.go beside %s", called, tree.pane)
	}

	// The editor setting takes the same open to the host's editor.
	a.set.FileOpen = "editor"
	a.openBySetting(tree, "/tmp/demo/a.txt")
	called = proto.CreateParams{}
	for i := 0; i < 100 && len(called.Command) == 0; i++ {
		tt.Frame()
		select {
		case raw := <-calls:
			if err := json.Unmarshal(raw, &called); err != nil {
				t.Fatal(err)
			}
		default:
		}
	}
	if len(called.Command) == 0 || called.File != "" || called.Split != tree.pane {
		t.Errorf("split %+v, want the host's editor on a.txt", called)
	}

	// The Preview toggle turns the preview back on.
	if err := tt.Click("Preview"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !a.filesPreview() {
		t.Error("the Preview toggle did not turn the preview on")
	}
}

// The Files view's old "what a click on a file does" preference becomes
// the file-open setting and leaves the app's own preferences: "preview"
// and nothing become the default, a file pane, "pane" and "editor"
// themselves.
func TestMigrateFileOpenPref(t *testing.T) {
	for _, c := range []struct {
		pref, want string
	}{
		{"", "pane"},
		{"preview", "pane"},
		{"pane", "pane"},
		{"editor", "editor"},
	} {
		home := t.TempDir()
		t.Setenv("KERO_HOME", home)
		t.Setenv("KERO_CONFIG", filepath.Join(home, "config"))
		if c.pref != "" {
			prefs := `{"fileOpen": "` + c.pref + `", "selected": "s1"}`
			if err := os.WriteFile(prefsPath(), []byte(prefs), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		a := newApp()
		if a.set.FileOpen != c.want {
			t.Errorf("pref %q opened as %q, want %q", c.pref, a.set.FileOpen, c.want)
		}
		// The preference's job is done: app.json keeps the rest, as the
		// choice the setting holds now.
		if c.pref == "" {
			continue
		}
		b, err := os.ReadFile(prefsPath())
		if err != nil {
			t.Fatal(err)
		}
		var kept map[string]any
		if err := json.Unmarshal(b, &kept); err != nil {
			t.Fatal(err)
		}
		if _, ok := kept["fileOpen"]; ok {
			t.Errorf("pref %q stayed in app.json: %s", c.pref, b)
		}
		if kept["selected"] != "s1" {
			t.Errorf("pref %q lost the rest of app.json: %s", c.pref, b)
		}
		if c.want == "pane" {
			continue // the default lives in the app, unwritten
		}
		cfg, err := os.ReadFile(settings.Path())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(cfg), "file-open = "+c.want) {
			t.Errorf("pref %q did not reach the config: %s", c.pref, cfg)
		}
	}
}

// A path of a host joins with the host's separator, which its own paths
// tell.
func TestHostJoin(t *testing.T) {
	for _, c := range []struct{ dir, name, want string }{
		{"/tmp/demo", "src", "/tmp/demo/src"},
		{"/tmp/", "src", "/tmp/src"},
		{`C:\Users\me`, "src", `C:\Users\me\src`},
		{`C:\Users\me\`, "src", `C:\Users\me\src`},
		{`C:`, "src", `C:\src`},
	} {
		if got := hostJoin(c.dir, c.name); got != c.want {
			t.Errorf("hostJoin(%q, %q) = %q, want %q", c.dir, c.name, got, c.want)
		}
	}
}

// A tree names a path from its folder, with either slash between.
func TestFileTreeRel(t *testing.T) {
	tree := &fileTree{root: "/tmp/demo"}
	for p, want := range map[string]string{
		"/tmp/demo/main.go":  "main.go",
		"/tmp/demo/src/a.go": "src/a.go",
		`/tmp/demo/src\a.go`: `src\a.go`,
	} {
		if got := tree.rel(p); got != want {
			t.Errorf("rel(%q) = %q, want %q", p, got, want)
		}
	}
	if !strings.HasPrefix(tree.rel("/tmp/demo/x"), "x") {
		t.Error("rel kept the folder's slash")
	}
	_ = proto.FSList{}
}

// The header's tabs show their view on a click, each way, so that the
// header's drag of the window does not keep them.
func TestSideTabsClick(t *testing.T) {
	a := testApp(t)
	s := &a.st.Sessions[0]
	s.Panes[0].ProcessDir, s.Panes[1].ProcessDir = "/tmp/demo", "/tmp/demo"
	// A host connected, whose folders never come: the tree shows what it
	// was given.
	a.local().cl = &client.Client{}
	pane, root := a.filesRoot(s)
	a.tree(pane, root).dirs[root] = &fileDir{paths: []string{"/tmp/demo/main.go"}}
	a.prefs.Side = sideChanges
	tt := ui.NewTester(a.view, 1280, 720)
	tt.Frame()
	if err := tt.Click("Files"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.prefs.Side != sideFiles {
		t.Fatalf("a click on Files showed %q", a.prefs.Side)
	}
	if !tt.HasText("main.go") {
		t.Errorf("the Files view did not show; texts %q", tt.Texts())
	}
	if err := tt.Click("Changes"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.prefs.Side != sideChanges {
		t.Fatalf("a click on Changes showed %q", a.prefs.Side)
	}
}

// A file pane shows its file in the app's editor, colored by its
// language, and has no terminal of its own.
func TestFilePane(t *testing.T) {
	a := testApp(t)
	a.local().cl = &client.Client{}
	s := &a.st.Sessions[0]
	s.Panes = append(s.Panes, proto.Pane{ID: "f", File: "/tmp/demo/main.go", Dir: "/tmp/demo", Status: proto.Idle, Progress: -1})
	s.Layout = s.Layout.Split("a", "f", "right", 0)
	p := s.PaneByID("f")
	if !p.FilePane() || !p.Shown() || p.Browser() {
		t.Fatalf("a file pane is not one: file %v, shown %v, browser %v", p.FilePane(), p.Shown(), p.Browser())
	}
	if name := p.PaneName(); name != "main.go" {
		t.Errorf("a file pane names itself %q", name)
	}
	if a.terms["f"] != nil {
		t.Error("a file pane has a terminal")
	}

	// What kerod would answer, read once.
	f := a.filePaneFor(p)
	f.read = true
	if a.hostOf("f").cl == nil {
		a.local().cl = &client.Client{}
	}
	b := editor.NewBuffer("package main\n\nfunc main() {}\n")
	b.SetLanguage(editor.LanguageFor("main.go"))
	f.st = &editor.State{Buffer: b}
	tt := ui.NewTester(a.view, 1280, 720)
	tt.Frame()
	if !tt.HasText("Code editor") || !tt.HasText("main.go") {
		t.Errorf("the file pane did not show its file; texts %q", tt.Texts())
	}
	f.binary = true
	tt.Frame()
	if !tt.HasText("Binary file") {
		t.Errorf("a binary file was not told of; texts %q", tt.Texts())
	}
}
