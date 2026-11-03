package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"kero/internal/proto"
)

// tuiFix is the working fixture: a busy three-pane session over an
// exited single-pane one, laid out as the list shows them —
// [s1, s1/p1, s1/p2, s1/p3, s2] with s1 focused on p2.
func tuiFix() proto.State {
	return proto.State{Sessions: []proto.Session{
		{
			ID:    "s1",
			Name:  "build",
			Panes: []proto.Pane{{ID: "p1", Title: "shell"}, {ID: "p2", Title: "vim"}, {ID: "p3", Title: "tests"}},
			Focus: "p2",
		},
		{
			ID:    "s2",
			Name:  "old",
			Pane:  proto.Pane{Status: proto.Exited, Title: "done"}, // the lead pane's fields ride the session
			Panes: []proto.Pane{{ID: "p4", Title: "done"}},
		},
	}}
}

// tuiMany is n idle single-pane sessions s01..sNN: one row each, in id
// order — the viewport scenario's tall, flat list.
func tuiMany(n int) proto.State {
	st := proto.State{}
	for i := 1; i <= n; i++ {
		id := "s" + string(rune('0'+i/10)) + string(rune('0'+i%10))
		st.Sessions = append(st.Sessions, proto.Session{
			ID:    id,
			Panes: []proto.Pane{{ID: "p" + id[1:]}},
		})
	}
	return st
}

func keyRune(r rune) tuiEvent   { return tuiEvent{Key: tuiKey{Rune: r}} }
func keyName(n string) tuiEvent { return tuiEvent{Key: tuiKey{Name: n}} }
func keyDigit(d int) tuiEvent   { return tuiEvent{Key: tuiKey{Digit: d}} }
func clickAt(row int) tuiEvent  { return tuiEvent{Mouse: tuiMouse{Col: 3, Row: row}, IsMouse: true} }
func wheel(d int) tuiEvent      { return tuiEvent{Mouse: tuiMouse{Col: 3, Row: 1, Wheel: d}, IsMouse: true} }

// stepOK runs one event through the machine, failing the test on any
// error: every scenario but the error one expects the step to succeed.
func stepOK(t *testing.T, m tuiModel, ev tuiEvent, st proto.State, h int) (tuiModel, *tuiPending) {
	t.Helper()
	m, p, err := tuiStep(m, ev, st, h)
	if err != nil {
		t.Fatalf("tuiStep(%+v): %v", ev, err)
	}
	return m, p
}

// wantPending asserts the pending the step fired, whole.
func wantPending(t *testing.T, p *tuiPending, want tuiPending) {
	t.Helper()
	if p == nil {
		t.Fatalf("no pending fired; want %+v", want)
	}
	if *p != want {
		t.Fatalf("pending = %+v; want %+v", *p, want)
	}
}

