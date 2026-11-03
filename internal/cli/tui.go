package cli

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"kero/internal/client"
	"kero/internal/proto"

	"golang.org/x/term"
)

// ---------------------------------------------------------------------------
// Model: the whole world the keyboard acts on
// ---------------------------------------------------------------------------

// tuiAction is what a step decided to do — the loop outside executes it.
type tuiAction uint8

const (
	tuiNone   tuiAction = iota
	tuiAttach           // enter a session's terminal
	tuiNew              // run a command in a new session
	tuiSplit            // split a pane into two
	tuiClose            // close a pane, or a whole session
	tuiRename           // give a session a new name
	tuiQuit             // leave the TUI
)

// tuiPending is one action fired and ready: Pane and SessionID name the
// target — Pane set for a pane-level act, "" on the session itself — and
// Text carries what a prompt asked for. Kind tuiQuit needs none of them.
type tuiPending struct {
	Kind      tuiAction
	Pane      string
	Text      string
	SessionID string
}

// tuiModel is the TUI's whole state. Offset is the first list row the
// viewport shows; Held is the row the cursor last stood on, private
// bookkeeping that keeps the cursor on its session across state changes.
type tuiModel struct {
	Mode   tuiMode
	Cursor int
	Offset int
	Filter string
	Editor lineEditor
	Action tuiAction
	Target tuiRow
	Live   bool

	held tuiRow
}

// ---------------------------------------------------------------------------
// The visible list
// ---------------------------------------------------------------------------

// visibleRows is the list as the screen shows it: tuiRows' layout with
// the filter applied. Both the state machine and the renderer go through
// here, so what the keys move over and what the screen draws cannot
// drift apart.
func visibleRows(st proto.State, filter string) []tuiRow {
	var out []tuiRow
	for _, r := range tuiRows(st) {
		if s := sessionOf(st, r.Session); s != nil && rowMatches(s, r.Pane, filter) {
			out = append(out, r)
		}
	}
	return out
}

// sessionOf finds a session by id in the state, or nil.
func sessionOf(st proto.State, id string) *proto.Session {
	for i := range st.Sessions {
		if st.Sessions[i].ID == id {
			return &st.Sessions[i]
		}
	}
	return nil
}

// rowAt is the row at i, or the zero row when i is outside the list —
// the empty list's answer to every "what is at the cursor".
func rowAt(rows []tuiRow, i int) tuiRow {
	if i >= 0 && i < len(rows) {
		return rows[i]
	}
	return tuiRow{}
}

// ---------------------------------------------------------------------------
// The state machine: one event, one transition
// ---------------------------------------------------------------------------

// tuiStep applies one event to the model and returns what changed: the
// model by value, the action it fired if any, and an error only when an
// attach could not resolve its pane. The row list is derived, never
// passed in, so model and screen cannot diverge. height is the count of
// visible list rows; the viewport slides to keep the cursor on screen.
func tuiStep(m tuiModel, ev tuiEvent, st proto.State, height int) (tuiModel, *tuiPending, error) {
	rows := visibleRows(st, m.Filter)
	// The state may have moved under us since the last event: pull the
	// cursor back onto the session it stood on before anything else
	// reads it as a position.
	m.Cursor = holdCursor(rows, m.held, m.Cursor)

	var pending *tuiPending
	var err error
	switch m.Mode {
	case tuiList:
		pending, err = stepList(&m, ev, st, rows, height)
	case tuiFilter:
		pending = stepFilter(&m, ev, st, rows)
	case tuiPrompt:
		pending = stepPrompt(&m, ev)
	case tuiConfirm:
		pending = stepConfirm(&m, ev)
	}

	// A filter change inside the step changed the list; clamp against
	// what stands now, then remember where the cursor ended up.
	rows = visibleRows(st, m.Filter)
	clampView(&m, len(rows), height)
	m.held = rowAt(rows, m.Cursor)
	return m, pending, err
}

