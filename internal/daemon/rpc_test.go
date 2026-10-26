package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"kero/internal/proto"
)

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
