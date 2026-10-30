package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"kero/internal/proto"
)

// agentEventCall runs pane.agentEvent the way a hook does.
func agentEventCall(t *testing.T, d *Daemon, p proto.AgentEventParams) (proto.AgentEventResult, error) {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.call(nil, "pane.agentEvent", raw)
	if err != nil {
		return proto.AgentEventResult{}, err
	}
	return out.(proto.AgentEventResult), nil
}

// moveSession runs session.move the way a client does.
func moveSession(t *testing.T, d *Daemon, p proto.MoveSessionParams) error {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.call(nil, "session.move", raw)
	return err
}

func sessionByID(t *testing.T, d *Daemon, id string) proto.Session {
	t.Helper()
	for _, s := range d.state().Sessions {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no session %s in the state", id)
	return proto.Session{}
}

// hooks.setup and hooks.remove put kero's hooks in the agent's settings
// and take them out, answering the state they leave — the GUI drives
// them per host — and a remove leaves the marker that keeps the
// daemon's startup ensure from putting them right back; a setup beats
// the marker, for it is the user asking for the hooks.
func TestHooksRPC(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // the marker goes there
	t.Setenv("KERO_HOME", filepath.Join(home, "kero"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex"))
	d := testDaemon(t)
	call := func(method string) proto.HooksResult {
		t.Helper()
		raw, err := json.Marshal(proto.HooksParams{Agent: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		out, err := d.call(nil, method, raw)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		r, ok := out.(proto.HooksResult)
		if !ok {
			t.Fatalf("%s answered %T", method, out)
		}
		return r
	}
	if got := call("hooks.state").State; got != "missing" {
		t.Fatalf("state %s before setup", got)
	}
	if got := call("hooks.setup").State; got != "current" {
		t.Fatalf("state %s after setup", got)
	}
	if got := call("hooks.remove").State; got != "missing" {
		t.Fatalf("state %s after remove", got)
	}
	if _, err := os.Stat(filepath.Join(home, "no-agent-hooks")); err != nil {
		t.Fatal("remove left no marker")
	}
	if got := call("hooks.setup").State; got != "current" {
		t.Fatalf("state %s after setup over the marker", got)
	}
	if _, err := os.Stat(filepath.Join(home, "no-agent-hooks")); !os.IsNotExist(err) {
		t.Fatal("setup left the removal marker")
	}
}

// A pin set by session.move shows in the state, and survives a restart:
// the daemon stores it, the restarted one restores it.
func TestSessionMovePin(t *testing.T) {
	d := testDaemon(t)
	first, err := d.create(proto.CreateParams{URL: "localhost:3000"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.create(proto.CreateParams{URL: "localhost:3000"})
	if err != nil {
		t.Fatal(err)
	}
	if err := moveSession(t, d, proto.MoveSessionParams{Session: second.ID, Pin: first.ID}); err != nil {
		t.Fatal(err)
	}
	if got := sessionByID(t, d, second.ID).Pin; got != first.ID {
		t.Errorf("pin after move = %q, want %q", got, first.ID)
	}
	if err := moveSession(t, d, proto.MoveSessionParams{Session: second.ID}); err != nil {
		t.Fatal(err)
	}
	if got := sessionByID(t, d, second.ID).Pin; got != "" {
		t.Errorf("pin after unpin = %q, want none", got)
	}
	if err := moveSession(t, d, proto.MoveSessionParams{Session: second.ID, Pin: "head"}); err != nil {
		t.Fatal(err)
	}
	if got := sessionByID(t, d, second.ID).Pin; got != "head" {
		t.Errorf("pin to head = %q, want \"head\"", got)
	}
	d.saveState()

	r := testDaemon(t)
	r.dir = d.dir
	r.restore()
	if got := sessionByID(t, r, second.ID).Pin; got != "head" {
		t.Errorf("pin after restore = %q, want \"head\"", got)
	}
}

// session.move takes a session to another project of the same daemon, pin
// and membership both, and both survive a restart.
func TestSessionMoveProject(t *testing.T) {
	d := testDaemon(t)
	d.projects = append(d.projects, proto.Project{ID: "pr2", Name: "other"})
	s, err := d.create(proto.CreateParams{URL: "localhost:3000"})
	if err != nil {
		t.Fatal(err)
	}
	other := "pr2"
	if err := moveSession(t, d, proto.MoveSessionParams{Session: s.ID, Pin: "head", Project: &other}); err != nil {
		t.Fatal(err)
	}
	if got := sessionByID(t, d, s.ID); got.Project != "pr2" || got.Pin != "head" {
		t.Errorf("after move: project %q, pin %q", got.Project, got.Pin)
	}
	d.saveState()

	r := testDaemon(t)
	r.dir = d.dir
	r.restore()
	if got := sessionByID(t, r, s.ID); got.Project != "pr2" || got.Pin != "head" {
		t.Errorf("after restore: project %q, pin %q", got.Project, got.Pin)
	}
}

// A project the daemon does not know is an error, not a new project: a
// session cannot leave the kerod that owns its terminal. The failed move
// changes nothing, the pin included.
func TestSessionMoveMissingProject(t *testing.T) {
	d := testDaemon(t)
	s, err := d.create(proto.CreateParams{URL: "localhost:3000"})
	if err != nil {
		t.Fatal(err)
	}
	missing := "nope"
	if err := moveSession(t, d, proto.MoveSessionParams{Session: s.ID, Pin: "head", Project: &missing}); err == nil {
		t.Fatal("moved to a project the daemon does not know")
	}
	if got := sessionByID(t, d, s.ID); got.Project != "pr" || got.Pin != "" {
		t.Errorf("after the refused move: project %q, pin %q", got.Project, got.Pin)
	}
}

// A hook's event becomes the pane's record, in the grammar of the reports
// (OSC 7501) that the pane's other status comes by: a prompt starts work,
// a stop ends it, a notification words a wait for the user or a quiet
// turn, and the session's end clears what it told. What kero doesn't take
// — an event it never asked a hook for, or the session's start, which
// registers rather than reports — tells no record, a message past the
// reports' limit is cut at its edge, and the bidi it may carry dropped.
func TestEventRecord(t *testing.T) {
	long := strings.Repeat("a", 3000)
	for _, test := range []struct {
		what string
		p    proto.AgentEventParams
		want proto.Record
		ok   bool
	}{
		{
			what: "a prompt starts work",
			p:    proto.AgentEventParams{Event: "UserPromptSubmit"},
			want: proto.Record{State: "working", Progress: -1},
			ok:   true,
		},
		{
			what: "a permission notification blocks",
			p:    proto.AgentEventParams{Event: "Notification", Message: "Claude needs your permission to use Bash"},
			want: proto.Record{State: "blocked", Kind: "permission", Msg: "Claude needs your permission to use Bash", Progress: -1},
			ok:   true,
		},
		{
			what: "another notification words a quiet turn",
			p:    proto.AgentEventParams{Event: "Notification", Message: "waiting for your input"},
			want: proto.Record{State: "idle", Msg: "waiting for your input", Progress: -1},
			ok:   true,
		},
		{
			what: "a stop ends the turn",
			p:    proto.AgentEventParams{Event: "Stop"},
			want: proto.Record{State: "done", Progress: -1},
			ok:   true,
		},
		{
			what: "the session's end clears",
			p:    proto.AgentEventParams{Event: "SessionEnd"},
			want: proto.Record{State: "clear", Progress: -1},
			ok:   true,
		},
		{
			what: "a message's bidi is dropped",
			p:    proto.AgentEventParams{Event: "Notification", Message: "a‮ b"},
			want: proto.Record{State: "idle", Msg: "a b", Progress: -1},
			ok:   true,
		},
		{
			what: "a message past the limit is cut",
			p:    proto.AgentEventParams{Event: "Notification", Message: long},
			want: proto.Record{State: "idle", Msg: long[:2048], Progress: -1},
			ok:   true,
		},
		{
			what: "an event kero doesn't take",
			p:    proto.AgentEventParams{Event: "PreToolUse"},
			ok:   false,
		},
		{
			what: "a session's start registers, not reports",
			p:    proto.AgentEventParams{Event: "SessionStart"},
			ok:   false,
		},
	} {
		got, ok := eventRecord(test.p)
		if ok != test.ok {
			t.Errorf("%s: ok = %v, want %v", test.what, ok, test.ok)
			continue
		}
		if !test.ok {
			continue
		}
		test.want.Updated = got.Updated
		if got != test.want {
			t.Errorf("%s: record %+v, want %+v", test.what, got, test.want)
		}
		if got.Updated <= 0 {
			t.Errorf("%s: the record tells no time", test.what)
		}
	}
}

// An event's hook tells of its pane by where it runs: the pane it names,
// the pane whose terminal it reads, or — reading a pane of a tmux session —
// the pane whose tmux client watches that session, however many sessions
// share its name, for the pane's terminal tells the session exactly. The
// tmux socket the hook passes along tells nothing: attribution reads the
// terminal only. An agent inside tmux works beside kero's shells: away,
// its commands have no pane of theirs. An event from nowhere is an error,
// for a hook to shrug at.
func TestAgentEventAttribution(t *testing.T) {
	d := testDaemon(t)
	onTTY := func(tty string) *pane {
		t.Helper()
		s, err := d.create(proto.CreateParams{URL: "localhost:3000"})
		if err != nil {
			t.Fatal(err)
		}
		pn := d.findPane(s.Panes[0].ID)
		pn.mu.Lock()
		pn.info.TTY = tty
		pn.mu.Unlock()
		return pn
	}
	a, b := onTTY("/dev/pts/10"), onTTY("/dev/pts/20")
	where := func(t *testing.T, p proto.AgentEventParams) (pane string, away bool) {
		t.Helper()
		r, err := agentEventCall(t, d, p)
		if err != nil {
			t.Fatalf("%+v: %v", p, err)
		}
		return r.Pane, r.Away
	}
	// tmux: two sessions share the name "work"; the hook's terminal is a
	// pane of one, and a kero pane's terminal a client of it.
	d.tmux = tmuxCache{ttl: time.Minute, run: func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "list-clients" {
			return "/dev/pts/20\twork\n", nil
		}
		return "/dev/pts/99\twork\t$2\t1\t1\tclaude\tclaude\n" +
			"/dev/pts/98\twork\t$1\t0\t1\tclaude\tclaude\n", nil
	}}
	for _, test := range []struct {
		what string
		p    proto.AgentEventParams
		want *pane
		away bool
	}{
		{
			what: "the pane the event names",
			p:    proto.AgentEventParams{Pane: a.info.ID, Agent: "claude", Event: "Stop"},
			want: a,
		},
		{
			what: "the pane on the hook's terminal",
			p:    proto.AgentEventParams{Agent: "claude", Event: "Stop", TTY: "/dev/pts/10"},
			want: a,
		},
		{
			what: "a pane of a tmux session, by the client watching it",
			p:    proto.AgentEventParams{Agent: "claude", Event: "Stop", TTY: "/dev/pts/99"},
			want: b,
			away: true, // the agent works in tmux, beside kero's shells
		},
		{
			what: "the tmux socket it passes along tells nothing",
			p:    proto.AgentEventParams{Agent: "claude", Event: "Stop", TTY: "/dev/pts/99", Tmux: "/tmp/tmux-501/default,123,0"},
			want: b,
			away: true,
		},
	} {
		got, away := where(t, test.p)
		if got != test.want.info.ID || away != test.away {
			t.Errorf("%s: pane %s away %v, want %s away %v", test.what, got, away, test.want.info.ID, test.away)
		}
	}
	if _, err := agentEventCall(t, d, proto.AgentEventParams{Agent: "claude", Event: "Stop", TTY: "/dev/pts/77"}); err == nil {
		t.Error("an event from a terminal no pane reads told of a pane")
	}
}

// The event's record reaches the pane's report machinery, and the agent's
// session registers as pane.agentSession's call does: the pane an event
// tells of holds the record — the state shows the work, and the session's
// end clears it — a pane no detection named takes the agent's name, and
// SessionStart sets the session to resume.
func TestAgentEventApplies(t *testing.T) {
	d := testDaemon(t)
	s, err := d.create(proto.CreateParams{URL: "localhost:3000"})
	if err != nil {
		t.Fatal(err)
	}
	id := s.Panes[0].ID
	pn := d.findPane(id)
	paneOf := func() proto.Pane {
		t.Helper()
		return sessionByID(t, d, s.ID).Panes[0]
	}

	if _, err := agentEventCall(t, d, proto.AgentEventParams{Pane: id, Agent: "claude", Event: "UserPromptSubmit"}); err != nil {
		t.Fatal(err)
	}
	pn.mu.Lock()
	agent, recs := pn.info.Agent, slices.Clone(pn.info.Records)
	pn.mu.Unlock()
	if agent != "claude" {
		t.Errorf("the pane's agent = %q, want the event's", agent)
	}
	if len(recs) != 1 || recs[0].State != "working" {
		t.Errorf("the pane's records = %+v, want one of work", recs)
	}
	if st := paneOf().Status; st != proto.Working {
		t.Errorf("the pane's status = %q, want working", st)
	}

	r, err := agentEventCall(t, d, proto.AgentEventParams{Pane: id, Agent: "claude", Session: "sess1", Event: "SessionStart"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Pane != id || r.Session != s.ID || r.Away {
		t.Errorf("SessionStart answered %+v, want pane %s of session %s", r, id, s.ID)
	}
	pn.mu.Lock()
	as := pn.info.AgentSession
	pn.mu.Unlock()
	if as == nil || as.Agent != "claude" || as.ID != "sess1" {
		t.Errorf("the pane's agent session = %+v, want claude's sess1", as)
	}

	if _, err := agentEventCall(t, d, proto.AgentEventParams{Pane: id, Event: "SessionEnd"}); err != nil {
		t.Fatal(err)
	}
	if p := paneOf(); len(p.Records) != 0 || p.Status == proto.Working {
		t.Errorf("after the end: records %+v, status %q", p.Records, p.Status)
	}
}

// A state.json from before pins loads: its sessions come back unpinned,
// and an unpinned session tells no pin on the wire.
func TestSessionMoveOldState(t *testing.T) {
	d := testDaemon(t)
	state := `{"version":2,"projects":[{"id":"pr","name":"repo"}],` +
		`"sessions":[{"id":"ses","project":"pr","created":1,` +
		`"panes":[{"id":"pn","url":"https://kero.sh","created":1,"cols":80,"rows":24}]}],"saved":1}`
	if err := os.WriteFile(d.statePath(), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	d.restore()
	if len(d.sessions) != 1 {
		t.Fatalf("restored %d sessions, want one", len(d.sessions))
	}
	if got := sessionByID(t, d, "ses").Pin; got != "" {
		t.Errorf("a session from before pins restored with pin %q", got)
	}
	b, err := json.Marshal(d.state())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`"pin"`)) {
		t.Errorf("the state of an unpinned session tells a pin: %s", b)
	}
}