// stepList is the browsing mode: motion, jumps, and the one-key acts.
func stepList(m *tuiModel, ev tuiEvent, st proto.State, rows []tuiRow, height int) (*tuiPending, error) {
	n := len(rows)
	if ev.IsMouse {
		if ev.Mouse.Wheel != 0 {
			// The wheel walks the cursor, not the viewport: the clamp
			// at the end of the step decides what scrolls.
			if ev.Mouse.Wheel < 0 {
				moveCursor(m, n, -1)
			} else {
				moveCursor(m, n, +1)
			}
			return nil, nil
		}
		// A click arrives as a 1-based screen row. The layout contract
		// with drawTUI: row 1 is the header, the list occupies rows
		// 2..height+1, the prompt line and footer follow — a click
		// there selects nothing.
		if ev.Mouse.Row < 2 || ev.Mouse.Row > height+1 {
			return nil, nil
		}
		i := ev.Mouse.Row - 2 + m.Offset
		if i < 0 || i >= n {
			return nil, nil // clicked a blank line below the list
		}
		m.Cursor = i
		return attachPending(st, rows, m.Cursor)
	}
	k := ev.Key
	switch {
	case k.Name == "up":
		moveCursor(m, n, -1)
	case k.Name == "down":
		moveCursor(m, n, +1)
	case k.Digit != 0:
		if n > 0 {
			m.Cursor = min(k.Digit-1, n-1) // a jump, never a prefix
		}
	case k.Name == "enter":
		return attachPending(st, rows, m.Cursor)
	case k.Rune == 'n':
		m.Mode, m.Action, m.Target = tuiPrompt, tuiNew, rowAt(rows, m.Cursor)
		m.Editor = lineEditor{}
	case k.Rune == 's' && n > 0: // row actions need a row: on an empty list only n, / and q act
		return splitPending(st, rows, m.Cursor), nil // zero input: fire at once
	case k.Rune == 'd' && n > 0:
		m.Mode, m.Action, m.Target = tuiConfirm, tuiClose, rowAt(rows, m.Cursor)
	case k.Rune == 'r' && n > 0:
		m.Mode, m.Action, m.Target = tuiPrompt, tuiRename, rowAt(rows, m.Cursor)
		m.Editor = lineEditor{}
		if s := sessionOf(st, m.Target.Session); s != nil {
			m.Editor.buf, m.Editor.pos = []rune(s.Name), len(s.Name)
		}
	case k.Rune == '/':
		m.Mode = tuiFilter
		m.Editor = lineEditor{buf: []rune(m.Filter), pos: len(m.Filter)} // edit, not replace
	case k.Rune == 'q':
		return &tuiPending{Kind: tuiQuit}, nil
	case k.Name == "escape":
		if m.Filter == "" {
			return &tuiPending{Kind: tuiQuit}, nil // the spec's q·esc pair
		}
		m.Cursor = clearFilter(m, st, rows)
	}
	return nil, nil
}

// stepFilter is the filter bar: the editor owns every key but two.
func stepFilter(m *tuiModel, ev tuiEvent, st proto.State, rows []tuiRow) *tuiPending {
	if m.Editor.key(ev.Key) {
		return nil // typed, not acted
	}
	switch ev.Key.Name {
	case "enter":
		m.Filter = m.Editor.text()
		m.Mode = tuiList
		m.Cursor = 0 // the first match, if any
	case "escape":
		m.Cursor = clearFilter(m, st, rows)
	}
	return nil
}

// stepPrompt is the one-line command box (new, rename): the editor owns
// every key but enter to run and escape to cancel.
func stepPrompt(m *tuiModel, ev tuiEvent) *tuiPending {
	if m.Editor.key(ev.Key) {
		return nil
	}
	switch ev.Key.Name {
	case "enter":
		p := &tuiPending{Kind: m.Action, Text: m.Editor.text()}
		if m.Action == tuiRename {
			p.SessionID = m.Target.Session // a rename names a session; a new session has none yet
		}
		m.Mode = tuiList
		return p
	case "escape":
		m.Mode = tuiList // cancelled: the editor's text stays behind, dead
	}
	return nil
}

