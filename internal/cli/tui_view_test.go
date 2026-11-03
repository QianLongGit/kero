package cli

import (
	"strings"
	"testing"

	"kero/internal/proto"
)

// panes builds panes by id, the way the daemon lists them in state order.
func panes(ids ...string) []proto.Pane {
	out := make([]proto.Pane, len(ids))
	for i, id := range ids {
		out[i] = proto.Pane{ID: id}
	}
	return out
}

// tuiRows lists sessions by how much the user just used them, the same
// order a bare attach enters, and spreads multi-pane sessions into an
// indented pane row under each.
func TestTUIRowsOrder(t *testing.T) {
	st := proto.State{Sessions: []proto.Session{
		{ID: "sc", Panes: panes("pc"), Activity: 10},
		{ID: "sa", Panes: panes("pa"), Activity: 30},
		{ID: "sb", Panes: panes("pb"), Activity: 20},
		// Ties with sa on activity: the smaller id comes first.
		{ID: "sz", Panes: panes("pz"), Activity: 30},
	}}
	var got []string
	for _, r := range tuiRows(st) {
		got = append(got, r.Session)
	}
	want := []string{"sa", "sz", "sb", "sc"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tuiRows order = %v, want %v", got, want)
	}
}

// A single-pane session reads as one row; only sessions with more panes
// list them, indented, in state order.
func TestTUIRowsPanes(t *testing.T) {
	st := proto.State{Sessions: []proto.Session{
		{ID: "one", Panes: panes("p1"), Activity: 20},
		{ID: "many", Panes: panes("p1", "p2", "p3"), Activity: 10},
	}}
	rows := tuiRows(st)
	if len(rows) != 5 {
		t.Fatalf("tuiRows gave %d rows, want 5: %+v", len(rows), rows)
	}
	want := []struct {
		session string
		pane    string
		indent  bool
	}{
		{"one", "", false},
		{"many", "", false},
		{"many", "p1", true},
		{"many", "p2", true},
		{"many", "p3", true},
	}
	for i, w := range want {
		if rows[i] != (tuiRow{Session: w.session, Pane: w.pane, Indent: w.indent}) {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], w)
		}
	}
}

// An empty state is an empty list, not a panic.
func TestTUIRowsEmpty(t *testing.T) {
	if rows := tuiRows(proto.State{}); len(rows) != 0 {
		t.Errorf("tuiRows of empty state gave %d rows, want 0", len(rows))
	}
}

// tuiRows must not reorder the state it was given: later renders sort the
// same slice again.
func TestTUIRowsDoesNotMutate(t *testing.T) {
	st := proto.State{Sessions: []proto.Session{
		{ID: "sb", Panes: panes("pb"), Activity: 1},
		{ID: "sa", Panes: panes("pa"), Activity: 2},
	}}
	tuiRows(st)
	if st.Sessions[0].ID != "sb" || st.Sessions[1].ID != "sa" {
		t.Errorf("tuiRows reordered the caller's state: %s, %s",
			st.Sessions[0].ID, st.Sessions[1].ID)
	}
}

// The filter matches text the user can see on the row, case-insensitively:
// a session's own name, title and directory, or a pane's title, directory
// and process.
func TestTUIRowMatches(t *testing.T) {
	st := proto.State{Sessions: []proto.Session{
		{
			ID:   "s",
			Name: "work",
			Pane: proto.Pane{Title: "notes", Dir: "/tmp/x"},
			Panes: []proto.Pane{
				{ID: "p1", Title: "KERO.md", Dir: "/tmp/x"},
				{ID: "p2", Process: "cargo build", Dir: "/tmp/y"},
			},
		},
	}}
	s := &st.Sessions[0]
	for _, tt := range []struct {
		name   string
		paneID string
		filter string
		want   bool
	}{
		{"empty filter matches a session row", "", "", true},
		{"empty filter matches a pane row", "p1", "", true},
		{"session name, case-insensitive", "", "WORK", true},
		{"session lead title", "", "notes", true},
		{"session dir", "", "tmp/x", true},
		{"pane title, case-insensitive", "p1", "kero", true},
		{"pane process", "p2", "cargo", true},
		{"pane dir", "p2", "tmp/y", true},
		{"unrelated text misses a session row", "", "zzz", false},
		{"unrelated text misses a pane row", "p1", "zzz", false},
		{"session name does not leak into a pane row", "p1", "work", false},
		{"pane process does not leak into the session row", "", "cargo", false},
	} {
		if got := rowMatches(s, tt.paneID, tt.filter); got != tt.want {
			t.Errorf("%s: rowMatches(%q, %q) = %v, want %v",
				tt.name, tt.paneID, tt.filter, got, tt.want)
		}
	}
}

