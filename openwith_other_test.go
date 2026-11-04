//go:build !darwin

package main

import (
	"slices"
	"testing"

	"github.com/egoist/mygo/ui"

	"kero/internal/client"
	"kero/internal/proto"

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
	// The tree's folder listed, counted as the tab's own (gen): the
	// listing sits as it is, asked of no one.
	h := a.hostOf(s.Panes[0].ID)
	a.side.files.dirs = map[string]*dirList{
		filesKey(h, "/tmp/demo"): {entries: []proto.FSEntry{{Name: "main.go"}}, gen: 1},
	}
	tt := ui.NewTester(a.view, 1280, 720)
	tt.Frame()
	if err := tt.RightClick("main.go"); err != nil {
		t.Fatal(err)
	}
	want := []string{locales.CommonOpen(), locales.FilesOpenVSCode(),
		"-", locales.FilesCopy(), locales.FilesPaste(), locales.FilesCopyPath(), locales.FilesCopyRelativePath(),
		"-", locales.FilesNewFile(), locales.FilesNewFolder(), "-", locales.FilesRename(), locales.FilesDeleteItem()}
	if got := tt.Menu(); !slices.Equal(got, want) {
		t.Fatalf("the file's menu is %q, want %q", got, want)
	}
}
