package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// tmux answers its list queries in tab-separated lines, a row a line;
// whatever else comes out of it — an empty line, a row short of its
// fields — is dropped rather than taken for data.
func TestTmuxParse(t *testing.T) {
	clients := parseClients("\n/dev/pts/3\tmain\n/dev/pts/5\twork\n\n/dev/pts/9\n")
	want := []tmuxClient{{TTY: "/dev/pts/3", Session: "main"}, {TTY: "/dev/pts/5", Session: "work"}}
	if !slices.Equal(clients, want) {
		t.Errorf("clients %+v, want %+v", clients, want)
	}

	panes := parsePanes(strings.Join([]string{
		"/dev/pts/4\tmain\t$2\t1\t1\tclaude\tclaude",
		"/dev/pts/6\tmain\t$2\t1\t0\tzsh\tzsh -l",
		"/dev/pts/7\tmain\t$2\t0\t0\tnode\tnode claude.js",
		"",
		"/dev/pts/8\twork\t$3",
		"junk",
		// Last line, its start command empty: the tab that ends it is a
		// field separator, not trailing whitespace.
		"/dev/pts/9\twork\t$3\t1\t1\tzsh\t",
	}, "\n"))
	wanted := []tmuxPane{
		{TTY: "/dev/pts/4", Session: "main", SessionID: "$2", Command: "claude", StartCommand: "claude", WindowActive: true, PaneActive: true},
		{TTY: "/dev/pts/6", Session: "main", SessionID: "$2", Command: "zsh", StartCommand: "zsh -l", WindowActive: true},
		{TTY: "/dev/pts/7", Session: "main", SessionID: "$2", Command: "node", StartCommand: "node claude.js"},
		{TTY: "/dev/pts/9", Session: "work", SessionID: "$3", Command: "zsh", WindowActive: true, PaneActive: true},
	}
	if !slices.Equal(panes, wanted) {
		t.Errorf("panes %+v, want %+v", panes, wanted)
	}
}

// The snapshot joins by tty: a client terminal tells the session its kero
// pane shows, a pane's tty the session it is in, and a session's id the
// name it goes by.
func TestTmuxSnapshotLookups(t *testing.T) {
	s := &tmuxSnapshot{
		Clients: []tmuxClient{{TTY: "/dev/pts/3", Session: "main"}},
		Panes: []tmuxPane{
			{TTY: "/dev/pts/6", Session: "main", SessionID: "$2", Command: "zsh", WindowActive: true},
			{TTY: "/dev/pts/7", Session: "main", SessionID: "$2", Command: "node"},
			{TTY: "/dev/pts/4", Session: "main", SessionID: "$2", Command: "claude", WindowActive: true, PaneActive: true},
			{TTY: "/dev/pts/8", Session: "work", SessionID: "$3", Command: "vim", PaneActive: true},
		},
	}
	if got := s.clientSession("/dev/pts/3"); got != "main" {
		t.Errorf("clientSession = %q", got)
	}
	if got := s.clientSession("/dev/pts/404"); got != "" {
		t.Errorf("clientSession of an unknown tty = %q", got)
	}
	panes := s.sessionPanes("main")
	if len(panes) != 3 || panes[0].TTY != "/dev/pts/4" {
		t.Errorf("sessionPanes = %+v", panes)
	}
	if panes := s.sessionPanes("nope"); len(panes) != 0 {
		t.Errorf("sessionPanes of an unknown session = %+v", panes)
	}
	if name, id := s.paneSession("/dev/pts/4"); name != "main" || id != "$2" {
		t.Errorf("paneSession = %q, %q", name, id)
	}
	if name, id := s.paneSession("/dev/pts/404"); name != "" || id != "" {
		t.Errorf("paneSession of an unknown tty = %q, %q", name, id)
	}
	if got := s.sessionName("$3"); got != "work" {
		t.Errorf("sessionName = %q", got)
	}
	if got := s.sessionName("$9"); got != "" {
		t.Errorf("sessionName of an unknown id = %q", got)
	}
}

// One snapshot stands for its ttl however often it is asked; a query that
// fails leaves the last one standing and starts the window anew, and the
// next query past that window takes up the topology again.
func TestTmuxCache(t *testing.T) {
	fail := false
	calls := 0
	clients := "/dev/pts/3\tmain"
	run := func(ctx context.Context, args ...string) (string, error) {
		calls++
		if fail {
			return "", errors.New("tmux is away")
		}
		if args[0] == "list-clients" {
			return clients, nil
		}
		return "/dev/pts/4\tmain\t$2\t1\t1\tclaude\tclaude", nil
	}
	c := tmuxCache{run: run, ttl: 2 * time.Second}
	now := time.Now()
	s := c.get(now)
	if calls != 2 || len(s.Clients) != 1 || len(s.Panes) != 1 || s.Panes[0].Command != "claude" {
		t.Fatalf("snapshot %+v after %d calls", s, calls)
	}
	c.get(now.Add(time.Second))
	if calls != 2 {
		t.Errorf("asked again within the ttl: %d calls", calls)
	}
	fail = true
	if kept := c.get(now.Add(3 * time.Second)); calls != 4 || kept != s {
		t.Fatalf("past the ttl: %d calls, snapshot %+v", calls, kept)
	}
	c.get(now.Add(4 * time.Second))
	if calls != 4 {
		t.Error("a failed query was retried within its window")
	}
	fail = false
	clients = "/dev/pts/3\tother"
	if fresh := c.get(now.Add(6 * time.Second)); calls != 6 || fresh == s || fresh.Clients[0].Session != "other" {
		t.Fatalf("recovered: %d calls, snapshot %+v", calls, fresh)
	}
}

