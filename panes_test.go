package main

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"kero/locales"
)

// A pane header's first button hides the side panel and shows it again,
// with the view it showed last, as its tooltip says which it will do.
func TestPaneHeaderSideToggle(t *testing.T) {
	a := testApp(t)
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()

	// The panel closed, a click opens Files, the view with no memory.
	if err := tt.Click(locales.SideShow()); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.prefs.Side != sideFiles {
		t.Fatalf("a click on the toggle opened %q, want Files", a.prefs.Side)
	}

	// The panel open, the tooltip flips, and a click hides it again.
	if err := tt.Click(locales.SideHide()); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.prefs.Side != "" {
		t.Fatalf("a click on the toggle left the panel on %q", a.prefs.Side)
	}

	// The view it showed comes back: switch to Changes first.
	a.showSide(sideChanges)
	a.toggleSidePanel()
	tt.Frame()
	if err := tt.Click(locales.SideShow()); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.prefs.Side != sideChanges {
		t.Fatalf("a click on the toggle opened %q, want the last view, Changes", a.prefs.Side)
	}
}