func TestTUIStep(t *testing.T) {
	st := tuiFix()

	t.Run("(a) enter on session row attaches the focused pane", func(t *testing.T) {
		m, p := stepOK(t, tuiModel{Mode: tuiList}, keyName("enter"), st, 10)
		wantPending(t, p, tuiPending{Kind: tuiAttach, SessionID: "s1", Pane: "p2"})
		if m.Cursor != 0 {
			t.Fatalf("cursor = %d; want 0", m.Cursor)
		}
	})

	t.Run("(b) enter on pane row attaches that pane", func(t *testing.T) {
		m, p := stepOK(t, tuiModel{Mode: tuiList, Cursor: 1}, keyName("enter"), st, 10)
		wantPending(t, p, tuiPending{Kind: tuiAttach, SessionID: "s1", Pane: "p1"})
		if m.Cursor != 1 {
			t.Fatalf("cursor = %d; want 1", m.Cursor)
		}
	})

	t.Run("(c) d on pane row confirms, y closes that pane", func(t *testing.T) {
		m, _ := stepOK(t, tuiModel{Mode: tuiList, Cursor: 1}, keyRune('d'), st, 10)
		if m.Mode != tuiConfirm || m.Action != tuiClose {
			t.Fatalf("mode/action = %v/%v; want confirm/close", m.Mode, m.Action)
		}
		if m.Target != (tuiRow{Session: "s1", Pane: "p1", Indent: true}) {
			t.Fatalf("target = %+v; want s1/p1", m.Target)
		}
		m, p := stepOK(t, m, keyRune('y'), st, 10)
		wantPending(t, p, tuiPending{Kind: tuiClose, SessionID: "s1", Pane: "p1"})
		if m.Mode != tuiList {
			t.Fatalf("mode = %v; want list", m.Mode)
		}
	})

	t.Run("(d) d on session row closes the whole session", func(t *testing.T) {
		m, _ := stepOK(t, tuiModel{Mode: tuiList, Cursor: 0}, keyRune('d'), st, 10)
		if m.Target != (tuiRow{Session: "s1"}) {
			t.Fatalf("target = %+v; want s1 session row", m.Target)
		}
		_, p := stepOK(t, m, keyRune('y'), st, 10)
		wantPending(t, p, tuiPending{Kind: tuiClose, SessionID: "s1"})
	})

	t.Run("(e) n opens an empty prompt, enter runs the text", func(t *testing.T) {
		m, p := stepOK(t, tuiModel{Mode: tuiList}, keyRune('n'), st, 10)
		if p != nil {
			t.Fatalf("pending = %+v; want none", p)
		}
		if m.Mode != tuiPrompt || m.Action != tuiNew {
			t.Fatalf("mode/action = %v/%v; want prompt/new", m.Mode, m.Action)
		}
		if m.Editor.text() != "" {
			t.Fatalf("editor = %q; want empty", m.Editor.text())
		}
		for _, r := range "claude" {
			m, _ = stepOK(t, m, keyRune(r), st, 10)
		}
		m, p = stepOK(t, m, keyName("enter"), st, 10)
		wantPending(t, p, tuiPending{Kind: tuiNew, Text: "claude"})
		if m.Mode != tuiList {
			t.Fatalf("mode = %v; want list", m.Mode)
		}
	})

	t.Run("(f) r seeds the prompt with the current name, rename carries it", func(t *testing.T) {
		m, _ := stepOK(t, tuiModel{Mode: tuiList, Cursor: 0}, keyRune('r'), st, 10)
		if m.Mode != tuiPrompt || m.Action != tuiRename {
			t.Fatalf("mode/action = %v/%v; want prompt/rename", m.Mode, m.Action)
		}
		if got := m.Editor.text(); got != "build" {
			t.Fatalf("editor = %q; want %q", got, "build")
		}
		if m.Editor.pos != len("build") {
			t.Fatalf("pos = %d; want %d", m.Editor.pos, len("build"))
		}
		m, _ = stepOK(t, m, keyRune('x'), st, 10) // buildx
		_, p := stepOK(t, m, keyName("enter"), st, 10)
		wantPending(t, p, tuiPending{Kind: tuiRename, SessionID: "s1", Text: "buildx"})
	})

	t.Run("r on a pane row stays session-level", func(t *testing.T) {
		m, _ := stepOK(t, tuiModel{Mode: tuiList, Cursor: 1}, keyRune('r'), st, 10)
		if m.Editor.text() != "build" {
			t.Fatalf("editor = %q; the pane row's session names the prompt", m.Editor.text())
		}
		_, p := stepOK(t, m, keyName("enter"), st, 10)
		wantPending(t, p, tuiPending{Kind: tuiRename, SessionID: "s1", Text: "build"})
	})

	t.Run("s splits at once: pane row its id, session row the focused pane", func(t *testing.T) {
		_, p := stepOK(t, tuiModel{Mode: tuiList, Cursor: 1}, keyRune('s'), st, 10)
		wantPending(t, p, tuiPending{Kind: tuiSplit, SessionID: "s1", Pane: "p1"})
		_, p = stepOK(t, tuiModel{Mode: tuiList, Cursor: 0}, keyRune('s'), st, 10)
		wantPending(t, p, tuiPending{Kind: tuiSplit, SessionID: "s1", Pane: "p2"})
		_, p = stepOK(t, tuiModel{Mode: tuiList, Cursor: 4}, keyRune('s'), st, 10)
		wantPending(t, p, tuiPending{Kind: tuiSplit, SessionID: "s2", Pane: "p4"})
	})

	t.Run("(g) filter with no match: enter no-op, n opens, q quits", func(t *testing.T) {
		m, _ := stepOK(t, tuiModel{Mode: tuiList}, keyRune('/'), st, 10)
		for _, r := range "zzz" {
			m, _ = stepOK(t, m, keyRune(r), st, 10)
		}
		m, _ = stepOK(t, m, keyName("enter"), st, 10)
		if m.Filter != "zzz" || m.Mode != tuiList {
			t.Fatalf("filter/mode = %q/%v; want zzz/list", m.Filter, m.Mode)
		}
		if rows := visibleRows(st, m.Filter); len(rows) != 0 {
			t.Fatalf("%d rows survive filter zzz; want none", len(rows))
		}
		// Enter, arrows and row actions on the empty list are no-ops.
		for _, ev := range []tuiEvent{keyName("enter"), keyName("up"), keyName("down"), keyDigit(5), keyRune('d'), keyRune('r'), keyRune('s'), clickAt(1)} {
			m2, p := stepOK(t, m, ev, st, 10)
			if p != nil {
				t.Fatalf("pending %+v fired on empty list", p)
			}
			if m2.Cursor != 0 || m2.Mode != tuiList {
				t.Fatalf("empty-list %v moved cursor/mode to %d/%v", ev, m2.Cursor, m2.Mode)
			}
		}
		// n still opens the prompt, q still quits.
		mn, p := stepOK(t, m, keyRune('n'), st, 10)
		if p != nil || mn.Mode != tuiPrompt || mn.Action != tuiNew {
			t.Fatalf("n on empty list: %+v %v/%v", p, mn.Mode, mn.Action)
		}
		_, p = stepOK(t, m, keyRune('q'), st, 10)
		wantPending(t, p, tuiPending{Kind: tuiQuit})
	})

	t.Run("(h) in filter mode d types the letter, no close pending", func(t *testing.T) {
		m, _ := stepOK(t, tuiModel{Mode: tuiList}, keyRune('/'), st, 10)
		m, p := stepOK(t, m, keyRune('d'), st, 10)
		if p != nil {
			t.Fatalf("pending = %+v; want none", p)
		}
		if m.Mode != tuiFilter || m.Editor.text() != "d" {
			t.Fatalf("mode/text = %v/%q; want filter/d", m.Mode, m.Editor.text())
		}
	})

	t.Run("filter mode keeps keys to editing: up and q do nothing", func(t *testing.T) {
		m, _ := stepOK(t, tuiModel{Mode: tuiList}, keyRune('/'), st, 10)
		m, p := stepOK(t, m, keyName("up"), st, 10)
		if p != nil || m.Mode != tuiFilter || m.Cursor != 0 {
			t.Fatalf("up in filter mode: %+v %v/%d", p, m.Mode, m.Cursor)
		}
		_, p = stepOK(t, m, keyRune('q'), st, 10)
		if p != nil || m.Mode != tuiFilter {
			t.Fatalf("q in filter mode fired %+v in mode %v", p, m.Mode)
		}
	})

	t.Run("(i) digits move the cursor, each on its own", func(t *testing.T) {
		m, p := stepOK(t, tuiModel{Mode: tuiList}, keyDigit(2), st, 10)
		if p != nil || m.Cursor != 1 {
			t.Fatalf("digit 2: %+v cursor %d; want nil/1", p, m.Cursor)
		}
		m, p = stepOK(t, m, keyDigit(3), st, 10)
		if p != nil || m.Cursor != 2 {
			t.Fatalf("digit 3: %+v cursor %d; want nil/2", p, m.Cursor)
		}
		// A digit past the list's end clamps to the last row.
		m, p = stepOK(t, m, keyDigit(9), st, 10)
		if p != nil || m.Cursor != 4 {
			t.Fatalf("digit 9: %+v cursor %d; want nil/4", p, m.Cursor)
		}
	})

	t.Run("(j) wheel moves the cursor one row, wrapping; click maps to a row", func(t *testing.T) {
		m, _ := stepOK(t, tuiModel{Mode: tuiList}, wheel(1), st, 10)
		if m.Cursor != 1 {
			t.Fatalf("cursor = %d; want 1", m.Cursor)
		}
		m, _ = stepOK(t, m, wheel(1), st, 10)
		if m.Cursor != 2 {
			t.Fatalf("cursor = %d; want 2", m.Cursor)
		}
		m, _ = stepOK(t, m, wheel(-1), st, 10)
		if m.Cursor != 1 {
			t.Fatalf("cursor = %d; want 1", m.Cursor)
		}
		m, _ = stepOK(t, m, wheel(-1), st, 10)
		if m.Cursor != 0 {
			t.Fatalf("cursor = %d; want 0", m.Cursor)
		}
		m, _ = stepOK(t, m, wheel(-1), st, 10) // wraps under the top
		if m.Cursor != 4 {
			t.Fatalf("cursor = %d; want 4 (wrap)", m.Cursor)
		}
		// The layout contract: screen row 1 is the header, the list
		// starts at screen row 2. A click at screen row 2, offset 0,
		// hits list row 0; row 3 hits list row 1.
		m, p := stepOK(t, tuiModel{Mode: tuiList}, clickAt(2), st, 10)
		if m.Cursor != 0 {
			t.Fatalf("cursor = %d; want 0", m.Cursor)
		}
		wantPending(t, p, tuiPending{Kind: tuiAttach, SessionID: "s1", Pane: "p2"})
		m, p = stepOK(t, tuiModel{Mode: tuiList}, clickAt(3), st, 10)
		if m.Cursor != 1 {
			t.Fatalf("cursor = %d; want 1", m.Cursor)
		}
		wantPending(t, p, tuiPending{Kind: tuiAttach, SessionID: "s1", Pane: "p1"})
		// The header and rows past the list select nothing.
		for _, off := range []int{1, 8} {
			m, p = stepOK(t, tuiModel{Mode: tuiList, Cursor: 0}, clickAt(off), st, 10)
			if p != nil || m.Cursor != 0 {
				t.Fatalf("click off the list (row %d): %+v cursor %d", off, p, m.Cursor)
			}
		}
	})

	t.Run("(k) enter on the exited session attaches its pane", func(t *testing.T) {
		_, p := stepOK(t, tuiModel{Mode: tuiList, Cursor: 4}, keyName("enter"), st, 10)
		wantPending(t, p, tuiPending{Kind: tuiAttach, SessionID: "s2", Pane: "p4"})
	})

	t.Run("up/down wrap around the list", func(t *testing.T) {
		m, _ := stepOK(t, tuiModel{Mode: tuiList}, keyName("up"), st, 10)
		if m.Cursor != 4 {
			t.Fatalf("up from the top = %d; want 4 (wrap)", m.Cursor)
		}
		m, _ = stepOK(t, tuiModel{Mode: tuiList, Cursor: 4}, keyName("down"), st, 10)
		if m.Cursor != 0 {
			t.Fatalf("down from the bottom = %d; want 0 (wrap)", m.Cursor)
		}
	})

	t.Run("prompt escape cancels and leaves the editor behind", func(t *testing.T) {
		m, _ := stepOK(t, tuiModel{Mode: tuiList}, keyRune('n'), st, 10)
		for _, r := range "ab" {
			m, _ = stepOK(t, m, keyRune(r), st, 10)
		}
		m, p := stepOK(t, m, keyName("escape"), st, 10)
		if p != nil || m.Mode != tuiList {
			t.Fatalf("escape in prompt: %+v mode %v", p, m.Mode)
		}
		m, _ = stepOK(t, m, keyRune('n'), st, 10) // a fresh prompt starts empty
		if m.Editor.text() != "" {
			t.Fatalf("editor = %q; the cancelled text must not leak", m.Editor.text())
		}
	})

	t.Run("confirm ignores every key but y, n and escape", func(t *testing.T) {
		m, _ := stepOK(t, tuiModel{Mode: tuiList, Cursor: 0}, keyRune('d'), st, 10)
		for _, ev := range []tuiEvent{keyRune('x'), keyName("enter"), keyName("up"), keyDigit(3)} {
			m2, p := stepOK(t, m, ev, st, 10)
			if p != nil || m2.Mode != tuiConfirm {
				t.Fatalf("confirm %v: %+v mode %v", ev, p, m2.Mode)
			}
		}
		// n is the no the (y/n) question promises: back to the list,
		// nothing fired.
		m2, p := stepOK(t, m, keyRune('n'), st, 10)
		if p != nil || m2.Mode != tuiList {
			t.Fatalf("n in confirm: %+v mode %v; want no pending, list", p, m2.Mode)
		}
		m, _ = stepOK(t, m2, keyRune('d'), st, 10) // ask again for escape
		if m.Mode != tuiConfirm {
			t.Fatalf("d after n: mode %v; want confirm again", m.Mode)
		}
		m, p = stepOK(t, m, keyName("escape"), st, 10)
		if p != nil || m.Mode != tuiList {
			t.Fatalf("escape in confirm: %+v mode %v", p, m.Mode)
		}
	})

	t.Run("enter on a paneless session fails with errNotFound", func(t *testing.T) {
		empty := proto.State{Sessions: []proto.Session{{ID: "s9", Name: "ghost"}}}
		_, p, err := tuiStep(tuiModel{Mode: tuiList}, keyName("enter"), empty, 10)
		if p != nil {
			t.Fatalf("pending = %+v; want none", p)
		}
		if !errors.Is(err, errNotFound) {
			t.Fatalf("err = %v; want errNotFound", err)
		}
	})

	t.Run("(l) viewport: cursor drags the offset, clicks map through it", func(t *testing.T) {
		st := tuiMany(30)
		m, _ := stepOK(t, tuiModel{Mode: tuiList}, keyRune('z'), st, 8) // settle the held row
		for i := 0; i < 8; i++ {
			m, _ = stepOK(t, m, keyName("down"), st, 8)
		}
		if m.Cursor != 8 || m.Offset != 1 {
			t.Fatalf("cursor/offset = %d/%d; want 8/1 (dragged along)", m.Cursor, m.Offset)
		}
		m, _ = stepOK(t, m, keyName("up"), st, 8)
		if m.Cursor != 7 || m.Offset != 1 {
			t.Fatalf("cursor/offset = %d/%d; want 7/1 (held)", m.Cursor, m.Offset)
		}
		for i := 0; i < 7; i++ {
			m, _ = stepOK(t, m, keyName("up"), st, 8)
		}
		if m.Cursor != 0 || m.Offset != 0 {
			t.Fatalf("cursor/offset = %d/%d; want 0/0 (pulled back)", m.Cursor, m.Offset)
		}
		// Scroll to a deeper offset, then click every screen row 1..8.
		m, _ = stepOK(t, tuiModel{Mode: tuiList}, keyRune('z'), st, 8)
		for i := 0; i < 10; i++ {
			m, _ = stepOK(t, m, keyName("down"), st, 8)
		}
		if m.Cursor != 10 || m.Offset != 3 {
			t.Fatalf("cursor/offset = %d/%d; want 10/3", m.Cursor, m.Offset)
		}
		// Click every list-area screen row 2..height+1; the header
		// (row 1) and the prompt line (row height+2) select nothing.
		for row := 1; row <= 10; row++ {
			m2, p := stepOK(t, m, clickAt(row), st, 8)
			if row == 1 || row == 10 {
				if p != nil {
					t.Fatalf("click at non-list row %d: pending %+v; want none", row, p)
				}
				continue
			}
			if p == nil || p.Kind != tuiAttach {
				t.Fatalf("click at screen row %d: pending %+v; want attach", row, p)
			}
			if want := 3 + row - 2; m2.Cursor != want {
				t.Fatalf("click at screen row %d: cursor %d; want %d", row, m2.Cursor, want)
			}
		}
	})

	t.Run("(m) cursor holds its session across state changes", func(t *testing.T) {
		m, _ := stepOK(t, tuiModel{Mode: tuiList}, keyRune('z'), st, 10) // held: s1's session row

		// A session inserted ahead: s1 slides down one row.
		ins := tuiFix()
		ins.Sessions = append(ins.Sessions, proto.Session{ID: "s0", Panes: []proto.Pane{{ID: "p0"}}})
		m, _ = stepOK(t, m, keyRune('z'), ins, 10)
		if rows := visibleRows(ins, ""); rows[m.Cursor] != (tuiRow{Session: "s1"}) {
			t.Fatalf("after insert cursor sits on %+v; want s1's row", rows[m.Cursor])
		}

		// A reorder that puts the exited session first.
		re := tuiFix()
		re.Sessions[1].Activity = 99
		m, _ = stepOK(t, m, keyRune('z'), re, 10)
		if rows := visibleRows(re, ""); rows[m.Cursor] != (tuiRow{Session: "s1"}) {
			t.Fatalf("after reorder cursor sits on %+v; want s1's row", rows[m.Cursor])
		}

		// The held session gone: the cursor clamps into range.
		gone := tuiFix()
		gone.Sessions = gone.Sessions[1:] // only s2 left
		m, _ = stepOK(t, m, keyRune('z'), gone, 10)
		if rows := visibleRows(gone, ""); rows[m.Cursor] != (tuiRow{Session: "s2"}) {
			t.Fatalf("after s1 closed cursor sits on %+v; want s2's row", rows[m.Cursor])
		}
	})

	t.Run("(n) q and esc quit bare; esc with a filter clears it and stays", func(t *testing.T) {
		_, p := stepOK(t, tuiModel{Mode: tuiList}, keyRune('q'), st, 10)
		wantPending(t, p, tuiPending{Kind: tuiQuit})
		_, p = stepOK(t, tuiModel{Mode: tuiList}, keyName("escape"), st, 10)
		wantPending(t, p, tuiPending{Kind: tuiQuit})

		m, _ := stepOK(t, tuiModel{Mode: tuiList}, keyRune('/'), st, 10)
		for _, r := range "vim" {
			m, _ = stepOK(t, m, keyRune(r), st, 10)
		}
		m, _ = stepOK(t, m, keyName("enter"), st, 10) // filter applied: one pane row
		if m.Filter != "vim" || m.Cursor != 0 {
			t.Fatalf("filter/cursor = %q/%d; want vim/0", m.Filter, m.Cursor)
		}
		m, p = stepOK(t, m, keyName("escape"), st, 10)
		if p != nil {
			t.Fatalf("esc with filter fired %+v; want none", p)
		}
		if m.Filter != "" || m.Mode != tuiList {
			t.Fatalf("filter/mode = %q/%v; want cleared/list", m.Filter, m.Mode)
		}
		// The cursor held its session: the pane row fell back to s1's row.
		if rows := visibleRows(st, ""); rows[m.Cursor] != (tuiRow{Session: "s1"}) {
			t.Fatalf("cursor sits on %+v; want s1's session row", rows[m.Cursor])
		}

		// Esc in filter mode clears the filter too.
		m, _ = stepOK(t, tuiModel{Mode: tuiList}, keyRune('/'), st, 10)
		for _, r := range "vim" {
			m, _ = stepOK(t, m, keyRune(r), st, 10)
		}
		m, p = stepOK(t, m, keyName("escape"), st, 10)
		if p != nil || m.Filter != "" || m.Mode != tuiList {
			t.Fatalf("esc in filter mode: %+v %q %v", p, m.Filter, m.Mode)
		}
	})
}