// stepConfirm is the close question: y does it, n or escape backs out,
// anything else waits.
func stepConfirm(m *tuiModel, ev tuiEvent) *tuiPending {
	switch {
	case ev.Key.Rune == 'y':
		p := &tuiPending{Kind: tuiClose, SessionID: m.Target.Session, Pane: m.Target.Pane}
		m.Mode = tuiList
		return p
	case ev.Key.Rune == 'n', ev.Key.Name == "escape":
		m.Mode = tuiList // the question promised (y/n): n is a no
	}
	return nil
}

// clearFilter empties the filter from list or filter mode and returns
// where the cursor lands: held on the session it stood on, in the list
// the cleared filter brings back.
func clearFilter(m *tuiModel, st proto.State, rows []tuiRow) int {
	held := rowAt(rows, m.Cursor)
	m.Filter = ""
	m.Mode = tuiList
	return holdCursor(visibleRows(st, ""), held, m.Cursor)
}

// attachPending resolves the row at the cursor into an attach: a pane
// row by its id, a session row by its focused pane — the only step that
// can fail, when the session has no pane to show.
func attachPending(st proto.State, rows []tuiRow, cursor int) (*tuiPending, error) {
	row := rowAt(rows, cursor)
	if row.Session == "" {
		return nil, nil // nothing selected: nothing to attach to
	}
	if row.Pane != "" {
		return &tuiPending{Kind: tuiAttach, SessionID: row.Session, Pane: row.Pane}, nil
	}
	s := sessionOf(st, row.Session)
	if s == nil {
		return nil, nil
	}
	pane, err := focusPane(*s)
	if err != nil {
		return nil, err
	}
	return &tuiPending{Kind: tuiAttach, SessionID: row.Session, Pane: pane}, nil
}

// splitPending resolves the row at the cursor into a split: the pane to
// halve is the pane row's own, else the session's focused one.
func splitPending(st proto.State, rows []tuiRow, cursor int) *tuiPending {
	row := rowAt(rows, cursor)
	if row.Session == "" {
		return nil
	}
	pane := row.Pane
	if pane == "" {
		if s := sessionOf(st, row.Session); s != nil && s.FocusPane() != nil {
			pane = s.FocusPane().ID
		}
	}
	return &tuiPending{Kind: tuiSplit, SessionID: row.Session, Pane: pane}
}

// moveCursor steps the cursor delta rows, wrapping at both ends of the
// list; an empty list has nowhere to go.
func moveCursor(m *tuiModel, n, delta int) {
	if n == 0 {
		return
	}
	m.Cursor = ((m.Cursor+delta)%n + n) % n
}

// holdCursor picks the row the cursor should stand on once the list may
// have changed shape: the held row where it still is, else the first row
// of the held row's session, else want clamped into range. A zero held
// row (nothing stood anywhere yet) only clamps.
func holdCursor(rows []tuiRow, held tuiRow, want int) int {
	n := len(rows)
	if n == 0 {
		return 0
	}
	if want >= 0 && want < n && rows[want] == held {
		return want // the row it stood on is still there
	}
	if held.Session != "" {
		for i, r := range rows {
			if r.Session == held.Session {
				return i // the session lives on: stand on its first row
			}
		}
	}
	switch {
	case want < 0:
		return 0
	case want >= n:
		return n - 1
	}
	return want
}

// clampView fits the viewport to the cursor: dragging the offset when
// the cursor walks past either edge, never scrolling past the list's
// end, and an empty list shows from the top.
func clampView(m *tuiModel, n, height int) {
	if n == 0 {
		m.Cursor, m.Offset = 0, 0
		return
	}
	if height < 1 {
		height = 1 // a screen too small to scroll still shows a row
	}
	if m.Cursor < 0 {
		m.Cursor = 0
	} else if m.Cursor >= n {
		m.Cursor = n - 1
	}
	switch {
	case m.Offset > m.Cursor:
		m.Offset = m.Cursor
	case m.Cursor >= m.Offset+height:
		m.Offset = m.Cursor - height + 1
	}
	if max := n - height; m.Offset > max {
		m.Offset = max
	}
	if m.Offset < 0 {
		m.Offset = 0
	}
}

