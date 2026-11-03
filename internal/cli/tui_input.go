package cli

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// tuiKey is one decoded keypress: a printable rune, a digit 1-9, or a
// named key. Digits are kept apart from runes because the list reads
// them as session shortcuts while the filter bar wants them as text.
type tuiKey struct {
	Rune  rune   // printable rune, 0 when the key is not a character
	Digit int    // 1-9 for '1'-'9', else 0
	Name  string // "" when the key is a rune or digit
}

// tuiMouse is a decoded SGR mouse report. Col and Row are 1-based screen
// cells; Wheel is -1 or +1 per scroll notch and 0 for a click.
type tuiMouse struct {
	Col, Row int
	Wheel    int
}

// tuiEvent is one decoded input event: a key, or — with IsMouse set — a
// mouse report.
type tuiEvent struct {
	Key     tuiKey
	Mouse   tuiMouse
	IsMouse bool
}

// inputParser turns raw terminal bytes into tuiEvents across reads as
// ragged as a phone SSH connection makes them: an escape sequence or a
// multi-byte rune can be split between two reads, so feed holds whatever
// it cannot yet decide and completes it with the next batch.
type inputParser struct {
	pending []byte
}

// feed takes one keyboard read and returns every event it completes. An
// empty return means the batch was all prologue — bytes buffered until
// the next read decides them — never that input was lost.
func (p *inputParser) feed(b []byte) []tuiEvent {
	p.pending = append(p.pending, b...)
	var events []tuiEvent
	for len(p.pending) > 0 {
		n, ev, has := decode(p.pending)
		if n == 0 {
			break // an unfinished sequence: wait for the next batch
		}
		p.pending = p.pending[n:]
		if has {
			events = append(events, ev)
		}
	}
	if len(p.pending) == 0 {
		p.pending = nil // release the buffer between keystrokes
	}
	return events
}

// decode decides the first event in buf, returning how many bytes it
// consumed, the event, and whether there is one. Zero bytes consumed
// means the front of buf is an incomplete sequence the parser must hold.
func decode(buf []byte) (n int, ev tuiEvent, has bool) {
	switch buf[0] {
	case 0x1b:
		// A lone ESC at the end of the buffer might be a torn sequence's
		// first byte, so it waits; only a following byte that starts no
		// sequence proves the user pressed Escape.
		if len(buf) == 1 {
			return 0, ev, false
		}
		if buf[1] == '[' {
			return decodeCSI(buf)
		}
		return 1, tuiEvent{Key: tuiKey{Name: "escape"}}, true
	case 0x0d, 0x0a:
		return 1, tuiEvent{Key: tuiKey{Name: "enter"}}, true
	case 0x09:
		return 1, tuiEvent{Key: tuiKey{Name: "tab"}}, true
	case 0x7f:
		return 1, tuiEvent{Key: tuiKey{Name: "backspace"}}, true
	case 0x15:
		return 1, tuiEvent{Key: tuiKey{Name: "ctrl+u"}}, true
	case 0x17:
		return 1, tuiEvent{Key: tuiKey{Name: "ctrl+w"}}, true
	}
	// Decoding over the pending buffer — never over the read batch — is
	// what carries a rune split across reads to its single decode.
	r, size := utf8.DecodeRune(buf)
	if r == utf8.RuneError && size == 1 {
		if !utf8.FullRune(buf) {
			return 0, ev, false // a rune still arriving
		}
		return 1, ev, false // not truncated, just invalid: drop the byte
	}
	if r < 0x20 {
		return size, ev, false // a control the TUI has no meaning for
	}
	if r >= '1' && r <= '9' {
		return size, tuiEvent{Key: tuiKey{Digit: int(r - '0')}}, true
	}
	return size, tuiEvent{Key: tuiKey{Rune: r}}, true
}

