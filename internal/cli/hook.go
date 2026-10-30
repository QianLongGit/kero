package cli

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"kero/internal/agenthooks"
	"kero/internal/client"
	"kero/internal/proto"
)

// hookCmd is what an agent's hooks run (the ones `kero skill setup`
// installs), on each event kero takes: it tells kerod what the agent's
// conversation is doing, and on SessionStart answers the agent that it
// runs in Kero, in which session and pane. It never fails the agent:
// outside Kero, or on any trouble, it says nothing.
func hookCmd(args []string) error {
	agent := ""
	if len(args) > 0 {
		agent = args[0]
	}
	if agent == "context" && len(args) >= 3 {
		return contextCmd(args[1], args[2], len(args) > 3 && args[3] == "wait")
	}
	var in hookInput
	json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&in)
	if in.AgentID != "" || !slices.Contains(hookEvents, in.Event) {
		return nil // a subagent's event, or one kero doesn't take
	}
	if t := os.Getenv("CODEX_THREAD_ID"); t != "" && in.Session != "" && t != in.Session {
		return nil // a Codex run by another agent in this pane, not the pane's
	}
	// One deadline, taken here, covers the hook's whole talk with kerod:
	// the connect and every call, so a daemon that hangs anywhere in it
	// leaves the hook silent well within the timeout of its entry in the
	// agent's settings.
	until := time.Now().Add(hookDeadline)
	c, err := hookConnect(until)
	if err != nil {
		return nil
	}
	defer c.Close()
	p := agentEventParams(in, agent, os.Getenv("KERO_PANE"), os.Getenv("TMUX"), controllingTTY())
	p.PID = os.Getpid()
	// kerod tells the pane the event is of: the environment may be that
	// of another, as when Codex's app server, which panes share, runs
	// the hook.
	var r proto.AgentEventResult
	if hookCall(c, until, "pane.agentEvent", p, &r) != nil {
		return nil
	}
	if in.Event != "SessionStart" {
		return nil
	}
	if in.Source != "resume" && in.Source != "compact" {
		if !forgetAgentName(c, until, r.Pane, r.Session) {
			return nil
		}
	}
	say, ok := hookContext(c, until, r.Pane, r.Session, r.Away)
	if !ok {
		return nil
	}
	out := map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName":     "SessionStart",
		"additionalContext": say,
	}}
	return json.NewEncoder(os.Stdout).Encode(out)
}

// hookInput is what a hook's stdin carries, of the event it runs on, as
// Claude Code's hook JSON has it.
type hookInput struct {
	Event   string `json:"hook_event_name"`
	AgentID string `json:"agent_id"`   // set for Claude Code's subagents
	Session string `json:"session_id"` // the agent's own
	Cwd     string `json:"cwd"`
	Source  string `json:"source"` // "startup", "resume", "clear", "compact"
	Message string `json:"message"`
}

// hookEvents are the events kero's hooks tell kerod of, by
// hook_event_name: the life of an agent's conversation.
var hookEvents = []string{"SessionStart", "UserPromptSubmit", "Notification", "Stop", "SessionEnd"}

// agentEventParams is a hook's input as kerod takes it, in the
// environment the hook runs in — its pane ($KERO_PANE), its tmux socket
// ($TMUX), its terminal — for kerod to tell the pane by. PID is the
// hook's own process, the caller's to set.
func agentEventParams(in hookInput, agent, pane, tmux, tty string) proto.AgentEventParams {
	return proto.AgentEventParams{
		Pane:    pane,
		Agent:   agent,
		Session: in.Session,
		Dir:     in.Cwd,
		Event:   in.Event,
		Message: in.Message,
		Tmux:    tmux,
		TTY:     tty,
	}
}

// hookDeadline bounds a hook's whole talk with kerod — its connect and
// every call, together — so a daemon that hangs anywhere in it can't
// hold the hook past the timeout of its entry in the agent's settings.
var hookDeadline = 2 * time.Second

// errHookLate is the hook's budget run out, whatever the talk was in the
// middle of: the hook goes silent, as it does on any trouble with kerod.
var errHookLate = errors.New("kerod did not answer in time")

// hookConnect opens kerod as a hook does, within the time left of the
// hook's budget: a daemon whose Welcome waits past that is one that
// hangs, the same silence as one that never answers. The connect the
// budget outran is closed as it ends, on the client's own deadline —
// the hook is on its way out.
func hookConnect(until time.Time) (*client.Client, error) {
	type opened struct {
		c   *client.Client
		err error
	}
	ch := make(chan opened, 1)
	go func() {
		c, err := client.Connect(true)
		ch <- opened{c, err}
	}()
	select {
	case got := <-ch:
		return got.c, got.err
	case <-time.After(time.Until(until)):
		go func() { // a late Welcome is a connection to close, not to use
			if got := <-ch; got.c != nil {
				got.c.Close()
			}
		}()
		return nil, errHookLate
	}
}