// ---------------------------------------------------------------------------
// Rendering: one frame, whole
// ---------------------------------------------------------------------------

// drawTUI paints the whole screen in one write: a header line, the
// viewport's slice of the list, the prompt-or-notice line, and the
// footer — exactly height lines of the screen's whole row count, the
// list living in the height-3 between the chrome. No line passes cols,
// each is cleared to its end so no stale cell survives a redraw, and
// the frame never goes out half-built: a torn frame tears on screen.
func drawTUI(w io.Writer, st proto.State, m tuiModel, notice string, cols, height int, now int64) error {
	// Home first: a redraw starts wherever the last frame left the
	// cursor, and only the first frame is guaranteed the top of a fresh
	// screen.
	var b strings.Builder
	b.WriteString("\x1b[H")
	line := func(text string) {
		b.WriteString(text)
		b.WriteString("\x1b[K\r\n")
	}
	// An ASCII dash, not an em dash: U+2014 is East Asian ambiguous —
	// wide on a CJK phone terminal — and a header wider than the screen
	// wraps the footer out of it.
	line(fitSegments(cols, fmt.Sprintf(" kero - %d sessions", len(st.Sessions))))
	list := visibleRows(st, m.Filter)
	n := max(height-3, 0)
	for i := 0; i < n; i++ {
		switch at := m.Offset + i; {
		case len(list) == 0 && i == 0:
			line(fitSegments(cols, "no sessions — n starts one"))
		case at >= 0 && at < len(list):
			line(renderRow(st, list[at], at == m.Cursor, cols, now))
		default:
			line("") // below the list: blank, cleared
		}
	}
	line(fitSegments(cols, promptLine(st, m, notice)))
	// The footer is the last line: a newline after it would scroll a
	// screen already full.
	b.WriteString(fitSegments(cols, footer(m.Mode, m.Live)))
	b.WriteString("\x1b[K")
	_, err := w.Write([]byte(b.String()))
	return err
}

// promptLine is the second-to-last line: what the mode asks for when it
// asks — the filter bar, the command box, the close question — else the
// last notice, blank when there was none.
func promptLine(st proto.State, m tuiModel, notice string) string {
	switch m.Mode {
	case tuiFilter:
		return "filter: " + m.Editor.text()
	case tuiPrompt:
		if m.Action == tuiRename {
			return "rename: " + m.Editor.text()
		}
		return "new session: " + m.Editor.text()
	case tuiConfirm:
		if m.Target.Pane != "" {
			return fmt.Sprintf("close pane %s? (y/n)", m.Target.Pane)
		}
		if s := sessionOf(st, m.Target.Session); s != nil {
			return fmt.Sprintf("close session %s? (y/n)", rowLabel(s, ""))
		}
		return "close session? (y/n)"
	}
	return notice
}

// renderRow is one line of the list: the status dot, the name, the
// directory, the program, the age — the cursor's row all reverse video.
// The dot's own color reset ends the video as soon as it starts, so the
// video goes out again right after the dot.
func renderRow(st proto.State, r tuiRow, cursor bool, cols int, now int64) string {
	s := sessionOf(st, r.Session)
	if s == nil {
		return ""
	}
	dot, dir, who, since := statusDot(s.Status), s.Dir, whoRuns(&s.Pane), s.Since
	if r.Pane != "" {
		p := s.PaneByID(r.Pane)
		if p == nil {
			return "" // the pane left between layout and draw
		}
		dot, dir, who, since = statusDot(p.Status), p.Dir, whoRuns(p), p.Since
	}
	label := rowLabel(s, r.Pane)
	if r.Indent {
		label = "  " + label
	}
	if !cursor {
		return fitSegments(cols, dot, label, proto.ShortDir(dir), who, humanSince(now, since))
	}
	// The dot and its space come off the budget before the rest fits.
	rest := fitSegments(cols-2, label, proto.ShortDir(dir), who, humanSince(now, since))
	var b strings.Builder
	b.WriteString("\x1b[7m")
	b.WriteString(dot)
	b.WriteString("\x1b[7m") // the dot's reset ended it: on again
	if rest != "" {
		b.WriteString(" ")
		b.WriteString(rest)
	}
	b.WriteString("\x1b[0m")
	return b.String()
}

