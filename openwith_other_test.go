//go:build !darwin

package main

import (
	"slices"
	"testing"

	"github.com/egoist/mygo/ui"

	"kero/internal/client"

	"kero/locales"
)

// A right click on a file of the tree opens it in an app: the system's
// own, or VS Code. No system but macOS names the apps that open a file,
// so there is no submenu of them to choose.
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
	want := []string{locales.CommonOpen(), locales.FilesOpenVSCode(),
		"-", locales.FilesCopy(), locales.FilesPaste(), "-", locales.FilesDelete()}
	if got := tt.Menu(); !slices.Equal(got, want) {
		t.Fatalf("the file's menu is %q, want %q", got, want)
	}
}
