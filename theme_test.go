package main

import (
	"image/color"
	"testing"

	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"
)

// onAccent is the color a chosen row's text takes on the theme's accent,
// on every theme and in both appearances. Ghostty themes bring cursors
// of any lightness, so the pairing holds on far ones too.
func TestOnAccent(t *testing.T) {
	contrast := func(a, b ui.Color) float64 {
		return contrastRatio(colorRGBA(a), colorRGBA(b))
	}
	for id := range washTable {
		for _, dark := range []bool{false, true} {
			pal := keroLook(id, dark).pal
			if c := contrast(onAccent(pal), pal.accent); c < 4 {
				t.Errorf("%s dark=%v: onAccent %v reads %.2f on the accent %v, want ≥ 4", id, dark, onAccent(pal), c, pal.accent)
			}
		}
	}
	// Ghostty themes bring cursors of any lightness, paired with a card
	// of their own: a deep accent takes white, a light one on a light
	// card takes the theme's own dark text, the one candidate a light
	// card leaves standing.
	for _, l := range []float32{0.3, 0.45} {
		for _, dark := range []bool{false, true} {
			pal := keroLook("paper", dark).pal
			pal.accent = ui.Oklch(l, 0.12, 315)
			if c := contrast(onAccent(pal), pal.accent); c < 4 {
				t.Errorf("an accent of lightness %.2f (dark=%v): onAccent reads %.2f, want ≥ 4", l, dark, c)
			}
		}
	}
	for _, l := range []float32{0.72, 0.9} {
		for _, dark := range []bool{false, true} {
			pal := keroLook("paper", dark).pal
			pal.accent = ui.Oklch(l, 0.12, 315)
			if c := contrast(onAccent(pal), pal.accent); c < 4 {
				t.Errorf("an accent of lightness %.2f (dark=%v): onAccent reads %.2f, want ≥ 4", l, dark, c)
			}
		}
	}
	// Every Ghostty theme there is, as its own card and cursor meet the
	// accent: no pairing of theirs may fall below what the eye reads.
	for _, name := range terminal.GhosttyThemes() {
		pal := lookNamed(name, false).pal
		if c := contrast(onAccent(pal), pal.accent); c < 4 {
			t.Errorf("the Ghostty theme %q: onAccent reads %.2f on the accent %v, want ≥ 4", name, c, pal.accent)
		}
	}
}

// colorRGBA is ui.Color as color.RGBA, for the contrast helpers.
func colorRGBA(c ui.Color) color.RGBA {
	return color.RGBA{R: c.R, G: c.G, B: c.B, A: c.A}
}