// hookCall calls a method of kerod as a hook does, within the time left
// of the hook's budget: a timeout is an error like any other, silence to
// the hook. Closing the client unbinds the call's goroutine from a
// socket that will never answer.
func hookCall(c *client.Client, until time.Time, method string, params, out any) error {
	ch := make(chan error, 1)
	go func() { ch <- c.Call(method, params, out) }()
	select {
	case err := <-ch:
		return err
	case <-time.After(time.Until(until)):
		c.Close()
		return errHookLate
	}
}

// controllingTTY names the controlling terminal the hook runs on
// (/dev/pts/3): what a tmux pane's program reads, for kerod to tell the
// kero pane that forwards it. "" when there is none, as under a daemon
// or CI, or its name can't be had.
func controllingTTY() string {
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return ""
	}
	defer tty.Close()
	return ttyName(tty)
}

// forgetAgentName drops the name an agent gave session sid, as a new
// conversation starts in its first pane, the one the name came from: the
// session shows the pane's title again. False tells the hook's budget
// ran out — the talk is over, silence; a call that merely fails it
// swallows, the name a nicety.
func forgetAgentName(c *client.Client, until time.Time, pane, sid string) bool {
	var st proto.State
	if err := hookCall(c, until, "state", nil, &st); err != nil {
		return !errors.Is(err, errHookLate)
	}
	for _, s := range st.Sessions {
		if s.ID == sid && s.AgentNamed && len(s.Panes) > 0 && s.Panes[0].ID == pane {
			hookCall(c, until, "rename", proto.RenameParams{ID: sid}, nil)
		}
	}
	return true
}

// contextCmd prints what to tell the agent of an agent's session, for an
// agent whose commands run away from its pane, as OpenCode's do in its
// service: nothing when no pane runs that session. wait gives a pane that
// runs the agent a moment to tell its session.
func contextCmd(agent, session string, wait bool) error {
	c, err := client.Connect(true)
	if err != nil {
		return nil
	}
	defer c.Close()
	for deadline := time.Now().Add(1500 * time.Millisecond); ; time.Sleep(150 * time.Millisecond) {
		var st proto.State
		if c.Call("state", nil, &st) != nil {
			return nil
		}
		pending := false
		for _, s := range st.Sessions {
			for _, p := range s.Panes {
				if a := p.AgentSession; a != nil && a.Agent == agent && a.ID == session {
					if say, ok := hookContext(c, deadline, p.ID, s.ID, true); ok {
						fmt.Println(say)
					}
					return nil
				}
				pending = pending || p.Agent == agent && (p.AgentSession == nil || p.AgentSession.ID != session)
			}
		}
		if !wait || !pending || time.Now().After(deadline) {
			return nil
		}
	}
}

// hookContext is what the hook tells an agent in pane, of session sid;
// c is kerod, nil when it is unreachable. away tells an agent whose
// commands don't run in its pane, and don't have its variables. ok is
// false when the hook's budget ran out — the talk is over, silence; a
// state call that merely fails leaves the words it has.
func hookContext(c *client.Client, until time.Time, pane, sid string, away bool) (string, bool) {
	var b strings.Builder
	if away {
		fmt.Fprintf(&b, "You are running in Kero, a terminal for coding agents: session %s, pane %s. "+
			"The `kero` command (see the kero skill) runs things beside you, such as a dev server, tests or another agent. "+
			"Your commands don't run in your pane and don't have its $KERO_SESSION and $KERO_PANE: give kero your pane as the target, "+
			"as in `kero split %s --down -- npm run dev`.", sid, pane, pane)
	} else {
		fmt.Fprintf(&b, "You are running in Kero, a terminal for coding agents: session %s, pane %s ($KERO_SESSION and $KERO_PANE). "+
			"The `kero` command (see the kero skill) runs things beside you, such as a dev server, tests or another agent, "+
			"and tells the user what you are doing.", sid, pane)
	}
	if c == nil {
		return b.String(), true
	}
	var st proto.State
	if err := hookCall(c, until, "state", nil, &st); err != nil {
		if errors.Is(err, errHookLate) {
			return "", false
		}
		return b.String(), true
	}
	for _, s := range st.Sessions {
		if s.ID != sid || len(s.Panes) == 0 {
			continue
		}
		switch {
		case s.AgentNamed:
			fmt.Fprintf(&b, " This conversation named the session %q: leave its name alone.", s.Name)
		case s.Name != "":
			fmt.Fprintf(&b, " The user knows this session as %q: leave its name alone.", s.Name)
		}
	}
	return b.String(), true
}