// decodeCSI handles a sequence from its ESC [ prefix. A sequence ends at
// its final byte, 0x40-0x7e; parameters and intermediates sit below
// that, so the scan cannot stop early on a digit or a ';'.
func decodeCSI(buf []byte) (n int, ev tuiEvent, has bool) {
	end := -1
	for i := 2; i < len(buf); i++ {
		if buf[i] >= 0x40 && buf[i] <= 0x7e {
			end = i
			break
		}
	}
	if end < 0 {
		return 0, ev, false // still arriving
	}
	// Bare arrows carry no parameters. Modified arrows ride the same
	// final byte, but mean nothing here and fall through swallowed.
	if end == 2 {
		switch buf[end] {
		case 'A':
			return end + 1, tuiEvent{Key: tuiKey{Name: "up"}}, true
		case 'B':
			return end + 1, tuiEvent{Key: tuiKey{Name: "down"}}, true
		case 'C':
			return end + 1, tuiEvent{Key: tuiKey{Name: "right"}}, true
		case 'D':
			return end + 1, tuiEvent{Key: tuiKey{Name: "left"}}, true
		}
	}
	if buf[2] == '<' && (buf[end] == 'M' || buf[end] == 'm') {
		return decodeMouse(buf, end)
	}
	// Unknown sequence: swallowed whole so its bytes never read as keys.
	return end + 1, ev, false
}

// decodeMouse parses the SGR mouse report ending at buf[end] — button;
// col; row, with M for a press and m for a release. Releases and buttons
// the list has no use for are consumed silently: a report the TUI
// ignores must still be eaten whole, or its trailing bytes surface as
// keystrokes.
func decodeMouse(buf []byte, end int) (n int, ev tuiEvent, has bool) {
	parts := strings.Split(string(buf[3:end]), ";")
	if len(parts) == 3 && buf[end] == 'M' {
		b, errb := strconv.Atoi(parts[0])
		col, errc := strconv.Atoi(parts[1])
		row, errr := strconv.Atoi(parts[2])
		if errb == nil && errc == nil && errr == nil {
			var wheel int
			switch b {
			case 0: // left press: a click
			case 64:
				wheel = -1
			case 65:
				wheel = 1
			default:
				return end + 1, ev, false
			}
			return end + 1, tuiEvent{Mouse: tuiMouse{Col: col, Row: row, Wheel: wheel}, IsMouse: true}, true
		}
	}
	return end + 1, ev, false
}

// lineEditor is the filter bar's text: a rune slice the user edits at a
// cursor. The fields are exported because the renderer draws the cursor
// from buf and pos directly.
type lineEditor struct {
	buf []rune
	pos int
}

// key applies one editing key, reporting whether it was one to take: a
// printable rune — digits included, put back as text once the list has
// not claimed them as shortcuts — inserts at the cursor; backspace and
// the arrows edit around it; ctrl+w deletes the word left of the cursor;
// ctrl+u clears. Any other key leaves buf and pos untouched and returns
// false for the state machine to handle.
func (e *lineEditor) key(k tuiKey) bool {
	r := k.Rune
	if r == 0 && k.Digit != 0 {
		r = rune('0' + k.Digit)
	}
	switch {
	case r != 0:
		e.buf = append(e.buf, 0)
		copy(e.buf[e.pos+1:], e.buf[e.pos:])
		e.buf[e.pos] = r
		e.pos++
		return true
	case k.Name == "backspace":
		if e.pos > 0 {
			e.buf = append(e.buf[:e.pos-1], e.buf[e.pos:]...)
			e.pos--
		}
		return true
	case k.Name == "left":
		if e.pos > 0 {
			e.pos--
		}
		return true
	case k.Name == "right":
		if e.pos < len(e.buf) {
			e.pos++
		}
		return true
	case k.Name == "ctrl+w":
		// Rub out the word left of the cursor along with the spaces
		// behind it, the way a shell's ctrl+w feels.
		start := e.pos
		for start > 0 && e.buf[start-1] == ' ' {
			start--
		}
		for start > 0 && e.buf[start-1] != ' ' {
			start--
		}
		e.buf = append(e.buf[:start], e.buf[e.pos:]...)
		e.pos = start
		return true
	case k.Name == "ctrl+u":
		e.buf, e.pos = e.buf[:0], 0
		return true
	}
	return false
}

// text is the edited line.
func (e *lineEditor) text() string {
	return string(e.buf)
}
