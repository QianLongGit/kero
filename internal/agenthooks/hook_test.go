package agenthooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"kero/internal/proto"
)

func testHook(t *testing.T, settings string) Hook {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // the marker goes there
	t.Setenv("KERO_HOME", filepath.Join(t.TempDir(), "kero home"))
	h := Hook{Agent: "claude", Events: claudeEvents, File: filepath.Join(t.TempDir(), "settings.json")}
	if settings != "" {
		if err := os.WriteFile(h.File, []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// decode parses what a settings file holds, for tests to walk it.
func decode(t *testing.T, path string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(read(t, path)), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// commands returns the command lines of an event's groups, in their
// order.
func commands(t *testing.T, v map[string]any, event string) []string {
	t.Helper()
	groups, ok := v["hooks"].(map[string]any)[event].([]any)
	if !ok {
		t.Fatalf("%s: no groups in:\n%s", event, v["hooks"])
	}
	var out []string
	for _, g := range groups {
		for _, hd := range g.(map[string]any)["hooks"].([]any) {
			if c, ok := hd.(map[string]any)["command"].(string); ok {
				out = append(out, c)
			}
		}
	}
	return out
}

// Installing keeps the rest of the settings, in their order, and the
// other hooks; installing again changes nothing; removing takes only
// kero's out, and the events it empties.
func TestHookInstallKeepsSettings(t *testing.T) {
	h := testHook(t, `{
  "model": "opus",
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "say done"}]}],
    "SessionStart": [{"matcher": "startup", "hooks": [{"type": "command", "command": "echo hi"}]}]
  },
  "env": {"A": "1"}
}
`)
	if st := h.State(); st != "missing" {
		t.Fatalf("state %s before install", st)
	}
	if err := h.Install(); err != nil {
		t.Fatal(err)
	}
	got := read(t, h.File)
	if h.State() != "current" {
		t.Fatalf("state %s after install:\n%s", h.State(), got)
	}
	if i, j, k := strings.Index(got, `"model"`), strings.Index(got, `"hooks"`), strings.Index(got, `"env"`); !(i < j && j < k) {
		t.Fatalf("keys out of order:\n%s", got)
	}
	for _, want := range []string{`"say done"`, `"echo hi"`, `"matcher": "startup"`, `"timeout": 10`, "kero home/bin/kero' hook claude", "hook claude 2>/dev/null || true"} {
		if !strings.Contains(got, want) {
			t.Fatalf("no %s in:\n%s", want, got)
		}
	}
	if err := h.Install(); err != nil {
		t.Fatal(err)
	}
	if again := read(t, h.File); again != got {
		t.Fatalf("installing again changed it:\n%s", again)
	}
	if err := h.Remove(); err != nil {
		t.Fatal(err)
	}
	removed := read(t, h.File)
	if strings.Contains(removed, "kero") || !strings.Contains(removed, `"echo hi"`) || !strings.Contains(removed, `"say done"`) {
		t.Fatalf("after remove:\n%s", removed)
	}
	if h.State() != "missing" {
		t.Fatalf("state %s after remove", h.State())
	}
}

// Another kero's hook, as of a moved KERO_HOME, is outdated, and setup
// replaces it: one group of kero's own per event, the old one gone.
func TestHookReplacesOutdated(t *testing.T) {
	h := testHook(t, `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "'/old/kero/bin/kero' hook claude", "timeout": 10}]}]}}`)
	if st := h.State(); st != "outdated" {
		t.Fatalf("state %s, want outdated", st)
	}
	if err := h.Install(); err != nil {
		t.Fatal(err)
	}
	got := read(t, h.File)
	if strings.Contains(got, "/old/kero") || strings.Count(got, "hook claude") != len(claudeEvents) {
		t.Fatalf("after install:\n%s", got)
	}
	if h.State() != "current" {
		t.Fatalf("state %s after install", h.State())
	}
}

// The events kero doesn't have yet go in beside the agent's own hooks,
// and its settings keep what kero doesn't know, value for value.
func TestHookEveryEventBesideForeign(t *testing.T) {
	h := testHook(t, `{
  "unknown": {"keep": [1, {"deep": true}]},
  "hooks": {
    "UserPromptSubmit": [{"matcher": "*", "hooks": [{"type": "command", "command": "log it"}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "say done"}]}]
  }
}`)
	if err := h.Install(); err != nil {
		t.Fatal(err)
	}
	v := decode(t, h.File)
	want := map[string]any{"keep": []any{float64(1), map[string]any{"deep": true}}}
	if !reflect.DeepEqual(v["unknown"], want) {
		t.Fatalf("unknown key changed: %v", v["unknown"])
	}
	for _, event := range claudeEvents {
		ours := 0
		for _, c := range commands(t, v, event) {
			if h.ours(c) {
				ours++
			}
		}
		if ours != 1 {
			t.Fatalf("%s: %d of kero's handlers in:\n%s", event, ours, read(t, h.File))
		}
	}
	// The agent's own hooks stay first in their events, as they were.
	if got := commands(t, v, "UserPromptSubmit"); len(got) != 2 || got[0] != "log it" {
		t.Fatalf("UserPromptSubmit's commands: %v", got)
	}
	if got := commands(t, v, "Stop"); len(got) != 2 || got[0] != "say done" {
		t.Fatalf("Stop's commands: %v", got)
	}
}

// Removal takes every group of kero's, of either command form, out: the
// events left empty go, those with the agent's own hooks stay.
func TestHookRemoveDropsEmptyEvents(t *testing.T) {
	const legacy = `'/old/kero/bin/kero' hook claude`
	h := testHook(t, `{"hooks": {
		"SessionStart": [{"hooks": [{"type": "command", "command": "`+legacy+`"}]}],
		"Notification": [{"hooks": [{"type": "command", "command": "`+legacy+`"}, {"type": "command", "command": "notify-send hi"}]}],
		"Stop": [{"hooks": [{"type": "command", "command": "say done"}]}]
	}}`)
	if err := h.Remove(); err != nil {
		t.Fatal(err)
	}
	v := decode(t, h.File)
	if _, ok := v["hooks"].(map[string]any)["SessionStart"]; ok {
		t.Fatalf("SessionStart stayed:\n%s", read(t, h.File))
	}
	if got := commands(t, v, "Notification"); len(got) != 1 || got[0] != "notify-send hi" {
		t.Fatalf("Notification's commands: %v", got)
	}
	if got := commands(t, v, "Stop"); len(got) != 1 || got[0] != "say done" {
		t.Fatalf("Stop's commands: %v", got)
	}
}

// ours tells kero's commands of both forms — guarded, and of the age
// before the guard — and no one else's.
func TestHookOurs(t *testing.T) {
	h := Hook{Agent: "claude", Events: claudeEvents}
	for _, command := range []string{
		"'/old/kero/bin/kero' hook claude",
		"'/new/kero/bin/kero' hook claude 2>/dev/null || true",
	} {
		if !h.ours(command) {
			t.Errorf("ours(%q) = false", command)
		}
	}
	for _, command := range []string{"kero hook codex", "'/x/bin/kero' hook", "say done"} {
		if h.ours(command) {
			t.Errorf("ours(%q) = true", command)
		}
	}
}

// The unix hook command carries its own guard: a kero that has moved or
// gone must fail no event of the agent.
func TestHookCommandGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the guard is shell syntax unix alone has")
	}
	h := testHook(t, "")
	want := "'" + Kero() + "' hook claude 2>/dev/null || true"
	if got := h.command(); got != want {
		t.Fatalf("command %s, want %s", got, want)
	}
}

// Without a settings file, install makes one, and remove takes away what
// it made.
func TestHookNewFile(t *testing.T) {
	h := testHook(t, "")
	if err := h.Install(); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(read(t, h.File)), &v); err != nil {
		t.Fatal(err)
	}
	if err := h.Remove(); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(read(t, h.File)); got != "{}" {
		t.Fatalf("after remove: %s", got)
	}
}

// Settings that aren't JSON are left alone.
func TestHookRefusesBadSettings(t *testing.T) {
	h := testHook(t, "{ // a comment\n}")
	if err := h.Install(); err == nil {
		t.Fatal("installed into settings it can't read")
	}
	if got := read(t, h.File); got != "{ // a comment\n}" {
		t.Fatalf("changed: %s", got)
	}
}

// Claude Code's hook takes every event kero handles, Codex's only
// SessionStart; each goes in only when the agent is there, or named.
func TestHooksForAgents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KERO_HOME", filepath.Join(home, "kero"))
	for _, env := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		t.Setenv(env, "")
	}
	if got := Hooks(""); len(got) != 1 || got[0].Agent != "claude" || !reflect.DeepEqual(got[0].Events, claudeEvents) || got[0].File != filepath.Join(home, ".claude", "settings.json") {
		t.Fatalf("with no agents installed: %+v", got)
	}
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	got := Hooks("")
	if len(got) != 2 || got[1].Agent != "codex" || !reflect.DeepEqual(got[1].Events, []string{"SessionStart"}) || got[1].File != filepath.Join(home, ".codex", "hooks.json") {
		t.Fatalf("with Codex installed: %+v", got)
	}
	if got := Hooks("agents"); len(got) != 1 || got[0].Agent != "codex" {
		t.Fatalf("agents: %+v", got)
	}
	for _, agent := range []string{"claude", "codex"} {
		if got := Hooks(agent); len(got) != 1 || got[0].Agent != agent {
			t.Fatalf("%s named: %+v", agent, got)
		}
	}
	if got := Hooks("pi"); len(got) != 0 {
		t.Fatalf("pi is the cli's, not this package's: %+v", got)
	}
}

