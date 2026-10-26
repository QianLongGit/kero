package main

import (
	"bytes"
	"os"
	"testing"

	"kero/internal/proto"
)

// jumpFixture is the state the resolver's rules play against: the remote
// session comes first, so a level that forgets its locality rule or its
// cascade lands on the remote shadow of a local pane.
func jumpFixture() (*proto.State, func(string) bool) {
	st := &proto.State{Sessions: []proto.Session{
		{
			ID: "sR",
			Panes: []proto.Pane{
				// pRemote shadows pAgent's device and directory; its agent
				// session is one only a remote pane holds.
				{ID: "pRemote", TTY: "/dev/ttys001", Dir: "/repo", Activity: 200,
					AgentSession: &proto.AgentSession{Agent: "claude", ID: "ses_2"}},
				{ID: "pRecent", Dir: "/repo", Activity: 9000},
			},
		},
		{
			ID: "sL",
			Panes: []proto.Pane{
				{ID: "pAgent", TTY: "/dev/ttys001", Dir: "/repo", Activity: 1000,
					AgentSession: &proto.AgentSession{Agent: "claude", ID: "ses_1"}},
				// pTwin shares pAgent's directory, older; pTie shares
				// pIdle's, as old. pBlank works in no directory at all.
				{ID: "pTwin", Dir: "/repo", Activity: 500},
				{ID: "pIdle", TTY: "/dev/ttys002", Dir: "/tmp", Activity: 100},
				{ID: "pTie", Dir: "/tmp", Activity: 100},
				{ID: "pBlank"},
			},
		},
	}}
	local := map[string]bool{"pAgent": true, "pTwin": true, "pIdle": true, "pTie": true, "pBlank": true}
	return st, func(id string) bool { return local[id] }
}

func TestResolveJump(t *testing.T) {
	st, local := jumpFixture()
	for _, tt := range []struct {
		name    string
		q       jumpQuery
		session string
		pane    string
	}{
		{"agent session", jumpQuery{session: "ses_1"}, "sL", "pAgent"},
		// The session level looks at any host's panes.
		{"agent session on a remote pane", jumpQuery{session: "ses_2"}, "sR", "pRemote"},
		// The device names a local pane, not its remote shadow.
		{"tty", jumpQuery{tty: "/dev/ttys001"}, "sL", "pAgent"},
		// A device only a remote pane has falls through to the directory.
		{"remote tty falls to cwd", jumpQuery{tty: "/dev/ttys009", cwd: "/repo"}, "sL", "pAgent"},
		{"cwd", jumpQuery{cwd: "/tmp"}, "sL", "pIdle"},
		// Local panes come first, the most recent of them: not the remote
		// pRecent, not the older local pTwin.
		{"cwd local before recent remote", jumpQuery{cwd: "/repo"}, "sL", "pAgent"},
		// Equal on locality and activity, the first pane in the state wins.
		{"cwd tie keeps state order", jumpQuery{cwd: "/tmp"}, "sL", "pIdle"},
		// An empty parameter skips its level: a pane with no directory is
		// not a match for no directory.
		{"empty cwd skips its level", jumpQuery{cwd: ""}, "", ""},
		{"no parameters", jumpQuery{}, "", ""},
		{"agent session prefix", jumpQuery{session: "ses_1x"}, "", ""},
		{"tty prefix", jumpQuery{tty: "/dev/ttys00"}, "", ""},
	} {
		if s, p := resolveJump(st, local, tt.q); s != tt.session || p != tt.pane {
			t.Errorf("%s: resolveJump(%+v) = (%q, %q), want (%q, %q)", tt.name, tt.q, s, p, tt.session, tt.pane)
		}
	}
}

func TestParseJumpURL(t *testing.T) {
	for _, tt := range []struct {
		name string
		url  string
		q    jumpQuery
		ok   bool
	}{
		{"decoded params",
			"kero://focus?session=a&tty=%2Fdev%2Fttys003&cwd=%2FUsers%2Ffoo%2Fmy%20project",
			jumpQuery{session: "a", tty: "/dev/ttys003", cwd: "/Users/foo/my project"}, true},
		{"uppercase host", "kero://FOCUS?cwd=/x", jumpQuery{cwd: "/x"}, true},
		{"uppercase scheme", "KERO://focus?cwd=/x", jumpQuery{cwd: "/x"}, true},
		{"other host", "kero://run?cwd=/x", jumpQuery{}, false},
		{"other scheme", "iterm://focus", jumpQuery{}, false},
		// Absent parameters and empty ones read the same.
		{"missing params", "kero://focus", jumpQuery{}, true},
		{"empty value", "kero://focus?cwd=", jumpQuery{}, true},
	} {
		q, ok := parseJumpURL(tt.url)
		if ok != tt.ok || q != tt.q {
			t.Errorf("%s: parseJumpURL(%q) = (%+v, %v), want (%+v, %v)", tt.name, tt.url, q, ok, tt.q, tt.ok)
		}
	}
}

