package actions

import (
	"runtime"
	"testing"
)

func TestFilesToggle(t *testing.T) {
	if got := pcKeys["files.toggle"]; got != "Ctrl+Alt+F" {
		t.Errorf(`pcKeys["files.toggle"] = %q, want "Ctrl+Alt+F"`, got)
	}
	files, ok := Find("files.toggle")
	if !ok {
		t.Fatal("no files.toggle action")
	}
	// init writes the PC keys over All on Windows and Linux.
	want := "Cmd+Shift+F"
	if runtime.GOOS != "darwin" {
		want = pcKeys["files.toggle"]
	}
	if files.Keys != want {
		t.Errorf("files.toggle Keys = %q, want %q", files.Keys, want)
	}
	// No other action may share the toggle's keys: find.open holds
	// Ctrl+Shift+F on the PC, pane.restart Shift+R everywhere.
	for _, a := range All {
		if a.ID != files.ID && a.Keys == files.Keys {
			t.Errorf("%s shares keys %q with files.toggle", a.ID, a.Keys)
		}
	}
	for id, k := range pcKeys {
		if id != files.ID && k == pcKeys["files.toggle"] {
			t.Errorf("%s shares PC keys %q with files.toggle", id, k)
		}
	}
}
