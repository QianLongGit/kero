package cli

import "testing"

// TestTUIInputParse feeds the parser batches a phone SSH connection could
// realistically produce — single runes, whole sequences, and sequences
// torn across reads — and checks the events each individual feed returns,
// so buffering is asserted batch by batch, not just in the aggregate.
func TestTUIInputParse(t *testing.T) {
	tests := []struct {
		name  string
		feeds []string
		wants [][]tuiEvent
	}{
		{
			name:  "printable rune",
			feeds: []string{"q"},
			wants: [][]tuiEvent{{{Key: tuiKey{Rune: 'q'}}}},
		},
		{
			name:  "arrow down",
			feeds: []string{"\x1b[B"},
			wants: [][]tuiEvent{{{Key: tuiKey{Name: "down"}}}},
		},
		{
			name:  "sgr click",
			feeds: []string{"\x1b[<0;5;2M"},
			wants: [][]tuiEvent{{{Mouse: tuiMouse{Col: 5, Row: 2}, IsMouse: true}}},
		},
		{
			name:  "wheel up",
			feeds: []string{"\x1b[<64;1;1M"},
			wants: [][]tuiEvent{{{Mouse: tuiMouse{Col: 1, Row: 1, Wheel: -1}, IsMouse: true}}},
		},
		{
			name:  "wheel down",
			feeds: []string{"\x1b[<65;1;1M"},
			wants: [][]tuiEvent{{{Mouse: tuiMouse{Col: 1, Row: 1, Wheel: 1}, IsMouse: true}}},
		},
		{
			name:  "torn escape sequence",
			feeds: []string{"\x1b", "[<0;7;3M"},
			wants: [][]tuiEvent{
				nil,
				{{Mouse: tuiMouse{Col: 7, Row: 3}, IsMouse: true}},
			},
		},
		{
			name:  "torn utf-8 rune",
			feeds: []string{"\xe4\xbd", "\xa0"},
			wants: [][]tuiEvent{
				nil,
				{{Key: tuiKey{Rune: '你'}}},
			},
		},
		{
			name:  "digit",
			feeds: []string{"3"},
			wants: [][]tuiEvent{{{Key: tuiKey{Digit: 3}}}},
		},
		{
			name:  "enter from carriage return",
			feeds: []string{"\r"},
			wants: [][]tuiEvent{{{Key: tuiKey{Name: "enter"}}}},
		},
		{
			name:  "enter from line feed",
			feeds: []string{"\n"},
			wants: [][]tuiEvent{{{Key: tuiKey{Name: "enter"}}}},
		},
		{
			name:  "tab",
			feeds: []string{"\x09"},
			wants: [][]tuiEvent{{{Key: tuiKey{Name: "tab"}}}},
		},
		{
			name:  "backspace",
			feeds: []string{"\x7f"},
			wants: [][]tuiEvent{{{Key: tuiKey{Name: "backspace"}}}},
		},
		{
			name:  "ctrl+w",
			feeds: []string{"\x17"},
			wants: [][]tuiEvent{{{Key: tuiKey{Name: "ctrl+w"}}}},
		},
		{
			name:  "ctrl+u",
			feeds: []string{"\x15"},
			wants: [][]tuiEvent{{{Key: tuiKey{Name: "ctrl+u"}}}},
		},
		{
			// A lone ESC cannot be told from a torn sequence's first byte,
			// so it buffers; only the next byte deciding it starts no
			// sequence proves the user pressed Escape.
			name:  "escape before a plain key",
			feeds: []string{"\x1b", "z"},
			wants: [][]tuiEvent{
				nil,
				{
					{Key: tuiKey{Name: "escape"}},
					{Key: tuiKey{Rune: 'z'}},
				},
			},
		},
		{
			name:  "mouse release dropped",
			feeds: []string{"\x1b[<0;5;2m"},
			wants: [][]tuiEvent{nil},
		},
		{
			name:  "unknown csi swallowed",
			feeds: []string{"\x1b[200~x"},
			wants: [][]tuiEvent{{{Key: tuiKey{Rune: 'x'}}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &inputParser{}
			for i, feed := range tt.feeds {
				got, want := p.feed([]byte(feed)), tt.wants[i]
				if len(got) != len(want) {
					t.Fatalf("feed %d (%q): got %d events %+v, want %d", i, feed, len(got), got, len(want))
				}
				for j := range got {
					if got[j] != want[j] {
						t.Errorf("feed %d (%q): event %d = %+v, want %+v", i, feed, j, got[j], want[j])
					}
				}
			}
		})
	}
}

// TestTUIInputEditor drives the line editor through the keys the filter
// bar actually receives: typing, cursor moves, deletions, and the keys
// that are not the editor's to take.
func TestTUIInputEditor(t *testing.T) {
	typeKeys := func(e *lineEditor, ks ...tuiKey) {
		t.Helper()
		for _, k := range ks {
			if !e.key(k) {
				t.Fatalf("key %+v reported not an editing key", k)
			}
		}
	}
	typeText := func(e *lineEditor, s string) {
		t.Helper()
		for _, r := range s {
			typeKeys(e, tuiKey{Rune: r})
		}
	}

	t.Run("insert at cursor", func(t *testing.T) {
		e := &lineEditor{}
		typeKeys(e, tuiKey{Rune: 'a'}, tuiKey{Rune: 'b'}, tuiKey{Name: "left"}, tuiKey{Rune: 'X'})
		if got, want := e.text(), "aXb"; got != want {
			t.Errorf("text = %q, want %q", got, want)
		}
		// The cursor sits after the inserted X, between it and the b.
		if e.pos != 2 {
			t.Errorf("pos = %d, want 2", e.pos)
		}
	})

	// Digits are split out by the parser for the list's shortcut keys;
	// inside the editor they are just text again.
	t.Run("digits are text here", func(t *testing.T) {
		e := &lineEditor{}
		typeKeys(e, tuiKey{Digit: 3})
		if got, want := e.text(), "3"; got != want {
			t.Errorf("text = %q, want %q", got, want)
		}
	})

	t.Run("backspace", func(t *testing.T) {
		e := &lineEditor{}
		typeKeys(e, tuiKey{Rune: 'a'}, tuiKey{Rune: 'b'}, tuiKey{Name: "backspace"})
		if got, want := e.text(), "a"; got != want {
			t.Errorf("text = %q, want %q", got, want)
		}
		if e.pos != 1 {
			t.Errorf("pos = %d, want 1", e.pos)
		}
	})

	t.Run("right stops at the end", func(t *testing.T) {
		e := &lineEditor{}
		typeKeys(e, tuiKey{Rune: 'a'}, tuiKey{Name: "right"}, tuiKey{Name: "right"})
		if e.pos != 1 {
			t.Errorf("pos = %d, want 1", e.pos)
		}
	})

	t.Run("ctrl+w deletes the word left", func(t *testing.T) {
		e := &lineEditor{}
		typeText(e, "foo bar")
		typeKeys(e, tuiKey{Name: "ctrl+w"})
		if got, want := e.text(), "foo "; got != want {
			t.Errorf("text = %q, want %q", got, want)
		}
		if e.pos != 4 {
			t.Errorf("pos = %d, want 4", e.pos)
		}
	})

	t.Run("ctrl+u clears", func(t *testing.T) {
		e := &lineEditor{}
		typeText(e, "left over")
		typeKeys(e, tuiKey{Name: "ctrl+u"})
		if got := e.text(); got != "" {
			t.Errorf("text = %q, want empty", got)
		}
		if e.pos != 0 {
			t.Errorf("pos = %d, want 0", e.pos)
		}
	})

	t.Run("non-editing keys leave state untouched", func(t *testing.T) {
		e := &lineEditor{}
		typeText(e, "hi")
		for _, k := range []tuiKey{{Name: "enter"}, {Name: "escape"}, {Name: "up"}, {Name: "down"}, {Name: "tab"}} {
			if e.key(k) {
				t.Fatalf("key %+v reported editing", k)
			}
			if e.text() != "hi" || e.pos != 2 {
				t.Fatalf("state moved on %+v: %q at %d", k, e.text(), e.pos)
			}
		}
	})
}