// whoRuns names what a pane is running for its row: the agent by its
// brand, else the foreground process, else nothing.
func whoRuns(p *proto.Pane) string {
	if p.Agent != "" {
		if brand, ok := proto.AgentNames[p.Agent]; ok {
			return brand
		}
		return p.Agent
	}
	return p.Process
}

// fitSegments lays segments left to right, one space between, into at
// most cols visible cells: a segment that does not fit is truncated
// with an ellipsis and everything after it dropped — the earlier fields,
// dot and name, are the ones worth keeping when the screen runs out.
// Empty segments never show, not even their space.
func fitSegments(cols int, segs ...string) string {
	var b strings.Builder
	used := 0
	for _, seg := range segs {
		if seg == "" {
			continue
		}
		gap := 0
		if used > 0 {
			gap = 1
		}
		if width := ansiWidth(seg); used+gap+width <= cols {
			if gap == 1 {
				b.WriteByte(' ')
			}
			b.WriteString(seg)
			used += gap + width
			continue
		}
		if avail := cols - used - gap; avail > 0 {
			if gap == 1 {
				b.WriteByte(' ')
			}
			b.WriteString(truncate(seg, avail))
		}
		return b.String()
	}
	return b.String()
}

// runeWidth is the cells a rune takes on screen: East Asian Wide and
// Fullwidth runes take two, everything else one. The table covers the
// CJK blocks a session name realistically holds — Chinese, Japanese,
// Korean, the fullwidth punctuation between them — not every wide range
// Unicode defines; ambiguous runes (em dashes, the middot) stay one.
func runeWidth(r rune) int {
	switch {
	case r >= 0x1100 && r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E,   // CJK radicals, kana aids, CJK punctuation
		r >= 0x3041 && r <= 0x33FF,   // Hiragana, Katakana, CJK compatibility
		r >= 0x3400 && r <= 0x4DBF,   // CJK ideographs, extension A
		r >= 0x4E00 && r <= 0x9FFF,   // CJK unified ideographs
		r >= 0xAC00 && r <= 0xD7A3,   // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF,   // CJK compatibility ideographs
		r >= 0xFE30 && r <= 0xFE4F,   // CJK compatibility forms
		r >= 0xFF00 && r <= 0xFF60,   // fullwidth forms and signs
		r >= 0x20000 && r <= 0x3FFFD: // CJK ideograph extensions
		return 2
	}
	return 1
}

// truncate cuts text to at most w cells, marking the cut with an
// ellipsis when there was more to show. The cut walks display cells, not
// runes or bytes — a CJK rune spans two cells and the boundary can land
// before it, never inside it — and escape sequences pass through free,
// the same pass ansiWidth makes.
func truncate(s string, w int) string {
	if w < 1 {
		return ""
	}
	if ansiWidth(s) <= w {
		return s
	}
	var b strings.Builder
	used := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			// An escape sequence styles but takes no room: keep it whole.
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
			}
			if j < len(s) {
				j++
			}
			b.WriteString(s[i:j])
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if used+runeWidth(r) > w-1 { // the ellipsis takes the last cell
			break
		}
		b.WriteString(s[i : i+size])
		used += runeWidth(r)
		i += size
	}
	return b.String() + "…"
}

// ansiWidth counts the cells a segment takes on screen: escape
// sequences style but take no room, every other rune its own width — a
// CJK rune two cells, not one.
func ansiWidth(s string) int {
	w, i := 0, 0
	for i < len(s) {
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
		r, size := utf8.DecodeRuneInString(s[i:])
		w += runeWidth(r)
		i += size
	}
	return w
}

// ---------------------------------------------------------------------------
// The loop: terminal, events, and the handoff into attach
// ---------------------------------------------------------------------------

