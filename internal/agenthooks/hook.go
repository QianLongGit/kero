// Package agenthooks installs kero's hooks in coding agents' settings:
// one command per event kero takes, running `kero hook <agent>`. The
// machinery lives here, not in package cli, so that the daemon can share
// it — to ensure the hooks as it starts, and to install or remove them
// by RPC — where importing cli would be a cycle.
package agenthooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"kero/internal/clilink"
	"kero/internal/proto"
)

// claudeEvents are the events kero hooks Claude Code on, as its settings
// name them: the life of a conversation.
var claudeEvents = []string{"SessionStart", "UserPromptSubmit", "Notification", "Stop", "SessionEnd"}

// Hook is kero's hook in an agent's settings: a command per event,
// telling kerod what the agent's conversation is doing.
type Hook struct {
	Agent  string   // as `kero hook` takes it: "claude", "codex"
	Events []string // the events the command runs on
	File   string   // the JSON file holding the agent's hooks
}

// Kero is the kero command of Kero's own directory (package clilink),
// which stays where it is as Kero updates.
func Kero() string {
	exe := filepath.Join(proto.Dir(), "bin", "kero")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	return exe
}

// command is the hook's command line, of every event: kero's own
// command, on unix guarded so that a kero gone or moved fails no event
// of the agent it runs for.
func (h Hook) command() string {
	if runtime.GOOS == "windows" {
		return `"` + Kero() + `" hook ` + h.Agent
	}
	return "'" + strings.ReplaceAll(Kero(), "'", `'\''`) + "' hook " + h.Agent + " 2>/dev/null || true"
}

// ours tells kero's hook commands, of this kero or another's, guarded or
// of the age before the guard.
func (h Hook) ours(command string) bool {
	return strings.Contains(command, "kero") && strings.Contains(command, " hook "+h.Agent)
}

// State is "current" (every event runs kero's command, alone), "missing"
// (no event does) or "outdated" (some do, not all, or not alone).
func (h Hook) State() string {
	b, err := os.ReadFile(h.File)
	if err != nil {
		return "missing"
	}
	settings, err := parseObject(b)
	if err != nil {
		return "missing"
	}
	settled, ours := h.scan(settings)
	switch {
	case settled:
		return "current"
	case !ours:
		return "missing"
	}
	return "outdated"
}

// each calls fn with the event and command of each of kero's command
// handlers in settings, event by event.
func (h Hook) each(settings object, fn func(event, command string)) {
	hooks, _ := parseObject(settings.get("hooks"))
	for _, event := range h.Events {
		var groups []object
		json.Unmarshal(hooks.get(event), &groups)
		for _, g := range groups {
			var handlers []struct{ Type, Command string }
			json.Unmarshal(g.get("hooks"), &handlers)
			for _, hd := range handlers {
				if hd.Type == "command" && h.ours(hd.Command) {
					fn(event, hd.Command)
				}
			}
		}
	}
}

// scan tells of kero's handlers in settings: settled — every event has
// exactly kero's command, alone — and ours — any event has one of
// kero's, however many or old.
func (h Hook) scan(settings object) (settled, ours bool) {
	n, exact := make(map[string]int, len(h.Events)), make(map[string]bool, len(h.Events))
	h.each(settings, func(event, command string) {
		n[event]++
		exact[event] = exact[event] || command == h.command()
	})
	settled = true
	for _, event := range h.Events {
		settled = settled && n[event] == 1 && exact[event]
		ours = ours || n[event] > 0
	}
	return settled, ours
}

// Install puts the hook in the agent's settings, for every event it
// takes, in place of kero's others, keeping the rest of the file as it
// is.
func (h Hook) Install() error {
	LinkCLI()
	return h.edit(true)
}

// LinkCLI makes the kero command that hooks and plugins run, should kerod
// not have made it yet.
func LinkCLI() {
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		bin := filepath.Join(proto.Dir(), "bin")
		if clilink.Target(bin) == "" {
			clilink.Link(exe, bin)
		}
	}
}

// Remove takes kero's hooks out of the agent's settings, and the events
// they leave empty.
func (h Hook) Remove() error {
	if _, err := os.Stat(h.File); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return h.edit(false)
}

func (h Hook) edit(add bool) error {
	b, err := os.ReadFile(h.File)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	settings := object{}
	if len(bytes.TrimSpace(b)) > 0 {
		if settings, err = parseObject(b); err != nil {
			return fmt.Errorf("%s: %w", proto.ShortDir(h.File), err)
		}
	}
	hooks, err := parseObject(settings.get("hooks"))
	if err != nil {
		return fmt.Errorf("%s: hooks: %w", proto.ShortDir(h.File), err)
	}
	// Already there, alone, for every event: nothing to do. Else drop
	// kero's handlers, and the groups they leave empty, and add a group
	// of kero's own per event.
	settled, ours := h.scan(settings)
	if add && settled || !add && !ours {
		return nil
	}
	for _, event := range h.Events {
		if hooks, err = h.editEvent(hooks, event, add); err != nil {
			return err
		}
	}
	if len(hooks) > 0 {
		settings = settings.set("hooks", hooks.marshal())
	} else {
		settings = settings.del("hooks")
	}
	out := settings.marshal()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, out, "", "  "); err != nil {
		return err
	}
	pretty.WriteByte('\n')
	if bytes.Equal(pretty.Bytes(), b) {
		return nil
	}
	return WriteFileAtomic(h.File, pretty.Bytes())
}

