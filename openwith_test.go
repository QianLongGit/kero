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
// runs them: on macOS open asks LaunchServices for an app by name, and
// VS Code falls back to its CLI, which elsewhere is the one.
func TestOpenWithCommand(t *testing.T) {
	if got := openWithCommand("TextEdit", "/tmp/note.txt"); !slices.Equal(got, []string{"open", "-a", "TextEdit", "/tmp/note.txt"}) {
		t.Errorf("openWithCommand = %q, want open -a TextEdit /tmp/note.txt", got)
	}
	want := [][]string{{"open", "-a", "Visual Studio Code", "/tmp/main.go"}, {"code", "/tmp/main.go"}}
	got := vscodeCommands("/tmp/main.go")
	if len(got) != len(want) {
		t.Fatalf("vscodeCommands = %q, want the app and then its CLI", got)
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Errorf("vscodeCommands[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// appsFixture builds, under a fresh root, the shapes Applications
// folders hold: bundles at the top, a suite folder with a bundle one
// level down, a helper nested inside a bundle, a bundle three plain
// levels down, and a plain file that ends in .app all the same.
func appsFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, parts := range [][]string{
		{"Alpha.app"},
		{"Alpha.app", "Helper.app"},
		{"Zealand.app"},
		{"Suite", "Photoshop.app"},
		{"Suite", "Presets"},
		{"deep", "deeper", "Stray.app"},
	} {
		if err := os.MkdirAll(filepath.Join(append([]string{root}, parts...)...), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "readme.app"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// The walk takes the bundles of the top level and of a suite folder one
// level in, and nothing else: not a bundle's own insides, not a stray
// past the depth, not a file however it is named. A folder that is not
// there holds no apps, quietly.
func TestAppsInDir(t *testing.T) {
	root := appsFixture(t)
	for _, tc := range []struct {
		name  string
		depth int
		want  []AppEntry
	}{
		{
			name:  "two levels",
			depth: 2,
			want: []AppEntry{
				{Name: "Alpha", Path: filepath.Join(root, "Alpha.app")},
				{Name: "Photoshop", Path: filepath.Join(root, "Suite", "Photoshop.app")},
				{Name: "Zealand", Path: filepath.Join(root, "Zealand.app")},
			},
		},
		{
			name:  "one level",
			depth: 1,
			want: []AppEntry{
				{Name: "Alpha", Path: filepath.Join(root, "Alpha.app")},
				{Name: "Zealand", Path: filepath.Join(root, "Zealand.app")},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := appsInDir(root, tc.depth)
			slices.SortFunc(got, func(a, b AppEntry) int { return strings.Compare(a.Path, b.Path) })
			if !slices.Equal(got, tc.want) {
				t.Errorf("appsInDir = %v, want %v", got, tc.want)
			}
		})
	}
	if got := appsInDir(filepath.Join(root, "not there"), 2); got != nil {
		t.Errorf("appsInDir of a missing folder = %v, want none", got)
	}
}

// Merged, the apps of several folders come by name with the case
// folded away and by path where names agree: the same bundle twice is
// one entry, the same name at two paths two, as the system holds it.
func TestAppsInDirs(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"1/ALPHA.app", "1/beta.app", "1/Same.app", "2/Same.app"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ent := func(p string) AppEntry {
		return AppEntry{Name: strings.TrimSuffix(filepath.Base(p), ".app"), Path: p}
	}
	alpha := ent(filepath.Join(root, "1", "ALPHA.app"))
	beta := ent(filepath.Join(root, "1", "beta.app"))
	same1 := ent(filepath.Join(root, "1", "Same.app"))
	same2 := ent(filepath.Join(root, "2", "Same.app"))
	for _, tc := range []struct {
		name string
		dirs []string
		want []AppEntry
	}{
		{"sorted across folders, ties by path", []string{filepath.Join(root, "2"), filepath.Join(root, "1")}, []AppEntry{alpha, beta, same1, same2}},
		{"the same folder twice is one list", []string{filepath.Join(root, "1"), filepath.Join(root, "1")}, []AppEntry{alpha, beta, same1}},
		{"a missing folder adds nothing", []string{filepath.Join(root, "none"), filepath.Join(root, "2")}, []AppEntry{same2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := appsInDirs(tc.dirs...); !slices.Equal(got, tc.want) {
				t.Errorf("appsInDirs = %v, want %v", got, tc.want)
			}
		})
	}
}

// The system's own Applications folder holds the Calculator on any
// Mac (the Finder itself it does not: the sealed system keeps it in
// CoreServices): the one live read the menu makes, skipped where the
// folder is not.
func TestInstalledApps(t *testing.T) {
	if fi, err := os.Stat("/System/Applications"); err != nil || !fi.IsDir() {
		t.Skip("no /System/Applications on this machine")
	}
	for _, app := range installedApps() {
		if app.Name != "Calculator" {
			continue
		}
		if !strings.HasSuffix(app.Path, "Calculator.app") {
			t.Errorf("Calculator is at %q, want a bundle's path", app.Path)
		}
		return
	}
	t.Error("Calculator is not among the installed apps")
}

// A right click on a file of the tree opens it in an app: the system's
// own, VS Code, or any app installed on the system.
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
	// Choose App holds the apps installed on the system, not a command
	// of its own.
	if err := tt.ChooseMenuItem(locales.FilesChooseApp()); err == nil || !strings.Contains(err.Error(), "submenu") {
		t.Errorf("Choose App is not a submenu: %v", err)
	}
	tt.CloseMenu()
}