// The zero cache — no runner, as a Daemon in tests stands — answers an
// empty snapshot rather than nothing.
func TestTmuxCacheZero(t *testing.T) {
	var c tmuxCache
	s := c.get(time.Now())
	if s == nil || len(s.Clients) != 0 || len(s.Panes) != 0 {
		t.Errorf("snapshot %+v", s)
	}
}

// The real runner asks a tmux binary it finds on PATH, and hands what it
// answers to the parsers: a fake tmux proves the queries and their
// formats.
func TestTmuxRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX shell")
	}
	bin := t.TempDir()
	script := `#!/bin/sh
case "$1" in
list-clients) printf '/dev/pts/3\tmain\n' ;;
*) printf '/dev/pts/4\tmain\t$2\t1\t1\tclaude\tclaude\n/dev/pts/5\tmain\t$2\t1\t0\tzsh\t\n' ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	c := tmuxCache{run: tmuxRun(), ttl: 2 * time.Second}
	s := c.get(time.Now())
	// The last pane's start command is empty, its line ending in the tab
	// that says so: the pane is data, not trailing whitespace to trim.
	if len(s.Clients) != 1 || s.Clients[0].Session != "main" ||
		len(s.Panes) != 2 || s.Panes[0].SessionID != "$2" || !s.Panes[0].WindowActive || s.Panes[0].Command != "claude" ||
		s.Panes[1].Command != "zsh" || s.Panes[1].StartCommand != "" {
		t.Fatalf("snapshot %+v", s)
	}
}

// A host without tmux — the common case — answers an empty snapshot
// through the real runner too: the lookup failure is a failed query like
// any other, and stands for its ttl.
func TestTmuxWithoutTmux(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH lookup differs")
	}
	t.Setenv("PATH", t.TempDir())
	c := tmuxCache{run: tmuxRun(), ttl: 2 * time.Second}
	now := time.Now()
	s := c.get(now)
	if len(s.Clients) != 0 || len(s.Panes) != 0 {
		t.Fatalf("snapshot %+v", s)
	}
	if c.get(now.Add(time.Second)) != s {
		t.Error("the empty snapshot did not stand for its ttl")
	}
}

// The pane that runs tmux does no work itself: the agent works in the
// session the pane's terminal watches. The session's panes answer in the
// order a user looks at them — the pane in front first, then a window no
// one looks at — and a pane running a runtime names its agent by how its
// shell started it.
func TestTmuxAgent(t *testing.T) {
	for _, test := range []struct {
		what string
		snap *tmuxSnapshot
		tty  string
		want string
	}{
		{
			what: "the active pane's agent",
			snap: &tmuxSnapshot{
				Clients: []tmuxClient{{TTY: "/dev/pts/3", Session: "main"}},
				Panes: []tmuxPane{
					{TTY: "/dev/pts/4", Session: "main", Command: "claude", WindowActive: true, PaneActive: true},
					{TTY: "/dev/pts/6", Session: "main", Command: "zsh", WindowActive: true},
				},
			},
			tty:  "/dev/pts/3",
			want: "claude",
		},
		{
			what: "another session's agent unseen",
			snap: &tmuxSnapshot{
				Clients: []tmuxClient{{TTY: "/dev/pts/3", Session: "main"}},
				Panes: []tmuxPane{
					{TTY: "/dev/pts/4", Session: "work", Command: "codex", WindowActive: true, PaneActive: true},
				},
			},
			tty:  "/dev/pts/3",
			want: "",
		},
		{
			what: "an agent behind the pane in front",
			snap: &tmuxSnapshot{
				Clients: []tmuxClient{{TTY: "/dev/pts/3", Session: "main"}},
				Panes: []tmuxPane{
					{TTY: "/dev/pts/4", Session: "main", Command: "codex", WindowActive: true, PaneActive: true},
					{TTY: "/dev/pts/6", Session: "main", Command: "claude"},
				},
			},
			tty:  "/dev/pts/3",
			want: "codex",
		},
		{
			what: "a runtime's script naming its agent",
			snap: &tmuxSnapshot{
				Clients: []tmuxClient{{TTY: "/dev/pts/3", Session: "main"}},
				Panes: []tmuxPane{
					{TTY: "/dev/pts/4", Session: "main", Command: "zsh", WindowActive: true, PaneActive: true},
					{TTY: "/dev/pts/6", Session: "main", Command: "node",
						StartCommand: "node /lib/node_modules/@anthropic-ai/claude-code/cli.js"},
				},
			},
			tty:  "/dev/pts/3",
			want: "claude",
		},
		{
			what: "no session on the tty",
			snap: &tmuxSnapshot{
				Clients: []tmuxClient{{TTY: "/dev/pts/9", Session: "main"}},
				Panes: []tmuxPane{
					{TTY: "/dev/pts/4", Session: "main", Command: "claude", WindowActive: true, PaneActive: true},
				},
			},
			tty:  "/dev/pts/3",
			want: "",
		},
		{
			what: "a session without an agent",
			snap: &tmuxSnapshot{
				Clients: []tmuxClient{{TTY: "/dev/pts/3", Session: "main"}},
				Panes: []tmuxPane{
					{TTY: "/dev/pts/4", Session: "main", Command: "zsh", WindowActive: true, PaneActive: true},
					{TTY: "/dev/pts/6", Session: "main", Command: "node", StartCommand: "node server.js"},
				},
			},
			tty:  "/dev/pts/3",
			want: "",
		},
	} {
		if got := tmuxAgent(test.snap, test.tty); got != test.want {
			t.Errorf("%s: tmuxAgent = %q, want %q", test.what, got, test.want)
		}
	}
}