// jumpApp is the app as a jump finds it: the tester's session, its panes
// told apart by their device, both of this machine, and no window — the
// cold-start shape a launch URL meets.
func jumpApp(t *testing.T) *App {
	t.Helper()
	a := testApp(t)
	a.st.Sessions[0].Panes[0].TTY = "/dev/ttys00a"
	a.st.Sessions[0].Panes[1].TTY = "/dev/ttys00b"
	h := &host{}
	a.owner = map[string]*host{"a": h, "b": h}
	return a
}

// A URL before any state waits for one to resolve it, jumping nowhere yet.
func TestOpenJumpColdStartWaits(t *testing.T) {
	a := jumpApp(t)
	a.loaded, a.selected = false, ""
	a.openJump("kero://focus?tty=/dev/ttys00b")
	if a.pending == nil || *a.pending != (jumpQuery{tty: "/dev/ttys00b"}) {
		t.Errorf("openJump before the state left %v pending, want the query", a.pending)
	}
	if a.selected != "" || a.focus["s1"] != "" {
		t.Errorf("openJump before the state selected %q, pane %q; want nothing yet", a.selected, a.focus["s1"])
	}
}

// A state that resolves the waiting jump goes to its pane, and consumes it.
func TestTakePendingJumpHits(t *testing.T) {
	a := jumpApp(t)
	a.selected = ""
	a.pending = &jumpQuery{tty: "/dev/ttys00b"}
	a.takePendingJump(&host{})
	if a.pending != nil {
		t.Error("takePendingJump left the jump pending after a hit")
	}
	if a.selected != "s1" {
		t.Errorf("takePendingJump selected %q, want s1", a.selected)
	}
	if a.focus["s1"] != "b" {
		t.Errorf("takePendingJump put the user in pane %q, want b", a.focus["s1"])
	}
}

// This machine's state settles a jump that nothing matches: it is spent,
// and nothing moves.
func TestTakePendingJumpDropsAfterLocalState(t *testing.T) {
	a := jumpApp(t)
	a.pending = &jumpQuery{tty: "/dev/ttys009"} // no pane has the device
	a.takePendingJump(&host{})
	if a.pending != nil {
		t.Error("takePendingJump kept the jump after this machine's state had none")
	}
	if a.selected != "s1" || a.focus["s1"] != "" || a.st.Sessions[0].Focus != "a" {
		t.Errorf("takePendingJump without a hit selected %q, pane %q, focus %q; want the state as it was",
			a.selected, a.focus["s1"], a.st.Sessions[0].Focus)
	}
}

// Another host's state cannot settle a jump that nothing matches: its
// pane may be in a state yet to come.
func TestTakePendingJumpKeepsForRemote(t *testing.T) {
	a := jumpApp(t)
	a.pending = &jumpQuery{tty: "/dev/ttys009"}
	a.takePendingJump(&host{name: "remote"})
	if a.pending == nil {
		t.Error("takePendingJump dropped the jump on another host's state")
	}
}

// The rule Kero writes for Vibe Island is the bytes its docs lock: the
// app and bundle it addresses, the focus URL and its parameters, indented
// as a page reads.
func TestVibeIslandRule(t *testing.T) {
	want := `{
  "displayName": "Kero",
  "bundleIdentifier": "sh.kero",
  "jumpRule": {
    "method": "urlScheme",
    "template": "kero://focus?session={session_id}&tty={tty}&cwd={cwd}"
  }
}`
	if got := string(vibeIslandRule("Kero", "sh.kero")); got != want {
		t.Errorf("vibeIslandRule() =\n%s\nwant\n%s", got, want)
	}
}

// The rule on disk lives its three states: none there until installed,
// current while it holds this Kero's bytes, outdated when something else
// is in its place, and gone again once removed.
func TestVibeIslandInstallState(t *testing.T) {
	home := t.TempDir()
	want := vibeIslandRule("Kero", "sh.kero")
	if s := vibeIslandRuleState(home, "kero.json", want); s != "missing" {
		t.Errorf("before install, state %q, want missing", s)
	}
	if err := installVibeIslandRule(home, "Kero", "kero.json", "sh.kero"); err != nil {
		t.Fatalf("installVibeIslandRule: %v", err)
	}
	if s := vibeIslandRuleState(home, "kero.json", want); s != "current" {
		t.Errorf("after install, state %q, want current", s)
	}
	b, err := os.ReadFile(vibeIslandRulePath(home, "kero.json"))
	if err != nil {
		t.Fatalf("reading the rule: %v", err)
	}
	if !bytes.Equal(b, want) {
		t.Errorf("the rule file holds %s, want the rule's bytes", b)
	}
	fi, err := os.Stat(vibeIslandRulePath(home, "kero.json"))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("the rule file's mode %v, err %v; want 0644", fi.Mode().Perm(), err)
	}
	if err := os.WriteFile(vibeIslandRulePath(home, "kero.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("overwriting the rule: %v", err)
	}
	if s := vibeIslandRuleState(home, "kero.json", want); s != "outdated" {
		t.Errorf("after an overwrite, state %q, want outdated", s)
	}
	if err := removeVibeIslandRule(home, "kero.json"); err != nil {
		t.Fatalf("removeVibeIslandRule: %v", err)
	}
	if s := vibeIslandRuleState(home, "kero.json", want); s != "missing" {
		t.Errorf("after removal, state %q, want missing", s)
	}
}