func TestTUIStepFilterApply(t *testing.T) {
	st := tuiFix()

	t.Run("enter applies the filter with the cursor on the first match", func(t *testing.T) {
		m, _ := stepOK(t, tuiModel{Mode: tuiList, Cursor: 4}, keyRune('/'), st, 10)
		for _, r := range "vim" {
			m, _ = stepOK(t, m, keyRune(r), st, 10)
		}
		m, p := stepOK(t, m, keyName("enter"), st, 10)
		if p != nil || m.Mode != tuiList {
			t.Fatalf("apply: %+v mode %v", p, m.Mode)
		}
		rows := visibleRows(st, "vim")
		if len(rows) != 1 || rows[0] != (tuiRow{Session: "s1", Pane: "p2", Indent: true}) {
			t.Fatalf("rows = %+v; want s1/p2 alone", rows)
		}
		if m.Cursor != 0 {
			t.Fatalf("cursor = %d; want 0 (first match)", m.Cursor)
		}
	})

	t.Run("slash reseeds the editor with the live filter", func(t *testing.T) {
		m, _ := stepOK(t, tuiModel{Mode: tuiList}, keyRune('/'), st, 10)
		for _, r := range "vi" {
			m, _ = stepOK(t, m, keyRune(r), st, 10)
		}
		m, _ = stepOK(t, m, keyName("enter"), st, 10)
		m, _ = stepOK(t, m, keyRune('/'), st, 10)
		if m.Mode != tuiFilter || m.Editor.text() != "vi" || m.Editor.pos != 2 {
			t.Fatalf("reseed: %v %q@%d; want filter vi@2", m.Mode, m.Editor.text(), m.Editor.pos)
		}
		// Editing continues from where it stood.
		m, _ = stepOK(t, m, keyRune('m'), st, 10)
		if m.Editor.text() != "vim" {
			t.Fatalf("editor = %q; want vim", m.Editor.text())
		}
	})

	t.Run("an emptied filter keeps the whole list", func(t *testing.T) {
		m, _ := stepOK(t, tuiModel{Mode: tuiList}, keyRune('/'), st, 10)
		m, _ = stepOK(t, m, keyRune('x'), st, 10)
		m, _ = stepOK(t, m, keyName("ctrl+u"), st, 10) // clear the line
		m, _ = stepOK(t, m, keyName("enter"), st, 10)
		if m.Filter != "" || len(visibleRows(st, m.Filter)) != 5 {
			t.Fatalf("filter %q kept %d rows; want the whole list", m.Filter, len(visibleRows(st, m.Filter)))
		}
	})
}

