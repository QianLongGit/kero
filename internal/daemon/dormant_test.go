package daemon

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"kero/internal/proto"
	"kero/internal/settings"
)

// writeSavedState writes the state of a daemon that ended, and each pane's
// history beside it, as the next one's restore finds them.
func writeSavedState(t *testing.T, d *Daemon, st savedState, history map[string]string) {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.statePath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(history) > 0 {
		if err := os.MkdirAll(filepath.Join(d.dir, "sessions"), 0o700); err != nil {
			t.Fatal(err)
		}
		for id, h := range history {
			if err := os.WriteFile(d.historyPath(id), []byte(h), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// dormantState is one session with one terminal pane that ran a tmux
// client, as the last daemon left it.
func dormantState(dir string) savedState {
	return savedState{
		Version:  2,
		Projects: []proto.Project{{ID: "pr", Name: "repo"}},
		Sessions: []savedSession{{
			ID: "ses", Project: "pr", Created: 1,
			Panes: []savedPane{{
				ID: "pn", Dir: dir, Created: 1, Cols: 80, Rows: 24,
				RestoreCmd: []string{"tmux", "attach", "-t", "dev"}, Tmux: true,
			}},
		}},
		Saved: 1,
	}
}

// A restore brings a terminal pane back dormant: its metadata and its
// history, but no process, with what it ran kept on it to run again when a
// client comes. Its session is in the daemon's — the count of them is what
// keeps it from exiting for idleness — and among the dormant ones.
func TestRestoreRegistersDormant(t *testing.T) {
	d := testDaemon(t)
	writeSavedState(t, d, dormantState(t.TempDir()), map[string]string{"pn": "$ tail -f log\r\n"})
	d.restore()

	if len(d.sessions) == 0 {
		t.Fatal("restored no sessions")
	}
	p := d.panes["pn"]
	if p == nil {
		t.Fatal("the pane was not registered")
	}
	if p.proc != nil {
		t.Error("the restored pane started a process")
	}
	if !p.info.Restored {
		t.Error("the restored pane does not tell it")
	}
	if text := p.readText(true, 0); !strings.Contains(text, "restored · session ended") {
		t.Errorf("the screen shows %q", text)
	}
	if !slices.Contains(d.dormant, d.sessions[0]) {
		t.Error("the session is not among the dormant ones")
	}
}

// What a dormant pane was restored with stays through a save — the 10s
// ticker's, say — so that a daemon restarting twice in a row still knows
// what to run again.
func TestDormantRetentionAcrossRestart(t *testing.T) {
	d := testDaemon(t)
	writeSavedState(t, d, dormantState(t.TempDir()), map[string]string{"pn": "$ tail -f log\r\n"})
	d.restore()
	d.saveState()

	r := testDaemon(t)
	r.dir = d.dir
	r.restore()
	p := r.panes["pn"]
	if p == nil {
		t.Fatal("the pane was not restored again")
	}
	if cmd := p.restoreCmd; !slices.Equal(cmd, []string{"tmux", "attach", "-t", "dev"}) {
		t.Errorf("the retained command = %q", cmd)
	}
	if !p.restoreTmux {
		t.Error("the retained tmux mark was lost")
	}
}

// paneEnv is the environment of a test's pane: a home of its own, with an
// empty .zshrc so zsh skips its new-user wizard, which would sit at its
// prompt and never run the pane's command.
func paneEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"HOME=" + home, "TERM=xterm-256color"}
}

// foreground waits until the pane's terminal foreground holds other than
// its shell's group — or the shell's own, when same — and fails the test
// when it never does, for the pane's login files take a moment first.
func foreground(t *testing.T, p *pane, same bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		fg := p.proc.Foreground()
		if fg != 0 && (fg == p.shellPID) == same {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the foreground stayed %d, the shell's group being %d", fg, p.shellPID)
		}
	}
}

// What a save records of a live pane: the program in its foreground, from
// the process itself — not a tmux client, not another directory — and
// nothing for a pane at its shell, whose foreground is the shell, with no
// command to run again.
func TestSaveRecordsForeground(t *testing.T) {
	d := testDaemon(t)
	env := paneEnv(t)
	ses, err := d.create(proto.CreateParams{Project: "pr", Dir: t.TempDir(), Command: []string{"sleep", "30"}, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	p := d.findPane(ses.Panes[0].ID)
	t.Cleanup(func() { p.close() })
	foreground(t, p, false)
	p.tick(time.Now())
	d.saveState()

	var st savedState
	if b, err := os.ReadFile(d.statePath()); err != nil {
		t.Fatal(err)
	} else if json.Unmarshal(b, &st) != nil {
		t.Fatal("the saved state is no JSON")
	}
	var sp savedPane
	for _, s := range st.Sessions {
		for _, q := range s.Panes {
			if q.ID == p.info.ID {
				sp = q
			}
		}
	}
	if cmd := sp.RestoreCmd; !slices.Equal(cmd, []string{"sleep", "30"}) {
		t.Errorf("the recorded command = %q", cmd)
	}
	if sp.Tmux {
		t.Error("sleep was taken for a tmux client")
	}
	if sp.RestoreDir != "" {
		t.Errorf("the recorded directory = %q, want the pane's own", sp.RestoreDir)
	}

	// A pane at its shell records nothing, for there is nothing to run
	// again: the foreground is the shell itself.
	bare, err := d.create(proto.CreateParams{Project: "pr", Dir: t.TempDir(), Env: env})
	if err != nil {
		t.Fatal(err)
	}
	q := d.findPane(bare.Panes[0].ID)
	t.Cleanup(func() { q.close() })
	foreground(t, q, true)
	q.tick(time.Now())
	d.saveState()
	if b, err := os.ReadFile(d.statePath()); err != nil {
		t.Fatal(err)
	} else if json.Unmarshal(b, &st) != nil {
		t.Fatal("the saved state is no JSON")
	}
	for _, s := range st.Sessions {
		if s.ID != bare.ID {
			continue
		}
		if cmd := s.Panes[0].RestoreCmd; len(cmd) != 0 {
			t.Errorf("a shell-alone pane recorded %q", cmd)
		}
	}
}

// A browser pane is no shell to start: it comes back as restored as it
// will ever be, and its session is no dormant one.
func TestRestoreBrowserPaneIsNotDormant(t *testing.T) {
	d := testDaemon(t)
	writeSavedState(t, d, savedState{
		Version:  2,
		Projects: []proto.Project{{ID: "pr", Name: "repo"}},
		Sessions: []savedSession{{
			ID: "ses", Project: "pr", Created: 1,
			Panes: []savedPane{{ID: "pn", URL: "https://kero.sh", Title: "Kero", Created: 1, Cols: 80, Rows: 24}},
		}},
		Saved: 1,
	}, nil)
	d.restore()

	if len(d.sessions) != 1 || len(d.sessions[0].panes) != 1 {
		t.Fatalf("restored %d sessions", len(d.sessions))
	}
	info := d.sessions[0].panes[0].info
	if info.URL != "https://kero.sh" || info.Title != "Kero" || !info.Restored {
		t.Errorf("restored %+v", info)
	}
	if len(d.dormant) != 0 {
		t.Errorf("%d dormant sessions, want none", len(d.dormant))
	}
}

// writeConfig writes the configuration, as the user's change between the
// daemon's boot and its panes' wake: materialize reads the settings again
// rather than what restore read.
func writeConfig(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(os.Getenv("KERO_CONFIG"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// dormantDaemon brings a saved state back and returns its pane, dormant,
// as a client then finds it.
func dormantDaemon(t *testing.T, st savedState) (*Daemon, *pane) {
	t.Helper()
	d := testDaemon(t)
	writeSavedState(t, d, st, map[string]string{"pn": "$ tail -f log\r\n"})
	d.restore()
	p := d.panes["pn"]
	if p == nil {
		t.Fatal("the pane was not registered")
	}
	t.Cleanup(func() { p.close() })
	return d, p
}

// What a pane runs again when a client comes for it: its agent's
// conversation first, then its tmux client, then any recorded command
// when the user asked for all of them — and nothing a state file could
// slip in past that order.
func TestMaterializeCommand(t *testing.T) {
	const id = "13b997c3-f8b3-4b3e-bb8a-ca1b5d1b9787"
	rerun := []string{"tmux", "attach", "-t", "dev"}
	for _, c := range []struct {
		name string
		set  settings.Settings
		sp   savedPane
		want []string
	}{
		{"the agent's conversation over anything recorded", settings.Settings{ResumeAgents: true},
			savedPane{Agent: &proto.AgentSession{Agent: "claude", ID: id}, RestoreCmd: rerun},
			[]string{"claude", "--resume", id}},
		{"an id the pattern refuses falls to the policy", settings.Settings{ResumeAgents: true, RestoreRerun: "all"},
			savedPane{Agent: &proto.AgentSession{Agent: "claude", ID: "x; rm -rf ~"}, RestoreCmd: rerun},
			rerun},
		{"no resuming, every command again", settings.Settings{RestoreRerun: "all"},
			savedPane{Agent: &proto.AgentSession{Agent: "claude", ID: id}, RestoreCmd: rerun},
			rerun},
		{"no resuming, nothing again", settings.Settings{RestoreRerun: "off"},
			savedPane{Agent: &proto.AgentSession{Agent: "claude", ID: id}, RestoreCmd: rerun},
			nil},
		{"the tmux client again, alone of the commands", settings.Settings{RestoreMultiplexer: true, RestoreRerun: "off"},
			savedPane{RestoreCmd: rerun, Tmux: true}, rerun},
		{"tmux off the setting, the policy still runs it", settings.Settings{RestoreRerun: "all"},
			savedPane{RestoreCmd: rerun, Tmux: true}, rerun},
		{"tmux off the setting, nothing again", settings.Settings{RestoreRerun: "off"},
			savedPane{RestoreCmd: rerun, Tmux: true}, nil},
		{"nothing recorded runs nothing", settings.Settings{RestoreMultiplexer: true, RestoreRerun: "all"},
			savedPane{}, nil},
		{"a browser pane starts nothing", settings.Settings{ResumeAgents: true, RestoreMultiplexer: true, RestoreRerun: "all"},
			savedPane{URL: "https://kero.sh"}, nil},
		{"nor a file pane", settings.Settings{RestoreRerun: "all"},
			savedPane{File: "notes.md"}, nil},
	} {
		if got := materializeCommand(c.set, c.sp); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// A dormant pane starts exactly once: materialize claims the pane under
// its lock, so a second one — a drive command's wake racing the wake of
// the rest, say — finds a process and leaves the shell it started.
func TestMaterializeStartsOnce(t *testing.T) {
	_, p := dormantDaemon(t, dormantState(t.TempDir()))
	writeConfig(t, "restore-multiplexer = false\n")
	if !p.materialize() {
		t.Fatal("materialize dropped the pane")
	}
	if p.proc == nil {
		t.Fatal("materialize started nothing")
	}
	shell := p.shellPID
	if !p.materialize() {
		t.Fatal("the second materialize dropped the pane")
	}
	if p.shellPID != shell {
		t.Errorf("the pane started again: shell %d, was %d", p.shellPID, shell)
	}
}

// A pane mid-start is claimed: a materialize that comes while another
// holds the claim — a drive command's wake racing the wake of the rest —
// starts nothing, and drops nothing.
func TestMaterializeLeavesClaimedPane(t *testing.T) {
	_, p := dormantDaemon(t, dormantState(t.TempDir()))
	p.mu.Lock()
	p.starting = true // as the wake that claimed it holds it
	p.mu.Unlock()
	if !p.materialize() {
		t.Fatal("materialize dropped a pane another wake was starting")
	}
	if p.proc != nil {
		t.Error("materialize started over another wake's claim")
	}
}

// A start that fails gives the claim back: the next wake tries again,
// rather than finding the pane claimed for a start that never came. The
// configured shell cannot run — no permission to execute — so the start
// fails with no shell to clean up after.
func TestMaterializeFailedStartRetries(t *testing.T) {
	_, p := dormantDaemon(t, dormantState(t.TempDir()))
	noexec := filepath.Join(t.TempDir(), "shell")
	if err := os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, "command = \""+noexec+"\"\n")
	if p.materialize() {
		t.Fatal("materialize claimed success with a shell that cannot run")
	}
	if p.materialize() {
		t.Fatal("materialize found the pane still claimed after its failed start")
	}
}

// A recorded command whose binary is gone leaves a pane at its shell: the
// interactive shell runs the command, it fails, and the session goes on
// at the prompt, as one the user typed and misspelled (shellArgs).
func TestMaterializeBogusCommandDegradesToShell(t *testing.T) {
	st := dormantState(t.TempDir())
	st.Sessions[0].Panes[0].RestoreCmd = []string{"/no/such/binary-ever"}
	st.Sessions[0].Panes[0].Tmux = false
	_, p := dormantDaemon(t, st)
	writeConfig(t, "restore-rerun = all\n")
	if !p.materialize() {
		t.Fatal("materialize dropped the pane")
	}
	if p.proc == nil {
		t.Fatal("materialize started nothing")
	}
	p.mu.Lock()
	closed, status := p.closed, p.info.Status
	p.mu.Unlock()
	if closed || status == proto.Exited {
		t.Errorf("the pane did not live past its failing command: closed=%v status=%s", closed, status)
	}
}

// The pane starts where its recorded command ran, when that was another
// directory than the pane's own.
func TestMaterializeUsesRestoreDir(t *testing.T) {
	dir := t.TempDir()
	st := dormantState(t.TempDir())
	st.Sessions[0].Panes[0].RestoreCmd = nil
	st.Sessions[0].Panes[0].Tmux = false
	st.Sessions[0].Panes[0].RestoreDir = dir
	_, p := dormantDaemon(t, st)
	if !p.materialize() {
		t.Fatal("materialize dropped the pane")
	}
	p.mu.Lock()
	got := p.info.Dir
	p.mu.Unlock()
	if got != dir {
		t.Errorf("the pane started in %q, want %q", got, dir)
	}
}

// restoreDormant brings d back with a session of the panes named, each a
// terminal holding nothing to run again: a wake starts each at its shell,
// and never a tmux client, which a machine's own server would take for
// real. The panes close with the test.
func restoreDormant(t *testing.T, d *Daemon, ids ...string) {
	t.Helper()
	st := savedState{Version: 2, Projects: []proto.Project{{ID: "pr", Name: "repo"}}, Saved: 1}
	s := savedSession{ID: "ses", Project: "pr", Created: 1}
	for _, id := range ids {
		s.Panes = append(s.Panes, savedPane{ID: id, Created: 1, Cols: 80, Rows: 24})
	}
	st.Sessions = append(st.Sessions, s)
	writeSavedState(t, d, st, nil)
	d.restore()
	t.Cleanup(func() {
		for _, p := range d.panes {
			p.close()
		}
	})
}

// controlClient dials d as a control connection, as the app's and the
// CLI's.
func controlClient(t *testing.T, d *Daemon) (net.Conn, *bufio.Reader) {
	t.Helper()
	return transferConn(t, d, "")
}

// rpcCall writes a request and reads its reply, past any events queued
// ahead of it.
func rpcCall(t *testing.T, conn net.Conn, r *bufio.Reader, id int64, method string, params any) proto.Message {
	t.Helper()
	writeJSON(t, conn, proto.Request{ID: id, Method: method, Params: params})
	for {
		var m proto.Message
		readJSON(t, r, &m)
		if m.ID == id {
			return m
		}
	}
}

// nextEvent reads the next event of the kind, past any replies.
func nextEvent(t *testing.T, r *bufio.Reader, event string) proto.Message {
	t.Helper()
	for {
		var m proto.Message
		readJSON(t, r, &m)
		if m.Event == event {
			return m
		}
	}
}

// statePane returns the pane's info as the state tells it.
func statePane(t *testing.T, st proto.State, id string) proto.Pane {
	t.Helper()
	for _, s := range st.Sessions {
		for _, p := range s.Panes {
			if p.ID == id {
				return p
			}
		}
	}
	t.Fatalf("no pane %s in the state", id)
	return proto.Pane{}
}

// A dormant pane starts for a client that shows it — an app subscribing,
// a client attaching — and for nothing else: kero ls reading the state
// must not ghost-attach the user's tmux sessions.
func TestStateRPCDoesNotWake(t *testing.T) {
	d := transferDaemon(t)
	restoreDormant(t, d, "pn")
	conn, r := controlClient(t, d)
	m := rpcCall(t, conn, r, 1, "state", nil)
	if m.Error != "" {
		t.Fatalf("state: %s", m.Error)
	}
	if pid := statePane(t, resultAs[proto.State](t, m), "pn").PID; pid != 0 {
		t.Errorf("the state tells the pane's pid %d, want none", pid)
	}
	if d.panes["pn"].proc != nil {
		t.Error("a state read started the pane")
	}
}

// kero events is a passive monitor: a watch wakes nothing, for a pane
// that ran a tmux client would attach the user's session from under it.
func TestWatchDoesNotWake(t *testing.T) {
	d := transferDaemon(t)
	restoreDormant(t, d, "pn")
	conn, r := controlClient(t, d)
	if m := rpcCall(t, conn, r, 1, "watch", nil); m.Error != "" {
		t.Fatalf("watch: %s", m.Error)
	}
	if d.panes["pn"].proc != nil {
		t.Error("a watch started the pane")
	}
}

// An app subscribing shows the panes: they start before the first state
// it gets, which tells of them started.
func TestSubscribeWakes(t *testing.T) {
	d := transferDaemon(t)
	restoreDormant(t, d, "pn")
	conn, r := controlClient(t, d)
	writeJSON(t, conn, proto.Request{ID: 1, Method: "subscribe"})
	m := nextEvent(t, r, "state")
	if pid := statePane(t, *m.State, "pn").PID; pid == 0 {
		t.Error("the first state tells no pane pid, want the started shell's")
	}
	if d.panes["pn"].proc == nil {
		t.Error("the subscribe started nothing")
	}
}

// A pane woken once stays at its shell: the second app's subscribe wakes
// nothing again.
func TestSecondClientNoDoubleStart(t *testing.T) {
	d := transferDaemon(t)
	restoreDormant(t, d, "pn")
	conn, r := controlClient(t, d)
	writeJSON(t, conn, proto.Request{ID: 1, Method: "subscribe"})
	nextEvent(t, r, "state")
	p := d.panes["pn"]
	if p.proc == nil {
		t.Fatal("the first subscribe started nothing")
	}
	shell := p.shellPID
	conn2, r2 := controlClient(t, d)
	writeJSON(t, conn2, proto.Request{ID: 2, Method: "subscribe"})
	if pid := statePane(t, *nextEvent(t, r2, "state").State, "pn").PID; pid != shell {
		t.Errorf("the second subscribe left the pane at pid %d, want its shell %d", pid, shell)
	}
	if p.shellPID != shell {
		t.Errorf("the pane started again: shell %d, was %d", p.shellPID, shell)
	}
}

// The attach Hello — a client showing the pane — wakes it.
func TestAttachHelloWakes(t *testing.T) {
	d := transferDaemon(t)
	restoreDormant(t, d, "pn")
	here, there := net.Pipe()
	go d.serve(there, identity{peer: "uid 501", via: "unix"})
	t.Cleanup(func() { here.Close() })
	writeJSON(t, here, proto.Hello{Kind: "attach", Session: "pn", Cols: 80, Rows: 24, Version: proto.Version})
	r := bufio.NewReader(here)
	var welcome proto.Welcome
	readJSON(t, r, &welcome)
	if welcome.Error != "" {
		t.Fatalf("welcome: %s", welcome.Error)
	}
	// The wake runs before serveAttach, so the pane's program is started
	// by the time the Welcome arrives; the frames that follow can wait.
	if d.panes["pn"].proc == nil {
		t.Error("the attach Hello started nothing")
	}
}

// A drive command wakes the pane it addresses, and no other: the rest
// wait for a client that shows them.
func TestSendWakesOnlyAddressedPane(t *testing.T) {
	d := transferDaemon(t)
	restoreDormant(t, d, "pn", "qb")
	conn, r := controlClient(t, d)
	if m := rpcCall(t, conn, r, 1, "send", proto.SendParams{ID: "pn", Data: "echo hi\r"}); m.Error != "" {
		t.Fatalf("send: %s", m.Error)
	}
	if d.panes["pn"].proc == nil {
		t.Error("the send woke nothing")
	}
	if d.panes["qb"].proc != nil {
		t.Error("the send woke a pane it does not address")
	}
	if len(d.dormant) != 1 {
		t.Errorf("%d dormant sessions, want the one left", len(d.dormant))
	}
}

// A pane whose wake fails leaves the daemon, but its history stays on
// disk: a wake that fails is a retry, not an end — the file goes with a
// close the user asked for, not with a start that failed. The
// configured shell cannot run, which fails the wake.
func TestWakeFailureKeepsHistory(t *testing.T) {
	d, p := dormantDaemon(t, dormantState(t.TempDir()))
	noexec := filepath.Join(t.TempDir(), "shell")
	if err := os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, "command = \""+noexec+"\"\n")
	d.wakePane(p)
	if d.panes["pn"] != nil {
		t.Fatal("the pane whose wake failed stayed in the daemon")
	}
	if _, err := os.Stat(d.historyPath("pn")); err != nil {
		t.Errorf("the pane's history went with its failed wake: %v", err)
	}
}