// Ensure installs where the hooks are missing or outdated, and touches
// nothing when they are kero's own already; Remove and State speak for
// the package as a whole.
func TestEnsureAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the marker goes there
	t.Setenv("KERO_HOME", filepath.Join(t.TempDir(), "kero home"))
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex"))
	if st := State("claude"); st != "missing" {
		t.Fatalf("state %s before ensure", st)
	}
	if err := Ensure("claude"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "settings.json")
	first := read(t, file)
	if st := State("claude"); st != "current" {
		t.Fatalf("state %s after ensure:\n%s", st, first)
	}
	if err := Ensure("claude"); err != nil {
		t.Fatal(err)
	}
	if again := read(t, file); again != first {
		t.Fatal("ensuring again rewrote the settings")
	}
	if err := Remove("claude"); err != nil {
		t.Fatal(err)
	}
	if st := State("claude"); st != "missing" {
		t.Fatalf("state %s after remove:\n%s", st, read(t, file))
	}
}

// The marker an explicit removal leaves — `kero skill remove`, or the
// hooks.remove RPC — keeps the daemon's ensure from putting the hooks
// back: a removal stays until a setup takes the marker away. The marker
// is one for every agent: it tells of kero's hooks, not one agent's.
func TestEnsureMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // the marker goes there
	t.Setenv("KERO_HOME", filepath.Join(home, "kero"))
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex"))
	marker := filepath.Join(proto.Home(), "no-agent-hooks")
	settings := filepath.Join(dir, "settings.json")

	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Ensure("claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(settings); !os.IsNotExist(err) {
		t.Fatal("ensure installed the hooks the marker refuses:\n" + read(t, settings))
	}

	os.Remove(marker)
	if err := Setup("claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("setup left the removal marker")
	}
	if st := State("claude"); st != "current" {
		t.Fatalf("state %s after setup", st)
	}

	if err := Remove("claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("remove left no marker")
	}
	if st := State("claude"); st != "missing" {
		t.Fatalf("state %s after remove", st)
	}
	if err := Ensure("claude"); err != nil {
		t.Fatal(err)
	}
	if st := State("claude"); st != "missing" {
		t.Fatal("ensure healed the hooks past the marker:\n" + read(t, settings))
	}
}
