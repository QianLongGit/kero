package main

import (
	"image/color"
	"testing"
	"time"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// A named terminal theme's colors are tuned to its own background, so a
// pane's body paints that background under its transparent terminal; Kero's
// themes sit on the card's color already, and keep showing it.
func TestPaneTerminalThemeBackground(t *testing.T) {
	th, err := terminal.GhosttyTheme("Tomorrow Night Blue")
	if err != nil {
		t.Fatal(err)
	}
	// What shows through the transparent terminal: a pixel of the focused
	// pane's body, below its header, well inside the card.
	bodyPixel := func(tt *ui.Tester) color.RGBA {
		title, ok := tt.Find("fish") // pane a's title
		if !ok {
			t.Fatalf("no pane a title; texts %q", tt.Texts())
		}
		return tt.Image().RGBAAt(int(title.X+80), int(title.Y+100))
	}
	near := func(got color.RGBA, want ui.Color) bool {
		return abs32(float32(got.R)-float32(want.R)) <= 2 &&
			abs32(float32(got.G)-float32(want.G)) <= 2 &&
			abs32(float32(got.B)-float32(want.B)) <= 2
	}
	rendered := func(a *App) *ui.Tester {
		tt := ui.NewTester(a.view, 1100, 640)
		tt.Frame()
		tt.Advance(time.Second) // the card's background eases in
		tt.Frame()
		return tt
	}

	t.Run("named theme", func(t *testing.T) {
		a := testApp(t)
		a.set.LightTheme = "Tomorrow Night Blue"
		if got := bodyPixel(rendered(a)); !near(got, th.Background) {
			t.Errorf("the body shows %v, want the theme's background %v", got, th.Background)
		}
	})
	t.Run("named theme, dark window", func(t *testing.T) {
		a := testApp(t)
		a.set.DarkTheme = "Tomorrow Night Blue"
		tt := ui.NewTester(a.view, 1100, 640)
		tt.SetDark(true)
		tt.Frame()
		tt.Advance(time.Second)
		tt.Frame()
		if got := bodyPixel(tt); !near(got, th.Background) {
			t.Errorf("the body shows %v, want the theme's background %v", got, th.Background)
		}
	})
	t.Run("kero theme", func(t *testing.T) {
		a := testApp(t)
		a.set.LightTheme = "kero"
		if got := bodyPixel(rendered(a)); !near(got, lightPalette.card) {
			t.Errorf("the body shows %v, want the card's color %v", got, lightPalette.card)
		}
	})
}