// A row's main name falls back the way the sidebar's does: the session's
// user name, else its lead pane's title; a pane's title, else its process,
// else its id.
func TestTUIRowLabel(t *testing.T) {
	st := proto.State{Sessions: []proto.Session{
		{
			ID:   "s",
			Name: "mine",
			Pane: proto.Pane{Title: "lead title"},
			Panes: []proto.Pane{
				{ID: "p1", Title: "pane title"},
				{ID: "p2", Process: "vim"},
				{ID: "p3"},
			},
		},
		{
			ID:   "anon",
			Pane: proto.Pane{Title: "lead title"},
			Panes: []proto.Pane{
				{ID: "q1"},
			},
		},
	}}
	for _, tt := range []struct {
		name    string
		session int
		paneID  string
		want    string
	}{
		{"session row uses the name when set", 0, "", "mine"},
		{"session row falls back to the lead title", 1, "", "lead title"},
		{"pane row uses the title", 0, "p1", "pane title"},
		{"pane row falls back to the process", 0, "p2", "vim"},
		{"pane row ends at its id", 0, "p3", "p3"},
	} {
		s := &st.Sessions[tt.session]
		if got := rowLabel(s, tt.paneID); got != tt.want {
			t.Errorf("%s: rowLabel = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// Every status shows one dot; only the color tells them apart, and idle
// and unknown stay uncolored.
func TestTUIStatusDot(t *testing.T) {
	for _, s := range []proto.Status{proto.Idle, proto.Working, proto.Waiting, proto.Failed, proto.Exited, proto.Status("bogus")} {
		if dot := statusDot(s); dot == "" {
			t.Errorf("statusDot(%q) is empty", s)
		}
	}
	if working, failed := statusDot(proto.Working), statusDot(proto.Failed); working == failed {
		t.Errorf("working and error dots are the same: %q", working)
	}
	for _, tt := range []struct {
		status proto.Status
		want   string
	}{
		{proto.Working, "\x1b[32m●\x1b[0m"},
		{proto.Waiting, "\x1b[33m●\x1b[0m"},
		{proto.Failed, "\x1b[31m●\x1b[0m"},
		{proto.Exited, "\x1b[90m●\x1b[0m"},
		{proto.Idle, "●"},
		{proto.Status("bogus"), "●"},
	} {
		if got := statusDot(tt.status); got != tt.want {
			t.Errorf("statusDot(%q) = %q, want %q", tt.status, got, tt.want)
		}
	}
}

// humanSince writes an age the way the sidebar's clock does, coarse to
// the unit it fits; a missing time (0) has no age to show.
func TestTUIHumanSince(t *testing.T) {
	const hr = 60 * 60 * 1000
	for _, tt := range []struct {
		name string
		now  int64
		t    int64
		want string
	}{
		{"no time", 50_000, 0, ""},
		{"negative time", 50_000, -1, ""},
		{"30 seconds", 31_000, 1_000, "30s"},
		{"a minute and a half", 91_000, 1_000, "1m"},
		{"three hours", 1_000 + 3*hr, 1_000, "3h"},
		{"two days and change", 1_000 + 50*hr, 1_000, "2d"},
	} {
		if got := humanSince(tt.now, tt.t); got != tt.want {
			t.Errorf("%s: humanSince(%d, %d) = %q, want %q",
				tt.name, tt.now, tt.t, got, tt.want)
		}
	}
}

// The footer tells the keys of the mode the user is in; only the list
// says when the state is not live.
func TestTUIFooter(t *testing.T) {
	list := "enter attach · n new · s split · d close · r rename · / filter · q quit"
	for _, tt := range []struct {
		name string
		mode tuiMode
		live bool
		want string
	}{
		{"list, live", tuiList, true, list},
		{"list, not live", tuiList, false, "live off · " + list},
		{"filter", tuiFilter, true, "enter apply · esc clear"},
		{"prompt", tuiPrompt, true, "enter run · esc cancel"},
		{"confirm", tuiConfirm, true, "y close · n/esc cancel"},
	} {
		if got := footer(tt.mode, tt.live); got != tt.want {
			t.Errorf("%s: footer = %q, want %q", tt.name, got, tt.want)
		}
	}
	if got := footer(tuiFilter, false); strings.Contains(got, "live off") {
		t.Errorf("filter mode footer carries the live prefix: %q", got)
	}
}
