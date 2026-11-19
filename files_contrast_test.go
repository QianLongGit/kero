package main

import (
	"image/color"
	"math"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"kero/internal/proto"
)

// The tree's list draws its chosen row in the theme's accent
// (ListState.Selected), which the row's own text colors must read on:
// pal.text says little on a light accent, and less with the 55% opacity
// an ignored row takes. These tests sample the pixels a chosen row
// paints and hold its text to a contrast the eye reads.

// relLum is a color's relative luminance, as WCAG defines it.
func relLum(c color.RGBA) float64 {
	lin := func(v uint8) float64 {
		x := float64(v) / 255
		if x <= 0.04045 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.R) + 0.7152*lin(c.G) + 0.0722*lin(c.B)
}

// contrastRatio is how well a reads on b, as WCAG defines it.
func contrastRatio(a, b color.RGBA) float64 {
	la, lb := relLum(a), relLum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// rowColors samples a row of the Files tree within the text r's box: the
// fill the row paints there — the box's most common color, its glyphs
// taking less of it than the fill leaves — and the pixel of the glyphs
// farthest from that fill, of the colors that paint at least three
// pixels, one alone being antialiasing's stray. The sample keeps to the
// text's first stretch, whose box runs past it to the scroll bar at the
// list's edge.
func rowColors(t *ui.Tester, r ui.Rect) (text, bg color.RGBA) {
	img := t.Image()
	x1 := int(r.X) + min(int(r.W), 100)
	counts := map[color.RGBA]int{}
	for x := int(r.X); x < x1; x++ {
		for y := int(r.Y); y < int(r.Y+r.H); y++ {
			counts[img.RGBAAt(x, y)]++
		}
	}
	for c, n := range counts {
		if n > counts[bg] {
			bg = c
		}
	}
	far := func(c color.RGBA) float64 {
		dr := float64(int(c.R) - int(bg.R))
		dg := float64(int(c.G) - int(bg.G))
		db := float64(int(c.B) - int(bg.B))
		return math.Sqrt(dr*dr + dg*dg + db*db)
	}
	best := -1.0
	for c, n := range counts {
		if n < 3 {
			continue
		}
		if d := far(c); d > best {
			best, text = d, c
		}
	}
	return text, bg
}

// A row the tree's list chooses shows the accent under its text, and the
// text reads on it — an ignored row's as well, whose fade stands down
// once chosen. Both appearances, light and dark.
func TestFilesChosenRowContrast(t *testing.T) {
	for _, dark := range []bool{false, true} {
		a := testApp(t)
		s := &a.st.Sessions[0]
		g := &proto.GitInfo{Root: "/tmp/demo", Branch: "main", Default: "main"}
		s.Panes[0].Git, s.Panes[1].Git = g, g
		s.Panes[0].Dir = g.Root
		a.prefs.Side = sideFiles
		h := a.hostOf(s.ID)
		a.side.files.dirs = map[string]*dirList{
			filesKey(h, "/tmp/demo"): {entries: []proto.FSEntry{
				{Name: "main.go"},
				{Name: "sample.log", Ignored: true},
				{Name: "bad", Dir: true},
			}},
			filesKey(h, "/tmp/demo/bad"): {err: "cannot read"}, // its row, unfolded
		}
		a.side.files.open = map[string]bool{filesKey(h, "/tmp/demo/bad"): true}
		tt := ui.NewTester(a.view, 1280, 720)
		tt.SetDark(dark)
		tt.Frame()
		tt.Advance(time.Second)
		tt.Frame()

		// Sanity: an unchosen row's text paints at all, and reads on the
		// wash under it — else these samples say nothing.
		r, ok := tt.Find("main.go")
		if !ok {
			t.Fatalf("dark=%v: no main.go; texts %q", dark, tt.Texts())
		}
		text, bg := rowColors(tt, r)
		if c := contrastRatio(text, bg); c < 4 {
			t.Fatalf("dark=%v: an unchosen row's text reads %.2f on the wash; the tester draws no glyphs", dark, c)
		}

		for _, name := range []string{"main.go", "sample.log", "cannot read"} {
			if err := tt.Click(name); err != nil {
				t.Fatal(err)
			}
			tt.Frame()
			r, ok := tt.Find(name)
			if !ok {
				t.Fatalf("dark=%v: no %q; texts %q", dark, name, tt.Texts())
			}
			text, bg := rowColors(tt, r)
			if c := contrastRatio(text, bg); c < 4 {
				t.Errorf("dark=%v: the chosen row %q's text reads %.2f on the accent (text %v on %v), want ≥ 4", dark, name, c, text, bg)
			}
		}
	}
}
