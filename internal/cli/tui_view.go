package cli

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"kero/internal/proto"
)

// tuiMode is what the keyboard means right now: the list and its keys,
// the filter bar taking text, a prompt awaiting a command, or a confirm
// awaiting yes or no. Task 3's state machine moves between them; the
// footer here is all this file needs of them.
type tuiMode uint8

const (
	tuiList    tuiMode = iota // browsing sessions
	tuiFilter                 // typing a filter
	tuiPrompt                 // typing a command to run
	tuiConfirm                // asking yes or no
)

// tuiRow is one line of the TUI's list: a session, or — Pane set — one of
// its panes, indented under it. Single-pane sessions list no pane row:
// they read as one.
type tuiRow struct {
	Session string
	Pane    string // "" on the session's own row
	Indent  bool   // pane rows sit under their session
}

// tuiRows lays the state out as list rows: sessions in the order a bare
// attach enters — most recently active first, the smaller id winning a
// tie, pickAttach's comparator — then, under a session with more panes
// than one, those panes in state order. The state is sorted on a copy:
// the caller's slice keeps its order for the next render.
func tuiRows(st proto.State) []tuiRow {
	ss := append([]proto.Session(nil), st.Sessions...)
	slices.SortStableFunc(ss, func(a, b proto.Session) int {
		if la, lb := lastActive(a), lastActive(b); la != lb {
			return cmp.Compare(lb, la) // the more active first
		}
		return strings.Compare(a.ID, b.ID) // a tie to the smaller id
	})
	var rows []tuiRow
	for _, s := range ss {
		rows = append(rows, tuiRow{Session: s.ID})
		if len(s.Panes) > 1 {
			for _, p := range s.Panes {
				rows = append(rows, tuiRow{Session: s.ID, Pane: p.ID, Indent: true})
			}
		}
	}
	return rows
}

// rowMatches tells whether a row survives the filter: a case-insensitive
// substring over the text the row shows — the session's own name, its
// lead pane's title and its directory for a session row; the pane's
// title, directory and foreground process for a pane row. An empty
// filter keeps everything.
func rowMatches(s *proto.Session, paneID string, filter string) bool {
	if filter == "" {
		return true
	}
	needle := strings.ToLower(filter)
	var fields []string
	if paneID == "" {
		fields = []string{s.Name, s.Title, s.Dir}
	} else if p := s.PaneByID(paneID); p != nil {
		fields = []string{p.Title, p.Dir, p.Process}
	}
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), needle) {
			return true
		}
	}
	return false
}

// rowLabel is a row's main name: the session's user-given name, else its
// lead pane's title, for a session row; a pane's title, else its process,
// else its id, for a pane row — the same falls as the sidebar's.
func rowLabel(s *proto.Session, paneID string) string {
	if paneID == "" {
		if s.Name != "" {
			return s.Name
		}
		return s.Title
	}
	if p := s.PaneByID(paneID); p != nil {
		if p.Title != "" {
			return p.Title
		}
		if p.Process != "" {
			return p.Process
		}
		return p.ID
	}
	return ""
}

// statusDot is a row's status as one colored dot, the sidebar's signal
// language: green is busy, yellow waits for the user, red failed, dim
// exited — idle and anything unknown keep the terminal's own color.
func statusDot(s proto.Status) string {
	switch s {
	case proto.Working:
		return "\x1b[32m●\x1b[0m"
	case proto.Waiting:
		return "\x1b[33m●\x1b[0m"
	case proto.Failed:
		return "\x1b[31m●\x1b[0m"
	case proto.Exited:
		return "\x1b[90m●\x1b[0m"
	}
	return "●"
}

// humanSince writes an age coarse to the unit it fits — seconds under a
// minute, then minutes, hours, days — the sidebar's clock in text. A
// missing time (0 or less) has no age to show.
func humanSince(now, t int64) string {
	if t <= 0 {
		return ""
	}
	// Clocks can disagree a little; a slightly-future time is now.
	sec := (now - t) / 1000
	if sec < 0 {
		sec = 0
	}
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 60*60:
		return fmt.Sprintf("%dm", sec/60)
	case sec < 24*60*60:
		return fmt.Sprintf("%dh", sec/60/60)
	}
	return fmt.Sprintf("%dd", sec/60/60/24)
}

// footer is the bottom line's hint at what the keys do in the mode the
// user is in. Only the list carries the live state — once filtered away
// from it, a stale list is not the thing in hand.
func footer(mode tuiMode, live bool) string {
	switch mode {
	case tuiFilter:
		return "enter apply · esc clear"
	case tuiPrompt:
		return "enter run · esc cancel"
	case tuiConfirm:
		return "y close · n/esc cancel"
	}
	list := "enter attach · n new · s split · d close · r rename · / filter · q quit"
	if !live {
		return "live off · " + list
	}
	return list
}
