//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"kero/internal/client"

	"kero/locales"
)

// The commands that open a file in an app are plain values, as the menu
// runs them: on macOS open asks LaunchServices for an app by name.
func TestOpenWithCommand(t *testing.T) {
	if got := openWithCommand("TextEdit", "/tmp/note.txt"); !slices.Equal(got, []string{"open", "-a", "TextEdit", "/tmp/note.txt"}) {
		t.Errorf("openWithCommand = %q, want open -a TextEdit /tmp/note.txt", got)
	}
	if got := vscodeCommand("/tmp/main.go"); !slices.Equal(got, []string{"open", "-a", "Visual Studio Code", "/tmp/main.go"}) {
		t.Errorf("vscodeCommand = %q, want open -a Visual Studio Code /tmp/main.go", got)
	}
}

// LaunchServices names the apps that open a file, the one it would
// choose itself first: TextEdit for a note, on any Mac.
func TestApplicationsForPath(t *testing.T) {
	f := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(f, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	apps, err := applicationsForPath(f)
	if err != nil {
		t.Fatalf("applicationsForPath: %v", err)
	}
	// An empty list is a quiet miss of LaunchServices' index, not a
	// failure: on a machine whose index is still cold there is nothing
	// to hold the test to.
	if len(apps) == 0 {
		t.Skip("LaunchServices named no app")
	}
	for _, app := range apps {
		if !strings.Contains(app.Name, "TextEdit") {
			continue
		}
		if !strings.HasSuffix(app.Path, ".app") {
			t.Errorf("TextEdit's entry is at %q, want a bundle", app.Path)
		}
		return
	}
	t.Errorf("no TextEdit among the %d apps named for a note: %v", len(apps), apps)
}

// A right click on a file of the tree opens it in an app: the system's
// own, VS Code, or one of the apps the system names.
func TestFilesOpenWithMenu(t *testing.T) {
	a := testApp(t)
	s := &a.st.Sessions[0]
	a.prefs.Side = sideFiles
	a.local().cl = &client.Client{}
	s.Panes[0].ProcessDir = "/tmp/demo"
	pane, root := a.filesRoot(s)
	a.tree(pane, root).dirs[root] = &fileDir{paths: []string{"/tmp/demo/main.go"}}
	tt := ui.NewTester(a.view, 1280, 720)
	tt.Frame()
	if err := tt.RightClick("main.go"); err != nil {
		t.Fatal(err)
	}
	want := []string{locales.CommonOpen(), locales.FilesOpenVSCode(), locales.FilesChooseApp()}
	if got := tt.Menu(); !slices.Equal(got, want) {
		t.Fatalf("the file's menu is %q, want %q", got, want)
	}
	// Choose App holds the apps the system names, not a command of its
	// own.
	if err := tt.ChooseMenuItem(locales.FilesChooseApp()); err == nil || !strings.Contains(err.Error(), "submenu") {
		t.Errorf("Choose App is not a submenu: %v", err)
	}
	tt.CloseMenu()
}

// A remote host's file has no menu to open it with: its path is not one
// of this machine, until the spec's cache step brings the file here.
func TestFilesOpenWithRemoteNone(t *testing.T) {
	a := testApp(t)
	s := &a.st.Sessions[0]
	a.prefs.Side = sideFiles
	h := &host{name: "remote", conn: connReady, cl: &client.Client{}, loaded: true}
	a.hosts = append(a.hosts, h)
	a.owner = map[string]*host{"s1": h, "p1": h, "a": h, "b": h}
	s.Panes[0].ProcessDir = "/tmp/demo"
	pane, root := a.filesRoot(s)
	a.tree(pane, root).dirs[root] = &fileDir{paths: []string{"/tmp/demo/main.go"}}
	tt := ui.NewTester(a.view, 1280, 720)
	tt.Frame()
	if err := tt.RightClick("main.go"); err != nil {
		t.Fatal(err)
	}
	if menu := tt.Menu(); menu != nil {
		t.Errorf("a remote file's menu is %q, want none", menu)
	}
}