func TestVisibleRows(t *testing.T) {
	st := tuiFix()
	if rows := visibleRows(st, ""); len(rows) != 5 {
		t.Fatalf("%d rows unfiltered; want 5", len(rows))
	}
	if rows := visibleRows(st, "BUILD"); len(rows) != 1 || rows[0] != (tuiRow{Session: "s1"}) {
		t.Fatalf("case-insensitive match: %+v; want s1's row", rows)
	}
	if rows := visibleRows(st, "done"); len(rows) != 1 || rows[0] != (tuiRow{Session: "s2"}) {
		t.Fatalf("exited session still matches: %+v; want s2's row", rows)
	}
	if rows := visibleRows(st, "zzz"); len(rows) != 0 {
		t.Fatalf("%d rows match zzz; want none", len(rows))
	}
}

// ---------------------------------------------------------------------------
// Frame rendering
// ---------------------------------------------------------------------------

// tui3Fix is the renderer's three-session scene: one working, one idle,
// one waiting, all single-pane so every session is exactly one row.
func tui3Fix() proto.State {
	mk := func(id, name string, status proto.Status, since int64) proto.Session {
		return proto.Session{
			ID: id, Name: name,
			Pane:  proto.Pane{Status: status, Since: since, Dir: "/home/u/" + name},
			Panes: []proto.Pane{{ID: "p" + id, Status: status, Since: since, Dir: "/home/u/" + name}},
		}
	}
	return proto.State{Sessions: []proto.Session{
		mk("a1", "build", proto.Working, 50_000),
		mk("a2", "deploy", proto.Idle, 0),
		mk("a3", "watch", proto.Waiting, 59_000),
	}}
}

