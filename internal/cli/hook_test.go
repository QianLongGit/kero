package cli

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"kero/internal/agenthooks"
	"kero/internal/client"
	"kero/internal/proto"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Pi's and OpenCode's plugins go in when the agent is installed, run
// Kero's own kero command, and come out whole.
func TestAgentPlugins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KERO_HOME", filepath.Join(home, "kero"))
	for _, env := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "PI_CODING_AGENT_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(env, "")
	}
	names := func() (out []string) {
		for _, it := range agentHooks("") {
			out = append(out, it.name())
		}
		return
	}
	if got := strings.Join(names(), ","); got != "claude" {
		t.Fatalf("with no agents installed: %s", got)
	}
	os.MkdirAll(filepath.Join(home, ".pi", "agent"), 0o755)
	os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o755)
	if got := strings.Join(names(), ","); got != "claude,pi,opencode" {
		t.Fatalf("with Pi and OpenCode: %s", got)
	}
	for _, it := range agentHooks("agents") {
		if it.state() != "missing" {
			t.Fatalf("%s: %s before install", it.name(), it.state())
		}
		if err := it.install(); err != nil {
			t.Fatal(err)
		}
		if it.state() != "current" {
			t.Fatalf("%s: %s after install", it.name(), it.state())
		}
		p := it.(agentPlugin)
		for name := range p.files {
			b := read(t, filepath.Join(p.dir, name))
			if strings.Contains(b, "__KERO__") || name != "package.json" && !strings.Contains(b, filepath.Join(home, "kero", "bin", "kero")) {
				t.Fatalf("%s/%s runs no kero:\n%s", it.name(), name, b)
			}
		}
		if err := it.remove(); err != nil {
			t.Fatal(err)
		}
		if it.state() != "missing" {
			t.Fatalf("%s: %s after remove", it.name(), it.state())
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "opencode", "plugins", "kero")); !os.IsNotExist(err) {
		t.Fatal("OpenCode's plugin directory stayed")
	}
	if _, err := os.Stat(filepath.Join(home, ".pi", "agent", "extensions")); err != nil {
		t.Fatal("Pi's extensions directory went with Kero's extension")
	}
}

// The params carry the event as the hook saw it, in the environment it
// runs in, for kerod to tell the pane by.
func TestAgentEventParams(t *testing.T) {
	const pane, tmux, tty = "p1", "/tmp/tmux-1000/default,1234,0", "/dev/pts/3"
	for _, in := range []hookInput{
		{Event: "SessionStart", Session: "cc81", Cwd: "/work", Source: "startup"},
		{Event: "UserPromptSubmit", Session: "cc81", Cwd: "/work", Message: "fix the bug"},
		{Event: "Notification", Session: "cc81", Message: "Claude needs your permission to use Bash"},
		{Event: "Stop", Session: "cc81", Cwd: "/work"},
		{Event: "SessionEnd", Session: "cc81"},
	} {
		want := proto.AgentEventParams{
			Pane: pane, Agent: "claude", Session: in.Session, Dir: in.Cwd,
			Event: in.Event, Message: in.Message, Tmux: tmux, TTY: tty,
		}
		if got := agentEventParams(in, "claude", pane, tmux, tty); got != want {
			t.Errorf("%s: got %+v, want %+v", in.Event, got, want)
		}
	}
	// Outside kero and tmux there is nothing to tell the pane by.
	if got := agentEventParams(hookInput{Event: "Stop"}, "claude", "", "", ""); got != (proto.AgentEventParams{Agent: "claude", Event: "Stop"}) {
		t.Errorf("outside kero: %+v", got)
	}
}

// The events hookCmd takes (hookEvents) and the ones setup installs for
// Claude Code (package agenthooks' claudeEvents) are two lists of the
// same thing: were they to drift apart, kero would install hooks it
// ignores, or handle events it never hooked. This pins them together.
func TestHookEventsMatchInstalled(t *testing.T) {
	hooks := agenthooks.Hooks("claude")
	if len(hooks) != 1 {
		t.Fatalf("claude's hooks: %+v", hooks)
	}
	if !slices.Equal(hooks[0].Events, hookEvents) {
		t.Fatalf("installed %v, hookCmd takes %v", hooks[0].Events, hookEvents)
	}
}

// hungDaemon listens on KERO_SOCKET, welcomes the client, and never
// answers its calls: a daemon that hangs, as one whose socket a hook's
// call would wait on forever.
func hungDaemon(t *testing.T) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "kerod.sock")
	t.Setenv("KERO_SOCKET", sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				if _, err := r.ReadString('\n'); err != nil { // the Hello
					return
				}
				fmt.Fprintf(c, "{\"version\":%d}\n", proto.Version) // the Welcome
				io.Copy(io.Discard, r)                              // then nothing
			}(conn)
		}
	}()
}

// A hook's call waits no longer than its deadline: a daemon that never
// answers is an error, so the hook goes silent instead of hanging the
// agent that runs it.
func TestHookCallDeadline(t *testing.T) {
	hungDaemon(t)
	c, err := client.Connect(true)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	old := hookDeadline
	hookDeadline = 50 * time.Millisecond
	defer func() { hookDeadline = old }()
	start := time.Now()
	if err := hookCall(c, "pane.agentEvent", proto.AgentEventParams{Event: "Stop"}, nil); err == nil {
		t.Fatal("a daemon that never answers was no error")
	} else if d := time.Since(start); d > time.Second {
		t.Fatalf("the call took %s, past its deadline", d)
	}
}