// editEvent rewrites one event's groups with kero's handlers out, and a
// group of kero's own in when add is set: one handler, kero's command.
func (h Hook) editEvent(hooks object, event string, add bool) (object, error) {
	var groups []object
	if raw := hooks.get(event); raw != nil {
		if err := json.Unmarshal(raw, &groups); err != nil {
			return nil, fmt.Errorf("%s: hooks.%s: %w", proto.ShortDir(h.File), event, err)
		}
	}
	kept := groups[:0]
	for _, g := range groups {
		var handlers []json.RawMessage
		json.Unmarshal(g.get("hooks"), &handlers)
		n := len(handlers)
		handlers = slices.DeleteFunc(handlers, func(raw json.RawMessage) bool {
			var hd struct{ Type, Command string }
			json.Unmarshal(raw, &hd)
			return hd.Type == "command" && h.ours(hd.Command)
		})
		if n > 0 && len(handlers) == 0 {
			continue
		}
		if len(handlers) != n {
			g = g.set("hooks", Marshal(handlers))
		}
		kept = append(kept, g)
	}
	groups = kept
	if add {
		handler := object{{"type", Marshal("command")}, {"command", Marshal(h.command())}, {"timeout", Marshal(10)}}
		groups = append(groups, object{{"hooks", Marshal([]object{handler})}})
	}
	if len(groups) > 0 {
		return hooks.set(event, Marshal(groups)), nil
	}
	return hooks.del(event), nil
}

// Hooks returns kero's hooks for agent: Claude Code's, on every event
// kero takes, and Codex's, on SessionStart, when it is installed (or
// named). The plugins of Pi and OpenCode are the cli's, not this
// package's.
func Hooks(agent string) []Hook {
	home := proto.Home()
	dirOf := func(env string, def ...string) string {
		if d := os.Getenv(env); d != "" {
			return d
		}
		return filepath.Join(append([]string{home}, def...)...)
	}
	claude := dirOf("CLAUDE_CONFIG_DIR", ".claude")
	codex := dirOf("CODEX_HOME", ".codex")
	installed := func(dir string) bool {
		st, err := os.Stat(dir)
		return err == nil && st.IsDir()
	}
	all := []struct {
		dir  string
		hook Hook
	}{
		{claude, Hook{Agent: "claude", Events: claudeEvents, File: filepath.Join(claude, "settings.json")}},
		{codex, Hook{Agent: "codex", Events: []string{"SessionStart"}, File: filepath.Join(codex, "hooks.json")}},
	}
	var out []Hook
	for _, a := range all {
		switch {
		case agent == a.hook.Agent,
			a.hook.Agent == "claude" && agent == "",
			a.hook.Agent != "claude" && (agent == "" || agent == "agents") && installed(a.dir):
			out = append(out, a.hook)
		}
	}
	return out
}

// Ensure installs kero's hooks for agent where they are missing or
// outdated: what the daemon does as it starts, for agents to be hooked
// without anyone running `kero skill setup`.
func Ensure(agent string) error {
	for _, h := range Hooks(agent) {
		if err := h.Install(); err != nil {
			return err
		}
	}
	return nil
}

// Remove takes kero's hooks for agent out of its settings.
func Remove(agent string) error {
	for _, h := range Hooks(agent) {
		if err := h.Remove(); err != nil {
			return err
		}
	}
	return nil
}

// State is the state of kero's hooks for agent: "current" when every one
// is, "missing" when none is there, else "outdated".
func State(agent string) string {
	var states []string
	for _, h := range Hooks(agent) {
		states = append(states, h.State())
	}
	switch {
	case len(states) == 0:
		return "missing"
	case !slices.ContainsFunc(states, func(s string) bool { return s != "current" }):
		return "current"
	case !slices.ContainsFunc(states, func(s string) bool { return s != "missing" }):
		return "missing"
	}
	return "outdated"
}

// object is a JSON object that keeps its keys' order, so that editing a
// settings file leaves the rest of it as it was.
type object []member

type member struct {
	Key string
	Val json.RawMessage
}

func parseObject(b []byte) (object, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return object{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var o object
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o = o.set(t.(string), v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("more than one JSON value")
	}
	return o, nil
}

func (o *object) UnmarshalJSON(b []byte) error {
	p, err := parseObject(b)
	*o = p
	return err
}

func (o object) get(key string) json.RawMessage {
	for _, m := range o {
		if m.Key == key {
			return m.Val
		}
	}
	return nil
}

func (o object) set(key string, v json.RawMessage) object {
	for i, m := range o {
		if m.Key == key {
			o[i].Val = v
			return o
		}
	}
	return append(o, member{key, v})
}

func (o object) del(key string) object {
	return slices.DeleteFunc(o, func(m member) bool { return m.Key == key })
}

func (o object) marshal() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(Marshal(m.Key))
		b.WriteByte(':')
		b.Write(m.Val)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func (o object) MarshalJSON() ([]byte, error) { return o.marshal(), nil }

// Marshal is JSON as kero writes settings: compact, and with HTML left
// alone, so that paths with & or < stay readable. Exported for the
// plugin installs of package cli, which share it.
func Marshal(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return bytes.TrimSpace(b.Bytes())
}

// WriteFileAtomic replaces a file whole, keeping its mode: a settings
// file is either the old one or the new, never half. Exported for the
// plugin installs of package cli, which share it.
func WriteFileAtomic(path string, b []byte) error {
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