// integration is what kero installs in an agent, beside its skill, to
// hook it into Kero: a hook in its settings, or a plugin of its own.
type integration interface {
	name() string // the agent, as `kero hook` takes it
	path() string // where it goes
	state() string
	install() error
	remove() error
}

// hookIntegration is the cli's handle on package agenthooks — where the
// machinery for kero's hooks in agents' settings lives, shared with the
// daemon — as the integration the skill commands drive.
type hookIntegration struct {
	hook agenthooks.Hook
}

func (h hookIntegration) name() string    { return h.hook.Agent }
func (h hookIntegration) path() string    { return h.hook.File }
func (h hookIntegration) state() string   { return h.hook.State() }
func (h hookIntegration) install() error  { return h.hook.Install() }
func (h hookIntegration) remove() error   { return h.hook.Remove() }

// agentPlugin is a plugin kero installs for an agent: files of its own,
// in a directory the agent loads plugins from.
type agentPlugin struct {
	agent string
	dir   string            // where the agent loads it from
	files map[string]string // by name in dir: the asset (package agents)
	own   bool              // dir is the plugin's alone, to remove with it
}

//go:embed agents
var agentAssets embed.FS

func (p agentPlugin) name() string { return p.agent }

func (p agentPlugin) path() string {
	if p.own || len(p.files) != 1 {
		return p.dir
	}
	for name := range p.files {
		return filepath.Join(p.dir, name)
	}
	return p.dir
}

// content is a file as kero writes it: the asset, with the kero command
// it runs.
func (p agentPlugin) content(asset string) []byte {
	b, _ := agentAssets.ReadFile(asset)
	return bytes.ReplaceAll(b, []byte("__KERO__"), agenthooks.Marshal(agenthooks.Kero()))
}

func (p agentPlugin) state() string {
	same, there := 0, 0
	for name, asset := range p.files {
		b, err := os.ReadFile(filepath.Join(p.dir, name))
		if err != nil {
			continue
		}
		there++
		if bytes.Equal(b, p.content(asset)) {
			same++
		}
	}
	switch {
	case same == len(p.files):
		return "current"
	case there == 0:
		return "missing"
	}
	return "outdated"
}

func (p agentPlugin) install() error {
	agenthooks.LinkCLI()
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		return err
	}
	for name, asset := range p.files {
		if err := agenthooks.WriteFileAtomic(filepath.Join(p.dir, name), p.content(asset)); err != nil {
			return err
		}
	}
	return nil
}

func (p agentPlugin) remove() error {
	for name := range p.files {
		if err := os.Remove(filepath.Join(p.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if p.own {
		os.Remove(p.dir) // if nothing else is in it
	}
	return nil
}

// agentHooks returns what kero installs to hook agents into Kero: for
// Claude Code, and for Codex, Pi and OpenCode when they are installed (or
// named). The hooks of the settings files are package agenthooks', shared
// with the daemon; the plugins of Pi and OpenCode are the cli's own.
func agentHooks(agent string) []integration {
	var out []integration
	for _, h := range agenthooks.Hooks(agent) {
		out = append(out, hookIntegration{h})
	}
	home := proto.Home()
	dirOf := func(env string, def ...string) string {
		if d := os.Getenv(env); d != "" {
			return d
		}
		return filepath.Join(append([]string{home}, def...)...)
	}
	pi := dirOf("PI_CODING_AGENT_DIR", ".pi", "agent")
	opencode := filepath.Join(dirOf("XDG_CONFIG_HOME", ".config"), "opencode")
	installed := func(dir string) bool {
		st, err := os.Stat(dir)
		return err == nil && st.IsDir()
	}
	all := []struct {
		agent, dir string
		it         integration
	}{
		{"pi", pi, agentPlugin{agent: "pi", dir: filepath.Join(pi, "extensions"), files: map[string]string{"kero.ts": "agents/pi.ts"}}},
		{"opencode", opencode, agentPlugin{agent: "opencode", dir: filepath.Join(opencode, "plugins", "kero"), own: true, files: map[string]string{
			"package.json": "agents/opencode/package.json", "index.js": "agents/opencode/index.js", "tui.js": "agents/opencode/tui.js",
		}}},
	}
	for _, a := range all {
		switch {
		case agent == a.agent,
			(agent == "" || agent == "agents") && installed(a.dir):
			out = append(out, a.it)
		}
	}
	return out
}