// The TUI's terminal, set up and torn down: the alternate screen, mouse
// button-tracking in SGR form, the cursor hidden. No DECCKM — the input
// parser reads no SS3 arrows.
const (
	tuiEnterScreen = "\x1b[?1049h\x1b[?1002h\x1b[?1006h\x1b[?25l\x1b[2J\x1b[H"
	tuiLeaveScreen = "\x1b[?25h\x1b[?1006l\x1b[?1002l\x1b[?1049l"
)

// tuiCmd is what the CLI switch calls for `kero tui`: it puts the
// session list on the terminal, keeps it live off the daemon's state
// events — one silent resubscribe when the watch's connection drops —
// and hands the terminal to attach when the user enters a session.
func tuiCmd(st proto.State, c *client.Client, args []string, host string) error {
	if len(args) > 0 {
		return usageErr("kero tui takes no arguments")
	}
	// A piped TUI has no renderer and no size: refuse before any daemon
	// work, as attach does.
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return usageErr("kero tui needs a terminal")
	}
	fd := int(os.Stdin.Fd())
	cols, scrRows, _, _, err := termPixels(fd)
	if err != nil {
		return err
	}
	raw, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	// Every exit path restores the shell's terminal before it returns.
	defer term.Restore(fd, raw)
	out := os.Stdout
	io.WriteString(out, tuiEnterScreen)
	defer io.WriteString(out, tuiLeaveScreen) // idempotent, sent again on attach
	var resubDone func()
	defer func() {
		if resubDone != nil {
			resubDone()
		}
	}()

	// Events wait in a room-one queue that keeps only the newest state:
	// a state dropped is one the next tells anyway, and the loop is
	// never kept waiting on a frame it has not drawn.
	states := make(chan proto.State, 1)
	onState := func(msg proto.Message) {
		if msg.Event != "state" || msg.State == nil {
			return
		}
		select {
		case states <- *msg.State:
		default:
			select {
			case <-states: // make room by dropping the stale one
			default:
			}
			select {
			case states <- *msg.State:
			default:
			}
		}
	}
	c.OnEvent(onState)
	m := tuiModel{Mode: tuiList, Live: true}
	var notice string
	if err := c.Call("watch", nil, nil); err != nil {
		// An old kerod, or a watch that would not start: the list still
		// works, frozen. The TUI does not die for want of events.
		m.Live = false
		if strings.Contains(err.Error(), "unknown method") {
			notice = "static list: this kerod predates live updates"
		} else {
			notice = err.Error()
		}
	}

	// One stdin reader for the whole command, not one per phase. The
	// TUI's batches go to the loop's channel; after the attach handoff
	// the same read feeds the pane's pipe — attachLoop starts a reader
	// of its own, and a second reader beside this one races the
	// terminal's bytes away (attach.go's own warning). The switch flips
	// between the two sends, so a batch waiting mid-flight lands
	// wherever the command now is.
	typed := make(chan []byte, 16)
	pr, pw := io.Pipe()
	var (
		paneMu  sync.Mutex
		paneOut *io.PipeWriter // set at the handoff
		handed  = make(chan struct{})
	)
	paneWriter := func() *io.PipeWriter {
		paneMu.Lock()
		defer paneMu.Unlock()
		return paneOut
	}
	defer pw.Close() // frees the attach's reader and the pump of their waits
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				b := append([]byte(nil), buf[:n]...)
				if w := paneWriter(); w != nil {
					w.Write(b)
					continue
				}
				select {
				case typed <- b:
				case <-handed: // the handoff landed mid-send
					paneWriter().Write(b)
				}
			}
			if err != nil {
				close(typed)
				if w := paneWriter(); w != nil {
					w.Close() // stdin died inside the attach: it ends too
				}
				return
			}
		}
	}()

	var (
		parser   inputParser
		want     string // the session a create echoed, to jump the cursor to
		attach   *tuiPending
		resubbed bool
		drawErr  error
		escTimer *time.Timer
		escQuiet <-chan time.Time
	)
	// The background retry's landing spot: buffered, so the goroutine
	// finishes its send even if the loop has already left.
	resub := make(chan resubResult, 1)
	// A lone ESC waits inside the parser for a byte proving it a prefix;
	// tuiEscapeQuiet of silence proves it a keypress instead.
	armEsc := func() {
		if !loneEscape(&parser) {
			return
		}
		if escTimer != nil {
			escTimer.Stop()
		}
		escTimer = time.NewTimer(tuiEscapeQuiet)
		escQuiet = escTimer.C
	}
	defer func() {
		if escTimer != nil {
			escTimer.Stop()
		}
	}()

	// apply runs one event through the machine and executes whatever it
	// fired — RPC errors become a notice for a frame, never an exit. It
	// reports an attach to hand off to, and that the loop is done.
	apply := func(ev tuiEvent) (handoff *tuiPending, done bool) {
		var p *tuiPending
		var err error
		m, p, err = tuiStep(m, ev, st, scrRows-3)
		if err != nil {
			notice = err.Error() // a failed step is a message, not an exit
			return nil, false
		}
		if p == nil {
			return nil, false
		}
		switch p.Kind {
		case tuiQuit:
			return nil, true
		case tuiAttach:
			return p, true
		case tuiNew:
			// Whitespace-split on purpose: commands with spaced
			// arguments belong to the full `kero new`.
			var s proto.Session
			params := proto.CreateParams{After: os.Getenv("KERO_SESSION")}
			if fields := strings.Fields(p.Text); len(fields) > 0 {
				params.Command = fields
			}
			if err := c.Call("create", params, &s); err != nil {
				notice = err.Error()
			} else {
				want = s.ID
			}
		case tuiSplit:
			if err := c.Call("split", proto.CreateParams{Split: p.Pane, Direction: "right", Ratio: 0}, nil); err != nil {
				notice = err.Error()
			}
		case tuiClose:
			var err error
			if p.Pane != "" {
				err = c.Call("pane.close", proto.IDParams{ID: p.Pane}, nil)
			} else {
				err = c.Call("close", proto.CloseParams{ID: p.SessionID}, nil)
			}
			if err != nil {
				notice = err.Error()
			}
		case tuiRename:
			if err := c.Call("rename", proto.RenameParams{ID: p.SessionID, Name: p.Text, By: os.Getenv("KERO_PANE")}, nil); err != nil {
				notice = err.Error()
			}
		}
		return nil, false
	}

	resize := time.NewTicker(250 * time.Millisecond)
	defer resize.Stop()
	// A signal from outside while raw mode swallows the keyboard's own
	// ^C: the one other way a user has of being let out.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	running := true
	// The watch connection's Done channel, cached: once it closes, a
	// receive on it fires every select round — a closed channel is
	// always ready — and the loop would spin redrawing at full tilt
	// while disconnected. Nil-ing it after the first fire blocks that
	// case until a resubscribe brings a fresh client's Done in.
	watchDone := c.Done()
	for running {
		if drawErr = drawTUI(out, st, m, notice, cols, scrRows, time.Now().UnixMilli()); drawErr != nil {
			break
		}
		select {
		case ns := <-states:
			st = ns
			if want != "" {
				pointAt(&m, st, want, scrRows-3)
				want = ""
			}
		case b, ok := <-typed:
			if !ok {
				return nil // stdin died: nothing left to serve
			}
			for _, ev := range parser.feed(b) {
				handoff, done := apply(ev)
				if handoff != nil {
					attach = handoff
				}
				if done || handoff != nil {
					running = false
					break
				}
			}
			if running {
				armEsc()
			}
		case <-escQuiet:
			escQuiet = nil
			if ev, ok := takeEscape(&parser); ok {
				handoff, done := apply(ev)
				if handoff != nil {
					attach = handoff
				}
				running = !done && handoff == nil
			}
		case <-resize.C:
			if nc, nr, _, _, err := termPixels(fd); err == nil && (nc != cols || nr != scrRows) {
				cols, scrRows = nc, nr
			}
		case <-watchDone:
			// The watch died with its connection. One silent retry on a
			// fresh one — the list's acts need a connection anyway — and
			// past that the list goes on without live.
			watchDone = nil // a closed channel fires forever; stop reading it
			if resubbed {
				m.Live = false
				break
			}
			resubbed = true
			// The retry dials in the background: to an unreachable host
			// connect gives itself thirty seconds, and the picture must
			// not freeze for one of them — keys and frames go on, and
			// the fresh connection slots in when it comes.
			go func() {
				nc, done, err := connect(host)
				if err != nil {
					resub <- resubResult{}
					return
				}
				nc.OnEvent(onState)
				live := nc.Call("watch", nil, nil) == nil
				resub <- resubResult{c: nc, done: done, live: live}
			}()
		case r := <-resub:
			if r.c == nil {
				m.Live = false // the retry reached no daemon at all
				break
			}
			resubDone = r.done
			c = r.c
			watchDone = c.Done() // the new connection's loss is watched now
			m.Live = r.live      // a watch that answers brings live back
		case <-sig:
			// Not the raw-mode ^C — that is a byte for the pane — but a
			// signal from outside: leave as a detach's, the deferred
			// cleanup taking the terminal back.
			return nil
		}
	}
	if drawErr != nil {
		return drawErr
	}
	if attach == nil {
		return nil
	}

	// Attach handoff. The watch is per-connection: closing the client
	// unsubscribes it, and run()'s deferred cleanup still runs. The alt
	// screen is left — mouse off, cursor back — but raw mode stays: the
	// pane's terminal wants it, and the deferred Restore has the shell.
	// Detach lands in the shell; the TUI never loops back.
	c.Close()
	var d client.Dialer = client.Local{}
	if host != "" {
		rc, err := dialHost(host)
		if err != nil {
			return err
		}
		defer rc.Close()
		d = rc
	}
	dial := func() (*client.Stream, error) { return client.AttachViaRaw(d, attach.Pane) }
	io.WriteString(out, tuiLeaveScreen)
	// The stdin pump turns to the pane's pipe from here; attachLoop reads
	// its end of it.
	paneMu.Lock()
	paneOut = pw
	paneMu.Unlock()
	close(handed)
	// The pane holds the loop for as long as the user stays in it, so the
	// signal wait moves with them: an outside ^C leaves the attach as it
	// left the list, the deferred cleanup taking the terminal back.
	attachDone := make(chan error, 1)
	go func() {
		attachDone <- attachLoop(dial, pr, os.Stdout, winchSignals(), func() (int, int, int, int, error) {
			return termPixels(fd)
		})
	}()
	select {
	case err := <-attachDone:
		return err
	case <-sig:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Small helpers of the loop
// ---------------------------------------------------------------------------

// resubResult is the background retry's answer: the connection it made
// (nil when it reached no daemon), its cleanup, and whether the watch on
// it answers — the live the footer tells from then on.
type resubResult struct {
	c    *client.Client
	done func()
	live bool
}

// tuiEscapeQuiet is how long a lone ESC waits for a byte proving it a
// prefix before the TUI takes it for the escape key. attach's own
// escapeQuiet measures gaps between local keystrokes; a phone SSH
// connection splits a mouse report by more than that, and its leading
// ESC misread as the key would quit the TUI mid-tap — so the TUI, where
// escape quits, waits longer.
const tuiEscapeQuiet = 50 * time.Millisecond

// loneEscape reports whether the parser holds nothing but an ESC — the
// one held state a keypress waits on, needing proof no sequence follows.
func loneEscape(p *inputParser) bool {
	return len(p.pending) == 1 && p.pending[0] == 0x1b
}

// takeEscape flushes that waiting ESC as the escape key the quiet
// timeout proved it to be.
func takeEscape(p *inputParser) (tuiEvent, bool) {
	if !loneEscape(p) {
		return tuiEvent{}, false
	}
	p.pending = nil
	return tuiEvent{Key: tuiKey{Name: "escape"}}, true
}

// pointAt stands the cursor on a session's first row — the jump a
// create makes to the session it brought in — through the same hold the
// steps use: the held row is that session.
func pointAt(m *tuiModel, st proto.State, session string, height int) {
	rows := visibleRows(st, m.Filter)
	m.held = tuiRow{Session: session}
	m.Cursor = holdCursor(rows, m.held, m.Cursor)
	clampView(m, len(rows), height)
}