// tuiTen is ten named sessions job01..job10, one row each in id order —
// the viewport scenarios' flat tall list.
func tuiTen() proto.State {
	st := proto.State{}
	for i := 1; i <= 10; i++ {
		st.Sessions = append(st.Sessions, proto.Session{
			ID: fmt.Sprintf("t%02d", i), Name: fmt.Sprintf("job%02d", i),
			Panes: []proto.Pane{{ID: "p"}},
		})
	}
	return st
}

// stripANSI takes an escape sequence out and leaves what shows on screen.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i++
			if i < len(s) && s[i] == '[' {
				i++
				for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
					i++
				}
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func TestDrawTUI(t *testing.T) {
	st := tui3Fix()

	t.Run("(a) 80x24: header, every label, cursor row reverse-video, footer", func(t *testing.T) {
		var buf bytes.Buffer
		if err := drawTUI(&buf, st, tuiModel{Mode: tuiList, Cursor: 1, Live: true}, "", 80, 24, 60_000); err != nil {
			t.Fatalf("drawTUI: %v", err)
		}
		frame := buf.String()
		if !strings.HasPrefix(frame, "\x1b[H") {
			t.Fatalf("frame does not home the cursor first:\n%q", frame[:min(40, len(frame))])
		}
		for _, want := range []string{"kero - 3 sessions", "build", "deploy", "watch", "enter attach"} {
			if !strings.Contains(frame, want) {
				t.Fatalf("frame lacks %q:\n%s", want, frame)
			}
		}
		// The cursor row's reverse video: opened before the dot and re-opened
		// right after it, once each, nowhere else.
		if n := strings.Count(frame, "\x1b[7m"); n != 2 {
			t.Fatalf("%d reverse-video opens; want 2 (before and after the dot)", n)
		}
	})

	t.Run("(b) width 15: no line past 15 cells, every newline in place", func(t *testing.T) {
		long := tui3Fix()
		long.Sessions[0].Name = "an-awfully-long-session-name"
		var buf bytes.Buffer
		if err := drawTUI(&buf, long, tuiModel{Mode: tuiList, Live: true}, "", 15, 10, 0); err != nil {
			t.Fatalf("drawTUI: %v", err)
		}
		frame := buf.String()
		if n := strings.Count(frame, "\r\n"); n != 9 {
			t.Fatalf("%d newlines; want 9 (10 lines, the last without one)", n)
		}
		for i, ln := range strings.Split(frame, "\r\n") {
			if w := utf8.RuneCountInString(stripANSI(ln)); w > 15 {
				t.Fatalf("line %d spans %d cells: %q", i, w, stripANSI(ln))
			}
		}
	})

	t.Run("(c) prompt and confirm lines", func(t *testing.T) {
		panes := tuiFix() // s1 carries panes p1..p3
		cases := []struct {
			st   proto.State
			m    tuiModel
			want string
		}{
			{st, tuiModel{Mode: tuiPrompt, Action: tuiNew, Editor: lineEditor{buf: []rune("claude")}}, "new session: claude"},
			{st, tuiModel{Mode: tuiPrompt, Action: tuiRename, Editor: lineEditor{buf: []rune("buildx")}}, "rename: buildx"},
			{st, tuiModel{Mode: tuiConfirm, Action: tuiClose, Target: tuiRow{Session: "a1"}}, "close session build? (y/n)"},
			{panes, tuiModel{Mode: tuiConfirm, Action: tuiClose, Target: tuiRow{Session: "s1", Pane: "p2", Indent: true}}, "close pane p2? (y/n)"},
		}
		for _, tc := range cases {
			var buf bytes.Buffer
			if err := drawTUI(&buf, tc.st, tc.m, "", 80, 24, 0); err != nil {
				t.Fatalf("drawTUI: %v", err)
			}
			if !strings.Contains(buf.String(), tc.want) {
				t.Fatalf("frame lacks %q:\n%s", tc.want, buf.String())
			}
		}
	})

	t.Run("(d) empty state renders the no-sessions hint", func(t *testing.T) {
		var buf bytes.Buffer
		if err := drawTUI(&buf, proto.State{}, tuiModel{Mode: tuiList}, "", 80, 24, 0); err != nil {
			t.Fatalf("drawTUI: %v", err)
		}
		if !strings.Contains(buf.String(), "no sessions") {
			t.Fatalf("frame lacks the hint:\n%s", buf.String())
		}
	})

	t.Run("(f) viewport: exactly height lines, rows follow the offset", func(t *testing.T) {
		ten := tuiTen()
		var buf bytes.Buffer
		if err := drawTUI(&buf, ten, tuiModel{Mode: tuiList, Live: true}, "", 80, 24, 0); err != nil {
			t.Fatalf("drawTUI: %v", err)
		}
		lines := strings.Split(buf.String(), "\r\n")
		if len(lines) != 24 {
			t.Fatalf("%d lines; want 24", len(lines))
		}
		for i := 1; i <= 10; i++ {
			if !strings.Contains(lines[i], fmt.Sprintf("job%02d", i)) {
				t.Fatalf("line %d lacks job%02d: %q", i, i, stripANSI(lines[i]))
			}
		}

		buf.Reset()
		if err := drawTUI(&buf, ten, tuiModel{Mode: tuiList, Cursor: 5, Offset: 2}, "", 80, 7, 0); err != nil {
			t.Fatalf("drawTUI: %v", err)
		}
		frame := buf.String()
		lines = strings.Split(frame, "\r\n")
		if len(lines) != 7 {
			t.Fatalf("%d lines; want 7", len(lines))
		}
		for i, job := range []string{"job03", "job04", "job05", "job06"} {
			if !strings.Contains(lines[1+i], job) {
				t.Fatalf("viewport row %d lacks %s: %q", i, job, stripANSI(lines[1+i]))
			}
		}
		for _, gone := range []string{"job02", "job07", "job01", "job10"} {
			if strings.Contains(frame, gone) {
				t.Fatalf("frame shows %s past the viewport:\n%s", gone, frame)
			}
		}
	})

	t.Run("(g) notice is list mode's second-to-last line", func(t *testing.T) {
		var buf bytes.Buffer
		if err := drawTUI(&buf, st, tuiModel{Mode: tuiList}, "kerod went away", 80, 6, 0); err != nil {
			t.Fatalf("drawTUI: %v", err)
		}
		lines := strings.Split(buf.String(), "\r\n")
		if len(lines) != 6 {
			t.Fatalf("%d lines; want 6", len(lines))
		}
		if got := stripANSI(lines[len(lines)-2]); got != "kerod went away" {
			t.Fatalf("notice line = %q; want the notice", got)
		}
	})
}

// runeWidth counts East Asian Wide and Fullwidth runes as two cells and
// everything else — the ambiguous em dash and middot included — as one;
// truncate walks display cells, so a CJK rune that would straddle the
// cut is dropped whole with the ellipsis in its place, never split.
func TestTUIWidths(t *testing.T) {
	for _, tt := range []struct {
		r    rune
		want int
	}{
		{'a', 1}, {'é', 1}, {'—', 1}, {'·', 1},
		{'你', 2}, {'あ', 2}, {'한', 2},
		{'。', 2}, {'，', 2}, {'Ａ', 2},
	} {
		if got := runeWidth(tt.r); got != tt.want {
			t.Errorf("runeWidth(%q) = %d; want %d", tt.r, got, tt.want)
		}
	}
	if got, want := ansiWidth("\x1b[32m你\x1b[0m"), 2; got != want {
		t.Errorf("ansiWidth of a styled CJK rune = %d; want %d", got, want)
	}
	for _, tt := range []struct {
		s    string
		w    int
		want string
	}{
		{"abc", 2, "a…"},
		{"abc", 3, "abc"},
		{"a你b", 3, "a…"}, // the 你 needs both remaining cells plus the ellipsis's: dropped
		{"a你b", 4, "a你b"},
		{"你我他", 4, "你…"}, // the cut lands before 我, not inside it
		{"你我他", 5, "你我…"},
	} {
		if got := truncate(tt.s, tt.w); got != tt.want {
			t.Errorf("truncate(%q, %d) = %q; want %q", tt.s, tt.w, got, tt.want)
		}
	}
}

// A CJK session name is two cells a rune on the terminal. Counted as
// one, a phone-narrow screen's lines would render past its width, wrap,
// and push the footer out of the frame — every frame, not just one. On
// a cols=30 screen every line must fit its width in display cells and
// the footer must keep its place as the last of the frame's lines.
func TestDrawTUICJKWidth(t *testing.T) {
	cjk := proto.State{Sessions: []proto.Session{
		{ID: "c1", Name: "数据库迁移任务", Dir: "/srv/长目录路径/projects",
			Panes: []proto.Pane{{ID: "p1", Dir: "/srv/长目录路径/projects"}}},
	}}
	const cols, rows = 30, 10
	var buf bytes.Buffer
	if err := drawTUI(&buf, cjk, tuiModel{Mode: tuiList, Live: true}, "", cols, rows, 0); err != nil {
		t.Fatalf("drawTUI: %v", err)
	}
	lines := strings.Split(buf.String(), "\r\n")
	if len(lines) != rows {
		t.Fatalf("%d lines; want %d — a wrapped line pushes the footer off", len(lines), rows)
	}
	for i, ln := range lines {
		if w := ansiWidth(ln); w > cols {
			t.Fatalf("line %d spans %d cells (max %d): %q", i, w, cols, stripANSI(ln))
		}
	}
	if last := stripANSI(lines[rows-1]); !strings.Contains(last, "enter attach") {
		t.Fatalf("footer line = %q; want the key hints still on screen", last)
	}
	if !strings.Contains(stripANSI(lines[1]), "数据库迁移任务") {
		t.Fatalf("first list line = %q; want the session name shown", stripANSI(lines[1]))
	}
}

// drawTUI is a pure function callers may hand any model: a viewport
// offset in the negatives must not index the list backwards — the rows
// it skips draw blank and the frame keeps its shape.
func TestDrawTUINegativeOffset(t *testing.T) {
	var buf bytes.Buffer
	m := tuiModel{Mode: tuiList, Offset: -2, Live: true}
	if err := drawTUI(&buf, tui3Fix(), m, "", 80, 6, 0); err != nil {
		t.Fatalf("drawTUI: %v", err)
	}
	lines := strings.Split(buf.String(), "\r\n")
	if len(lines) != 6 {
		t.Fatalf("%d lines; want 6", len(lines))
	}
	for i := 1; i <= 2; i++ { // rows before the offset: blank, not a panic
		if got := stripANSI(lines[i]); got != "" {
			t.Fatalf("line %d = %q; want blank (before the offset)", i, got)
		}
	}
	if !strings.Contains(lines[3], "watch") {
		t.Fatalf("line 3 = %q; want list[0] where the offset lands", stripANSI(lines[3]))
	}
}

// A Go test runs without a tty, so os.Stdin already fails the guard —
// the refusal must come before the client is ever touched (nil here).
func TestTUICmdNeedsTerminal(t *testing.T) {
	err := tuiCmd(proto.State{}, nil, nil, "")
	if !errors.Is(err, errUsage) {
		t.Fatalf("err = %v; want errUsage", err)
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("err = %v; want it to mention a terminal", err)
	}
	// Any argument is refused the same way, before the terminal check.
	err = tuiCmd(proto.State{}, nil, []string{"x"}, "")
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "no arguments") {
		t.Fatalf("err = %v; want the no-arguments usage error", err)
	}
}

// The usage block ships `kero tui`: without a line there the command is
// invisible to `kero help` readers.
func TestUsageListsTUI(t *testing.T) {
	if !strings.Contains(usage, "kero tui") {
		t.Fatal("usage lacks a kero tui entry")
	}
}

// The lone-ESC quiet timeout's two halves: what counts as held, and what
// flushing it yields. Only the single waiting ESC qualifies — a torn
// sequence or rune still wants its bytes.
func TestTUIEscapeFlush(t *testing.T) {
	var p inputParser
	if loneEscape(&p) {
		t.Fatalf("empty parser counts as a held escape")
	}
	if ev := p.feed([]byte("\x1b")); len(ev) != 0 {
		t.Fatalf("lone ESC yielded %d events; want it held", len(ev))
	}
	if !loneEscape(&p) {
		t.Fatalf("held lone ESC not seen")
	}
	if ev, ok := takeEscape(&p); !ok || ev.Key.Name != "escape" {
		t.Fatalf("takeEscape = %+v, %v; want the escape key", ev, ok)
	}
	if loneEscape(&p) {
		t.Fatalf("parser still holds after the flush")
	}
	if _, ok := takeEscape(&p); ok {
		t.Fatalf("second flush yielded an event from nothing")
	}

	// A torn sequence is not a lone escape: it waits for bytes, not the
	// timeout.
	p.feed([]byte("\x1b["))
	if loneEscape(&p) {
		t.Fatalf("torn CSI counts as a lone escape")
	}
}

// The TUI's lone-ESC quiet must outlast attach's: a phone SSH
// connection can split a mouse report by more than the local byte gap
// attach measures, and the leading ESC taken for the key would quit the
// TUI mid-tap.
func TestTUIEscapeQuiet(t *testing.T) {
	if tuiEscapeQuiet <= escapeQuiet {
		t.Fatalf("tuiEscapeQuiet = %v; want longer than attach's escapeQuiet (%v)", tuiEscapeQuiet, escapeQuiet)
	}
}
