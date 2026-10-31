# tmux Claude Code Detection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make kero detect and report Claude Code sessions running inside tmux — agent identification through tmux topology, hook events over the socket, self-installing hooks that never disturb Claude Code.

**Architecture:** All daemon-side, as today's detection is. A cached tmux client/pane snapshot joins kero panes (by pty slave path, `Pane.TTY`) to tmux sessions, letting `tick()` name the agent behind a tmux client and a new `pane.agentEvent` RPC attribute hook reports to the right pane. `hookCmd` handles all managed Claude Code hook events with a 2s deadline and total silence on failure; the hook entries self-install into `~/.claude/settings.json` at daemon startup.

**Tech Stack:** Go 1.2x, stdlib `os/exec` (tmux queries), existing `internal/{daemon,cli,proto,client}` packages. No new dependencies.

**Spec:** `specs/2026-10-30-tmux-claude-detection-design.md`

## Global Constraints

- Code comments and doc comments are English prose explaining why, matching
  existing files. One feature per file where new files are created.
- No `proto.Version` bump — every protocol change is additive optional JSON.
- Silent-failure contract: every hook-side error path exits 0 with no output.
- settings.json edits are additive, atomic (`writeFileAtomic`), and preserve
  every foreign key and hook verbatim.
- Tests isolate with `t.Setenv("KERO_HOME", t.TempDir())` + temp `KERO_CONFIG`.
- Gates per task: `go vet ./...` and `go test ./internal/...`. Tasks 1-5
  leave the root package untouched; Task 6 touches only the two daemon entry
  lines; Task 7 (UI) touches the root package and adds its gates:
  `go test .` (first run downloads libghostty-vt) and `go tool mygo vet .`.
  Before Task 7, read `.agents/skills/mygo-maintenance/SKILL.md` (repo rule).
- Commit messages: English imperative, capitalized, no conventional-commit
  prefixes, no emoji. Land on `main` (repo memory: no feature branch).
- tmux queries: 1s exec deadline, ~2s TTL cache, no tmux → feature off.

## Review Focus

1. **settings.json with foreign hooks and keys** must survive an install
   with every foreign group and key preserved (the file is re-indented as
   today's edit already does) → Task 5, fixture test with
   foreign matcher groups and unknown top-level keys.
2. **Dead daemon or hanging RPC** must exit the hook 0 within ~2s, no output →
   Task 3, fake unix-socket listener that never replies.
3. **tmux binary absent or hanging** must leave detection exactly as today →
   Task 1, LookPath failure and exec-timeout tests.
4. **tmux client attached from a non-kero terminal** must attribute no pane →
   Task 4, snapshot fixture whose `client_tty` matches no `Pane.TTY`.
5. **Old single-event hook entry already in settings** must be replaced in
   place, one entry per event, no duplicates → Task 5, migration fixture from
   the current SessionStart-only format.

---

### Task 1: tmux topology snapshot — `internal/daemon/tmux.go`

**Files:**
- Create: `internal/daemon/tmux.go`
- Test: `internal/daemon/tmux_test.go`
- Modify: `internal/daemon/daemon.go` (Daemon struct + constructor: `tmux tmuxCache` field, initialized with the real runner and 2s TTL)

**Interfaces:**
- Produces (Task 2, 4 consume):
  - `type tmuxClient struct { TTY, Session string }`
  - `type tmuxPane struct { TTY, Session, SessionID, Command, StartCommand string; WindowActive, PaneActive bool }`
  - `type tmuxSnapshot struct { Clients []tmuxClient; Panes []tmuxPane }`
  - `func (s *tmuxSnapshot) clientSession(tty string) string` — session name of the client on tty, "" if none
  - `func (s *tmuxSnapshot) sessionPanes(session string) []tmuxPane` — active-window/active-pane first
  - `func (s *tmuxSnapshot) paneSession(tty string) (name, id string)`
  - `func (s *tmuxSnapshot) sessionName(id string) string`
  - `type tmuxCache struct { ...; run func(ctx context.Context, args ...string) (string, error); ttl time.Duration }`
  - `func (c *tmuxCache) get(now time.Time) *tmuxSnapshot` — cached ≤ ttl; on exec error keeps the last snapshot and restarts the ttl window; nil-safe (zero value returns empty snapshot)

- [ ] **Step 1: Write the failing tests**

Parser: `parseClients`/`parsePanes` (unexported) against fixture lines —
`/dev/pts/3\tmain`, `/dev/pts/4\tmain\t$2\t1\t1\tclaude\tclaude`, empty line,
line with too few fields (skipped), `window_active` `0` vs `1`. Lookups:
`clientSession` hit/miss, `sessionPanes` ordering (active-window active-pane
first), `paneSession`, `sessionName`. Cache: injected `run` + injected `now` —
fresh within ttl (one exec), refresh after ttl, exec error keeps prior
snapshot, `run == nil` returns empty snapshot.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/daemon/ -run TestTmux -v`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Implement `tmux.go`**

Exec format strings (tab-separated, `\t` literal in `-F`):
`list-clients -F '#{client_tty}\t#{client_session}'` and
`list-panes -a -F '#{pane_tty}\t#{session_name}\t#{session_id}\t#{window_active}\t#{pane_active}\t#{pane_current_command}\t#{pane_start_command}'`.
Default `run`: `exec.LookPath("tmux")` once (absent → empty snapshot, cached
through the ttl so no per-tick exec); else `exec.CommandContext` with a 1s
deadline, trimmed combined output. Add the `tmux tmuxCache` field to Daemon
and initialize it where the Daemon is constructed.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run TestTmux -v` → PASS; then
`go vet ./...` → clean.

- [ ] **Step 5: Commit**

`git add internal/daemon/tmux.go internal/daemon/tmux_test.go internal/daemon/daemon.go && git commit -m "Read tmux clients and panes for panes that attach to it"`

---

### Task 2: Agent identification through tmux — `tick()` wiring

**Files:**
- Modify: `internal/daemon/tmux.go` (add `tmuxAgent`), `internal/daemon/session.go:334-338`
- Test: `internal/daemon/tmux_test.go`

**Interfaces:**
- Consumes: Task 1 snapshot + `detectAgent(name string, args []string) string` (daemon.go:559).
- Produces: `func tmuxAgent(snap *tmuxSnapshot, paneTTY string) string` — agent name or "".

- [ ] **Step 1: Write the failing test**

Table over fixtures: pane on `clientSession` with claude as active pane →
"claude"; inactive window's claude found only when nothing active matches;
`pane_current_command` "node" resolved via `pane_start_command` fields
(`detectAgent(base(fields[0]), fields[1:])`); no session / no agent → "".

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestTmuxAgent -v` → FAIL.

- [ ] **Step 3: Implement and wire**

`tmuxAgent` walks `sessionPanes` in order: `detectAgent(p.Command, nil)`, and
for runtime names (`node bun python python3 deno`) `detectAgent` over the
split `StartCommand`. In `session.go` `tick()`, immediately before
`if agent != "" { proc = agent }` (session.go:335):

```go
if proc == "tmux" && agent == "" {
    if a := tmuxAgent(s.d.tmux.get(now), s.info.TTY); a != "" {
        agent, proc = a, a
    }
}
```

- [ ] **Step 4: Run tests**

`go test ./internal/daemon/ -v` → PASS (whole package — tick is shared).

- [ ] **Step 5: Commit**

`git add internal/daemon/tmux.go internal/daemon/tmux_test.go internal/daemon/session.go && git commit -m "Identify an agent inside a tmux session from its panes"`

---

### Task 3: hookCmd handles all events — `internal/cli/hook.go`, `internal/proto/proto.go`

**Files:**
- Modify: `internal/proto/proto.go` (params/result types), `internal/cli/hook.go:27-75`
- Test: `internal/cli/hook_test.go`

**Interfaces:**
- Consumes: `client.Connect(bool)`, `client.Client.Call`.
- Produces (Task 4 consumes):
  - `proto.AgentEventParams{PID int; Pane, Agent, Session, Dir, Event, Message, Tmux, TTY string}` (json: pid,pane,agent,session,dir,event,message,tmux,tty)
  - `proto.AgentEventResult{Pane, Session string; Away bool}` (json: pane,session,away)
  - RPC method name: `"pane.agentEvent"`
  - Local helpers: `func controllingTTY() string`, `func hookCall(c *client.Client, method string, params, out any)` (2s deadline; timeout is an error like any other)

- [ ] **Step 1: Write the failing tests**

`agentEventParams(in hookInput, env pane/tmux/tty) proto.AgentEventParams` —
pure builder, table per event. `hookCall` deadline: fake unix listener that
accepts and never replies → returns error within ~2s (tighten with a
package-level deadline var set to 50ms in the test). `controllingTTY()` under
no controlling terminal (CI) → "" without panic.

- [ ] **Step 2: Run tests to verify they fail**

`go test ./internal/cli/ -run 'TestAgentEvent|TestHookCall|TestControllingTTY' -v` → FAIL.

- [ ] **Step 3: Implement**

`hookCmd`: keep the stdin decode (add `Message string \`json:"message"\``),
drop the `pane == ""`/SessionStart-only gate. Act when
`in.AgentID == ""` and `in.Event` is one of `SessionStart, UserPromptSubmit,
Notification, Stop, SessionEnd`. Connect (`client.Connect(true)`; error →
return nil silently), build params
(`Pane: os.Getenv("KERO_PANE")`, `TTY: controllingTTY()`,
`Tmux: os.Getenv("TMUX")`, `PID: os.Getpid()`), `hookCall` it; error → nil.
On success: SessionStart keeps today's response flow (additionalContext via
`hookContext` + `forgetAgentName`); all other events print nothing. Keep the
`CODEX_THREAD_ID` guard. `controllingTTY`: open `/dev/tty`; Linux readlink
`/proc/self/fd/<fd>`, darwin `fcntl(F_GETPATH)`, else "".

- [ ] **Step 4: Run tests**

`go test ./internal/cli/ -v` → PASS.

- [ ] **Step 5: Commit**

`git add internal/proto/proto.go internal/cli/hook.go internal/cli/hook_test.go && git commit -m "Report agent events from Claude Code hooks through the socket"`

---

### Task 4: `pane.agentEvent` RPC — attribution and records

**Files:**
- Modify: `internal/daemon/rpc.go` (method dispatch), `internal/daemon/sessions.go` (attribution helper), `internal/daemon/osc.go` or new small file for the record map
- Test: `internal/daemon/rpc_test.go` (or the daemon test file holding RPC tests today)

**Interfaces:**
- Consumes: Task 1 snapshot, Task 3 params, `agentPane(pid, agent, dir)` (sessions.go:158), `pane.applyReport` (osc.go), `neutralize` (osc.go), `resumeCommand` (store.go).
- Produces: `func (d *Daemon) agentEvent(p proto.AgentEventParams) (proto.AgentEventResult, error)`; `func eventRecord(p proto.AgentEventParams) (proto.Record, bool)`.

- [ ] **Step 1: Write the failing tests**

`eventRecord` table: UserPromptSubmit→`{State:"working"}`;
Notification "…needs your permission…"→`{State:"blocked",Kind:"permission",Msg:neutralized}`;
Notification other→`{State:"idle",Msg}`; Stop→`{State:"done"}`;
SessionEnd→`{State:"clear"}`; unknown→ok=false; msg over 2048 bytes truncated
or rejected. Attribution (daemon harness with `d.tmux.run` injected): direct
`TTY == Pane.TTY`; tmux path `pane_tty → session → client_tty → pane`
(duplicate session names resolve via exact `pane_tty` first; the `Tmux`
param is informational and never parsed); no match → error. Application:
existing record machinery receives it (`info.Agent` bootstrapped when empty;
`AgentSession` set on SessionStart exactly as rpc.go:376-398 does today).

- [ ] **Step 2: Run tests to verify they fail**

`go test ./internal/daemon/ -run 'TestAgentEvent|TestEventRecord' -v` → FAIL.

- [ ] **Step 3: Implement**

Dispatch entry in `rpc.go` beside `pane.agentSession`. Attribution order:
explicit `p.Pane` id → direct TTY match over live panes → tmux
(`paneSession(p.TTY)` → clients of that session matched by TTY) →
`agentPane` fallback. SessionStart reuses the agentSession body (registration,
conversations note); other events `applyReport(eventRecord(p))` under
`pn.mu`, then `d.changed()`. Records set `Updated: time.Now().UnixMilli()`.

- [ ] **Step 4: Run tests**

`go test ./internal/daemon/ -v` → PASS.

- [ ] **Step 5: Commit**

`git add internal/daemon/rpc.go internal/daemon/sessions.go internal/daemon/ && git commit -m "Attribute agent events to panes through tmux topology"`

---

### Task 5: Multi-event install, guarded command, shared package — `internal/agenthooks`

**Files:**
- Create: `internal/agenthooks/hook.go`, `internal/agenthooks/hook_test.go` (moved + extended from internal/cli)
- Modify: `internal/cli/hook.go:176-344` (machinery moves out; thin wrappers stay), `internal/cli/hook.go:446` / `internal/cli/skill.go` (construct events, call into the package)
- Test: `internal/agenthooks/hook_test.go` (moved fixtures + new cases)

**Interfaces:**
- Produces: package `agenthooks` exporting the hook machinery for Task 6:
  `type Hook struct{Agent string; Events []string; File string}`, with
  `Install()/Remove()/State() string` methods and `Ensure(agent string) error`;
  claude constructed with `{"SessionStart","UserPromptSubmit","Notification","Stop","SessionEnd"}`, other agents `{"SessionStart"}`.
  `kero skill setup|remove|status` in cli keeps working through thin calls.

**Why the move:** Task 6's daemon RPC and startup ensure need this machinery;
daemon cannot import cli (cli imports daemon for `daemon serve`).

- [ ] **Step 1: Write the failing tests** (move existing fixtures from internal/cli/hook_test.go, then extend)

Foreign matcher groups and unknown top-level keys preserved (value-equal
after the existing re-indent); missing events added beside them; outdated
kero entries (old un-guarded command) replaced in place, one group per
event; `ours()` accepts guarded and legacy forms; removal drops every kero
group and empty events only.

- [ ] **Step 2: Run tests to verify they fail**

`go test ./internal/agenthooks/ -v` → FAIL (package missing).

- [ ] **Step 3: Implement — move and generalize**

Move `agentHook` + `edit/each/ours/state/install/remove/command` + the
`object`/`writeFileAtomic` helpers they need out of internal/cli/hook.go
into internal/agenthooks (exported thin surface per Interfaces), keeping
behavior identical. Then generalize: unix `command()` form
`'<kero>' hook <agent> 2>/dev/null || true` (Windows unchanged);
`ours()`: `Contains(command,"kero") && Contains(command," hook "+h.agent)`;
`each()` loops `h.events`; `edit()` loops events through a per-event helper,
single atomic write at the end. Set the claude `events` at the construction
site (hook.go:446). cli keeps wrappers so `kero skill setup|remove|status`
and its existing tests (moved where they test the machinery, kept where they
test the cli wiring) still pass.

- [ ] **Step 4: Run tests**

`go test ./internal/agenthooks/ ./internal/cli/ -v` → PASS.

- [ ] **Step 5: Commit**

`git add internal/agenthooks internal/cli/hook.go internal/cli/hook_test.go internal/cli/skill.go && git commit -m "Install the agent hook for every event it handles"`

---

### Task 6: Auto-ensure at daemon startup, RPC install/remove, removal marker

**Files:**
- Modify: `internal/agenthooks/` (marker + `Ensure`), `internal/daemon/rpc.go` (two RPC methods), `main.go:35-38`, `cmd/kero-server/main.go:29-32`
- Test: `internal/agenthooks/hook_test.go`, `internal/daemon/rpc_test.go`

**Interfaces:**
- Consumes: Task 5's `agenthooks` package (`Ensure(agent) error`, `Remove(agent) error`, `State(agent) string`).
- Produces: `func EnsureAgentHooks() error` semantics live in `agenthooks.Ensure`; RPC methods `"hooks.setup"`, `"hooks.remove"` and `"hooks.state"`, each taking `{agent string}` and returning `{state string}`; marker file `proto.Dir()/no-agent-hooks`.

- [ ] **Step 1: Write the failing tests**

Marker present → `Ensure` touches nothing; marker absent + temp
`CLAUDE_CONFIG_DIR` → all five events installed, second run is a no-op (file
unchanged); `kero skill setup` clears the marker, `kero skill remove --agent
claude` writes it; `hooks.setup`/`hooks.remove` RPC round-trip against a
test daemon (state result reflects current/missing).

- [ ] **Step 2: Run tests to verify they fail**

`go test ./internal/agenthooks/ ./internal/daemon/ -run 'TestEnsure|TestHooks' -v` → FAIL.

- [ ] **Step 3: Implement**

`agenthooks.Ensure`: marker → nil; else install claude (errors returned,
callers ignore). Marker written by remove, cleared by setup/install. RPC
cases in `rpc.go` beside the skill-facing methods: decode `{agent}`,
dispatch to the package (`hooks.setup` install, `hooks.remove` remove, 
`hooks.state` state), return `{state}`. Call `EnsureAgentHooks` in both
`__kerod` entries (`main.go:36`, `cmd/kero-server/main.go:30`) before
`daemon.Run`, ignoring the error.

- [ ] **Step 4: Run tests + full gates**

`go test ./internal/... ` → PASS; `go vet ./...` → clean;
`go build ./...` → clean.

- [ ] **Step 5: Commit**

`git add internal/agenthooks internal/daemon/rpc.go internal/daemon/rpc_test.go main.go cmd/kero-server/main.go && git commit -m "Ensure the Claude Code hooks at daemon startup"`

---

### Task 7: Hosts UI — hooks install/uninstall (Settings›Hosts + host context menu)

UI facts pinned by research (hosts_settings.go is the Hosts page, not hosts_view.go):

**Files:**
- Modify: `hosts_settings.go:154-213` (`hostRow`: add a hooks status row after the fixes-it row — label + state detail + Install/Remove buttons, the `settingRow`/skills-row pattern from settings_view.go:1111-1135), `hosts_view.go:231-273` (`hostMenu`: two items in the reconnect/restart group, `.Disabled(h.cl == nil || h.busy != "")`), `hosts_view.go:76-86` (`hostTag` gains `.ContextMenu(func(m *ui.Menu) { a.hostMenu(m, h) })` — today the menu only exists on the trouble-state hostHeader; this makes it reachable in the normal state), `locales/en.json` + `ja.json` + `zh-Hans.json` (new keys: `setHostHooks`, `setHostHooksDetail` with `{state}`-ish detail, `hostMenuInstallHooks`, `hostMenuRemoveHooks`; reuse `CommonInstall/CommonRemove` where they fit) + `go tool mygo generate`
- Create: `hosts_hooks.go` + `hosts_hooks_test.go` (state fetch + action dispatch, per repo rule one feature per file)
- Test: `hosts_hooks_test.go` (row-state mapping and dispatch, pure logic; `t.Setenv("KERO_HOME", t.TempDir())` pattern)

**Interfaces:**
- Consumes: Task 6's RPCs `hooks.state`/`hooks.setup`/`hooks.remove` (`{agent:"claude"}` → `{state}`), via `a.callOn(h, method, params, done)` (app.go:1329-1353) for actions; state fetch follows the `refreshService` pattern (settings_view.go:151-175: goroutine + RPC + `a.update` + `setWin.Invalidate()`), stored on `a.ss` keyed by host.
- Produces: per-host hooks row and menu actions; state string "current"/"outdated"/"missing" rendered with existing `SetSkill*` locale keys or the new detail key.

- [ ] **Step 1: Write the failing test** (`hosts_hooks_test.go`: state→detail mapping, action dispatch chooses setup vs remove RPC per current state, disconnected host → no call + toast)

- [ ] **Step 2: Run test to verify it fails** → FAIL

- [ ] **Step 3: Implement** (row in hostRow; hostMenu items; hostTag ContextMenu one-liner; `a.callOn` actions with post-action state re-fetch; locales + `go tool mygo generate`)

- [ ] **Step 4: Run tests + UI gates**

`go test .` → PASS (first run downloads libghostty-vt); `go tool mygo vet .` → clean; `go vet ./...` → clean.

- [ ] **Step 5: Commit**

`git add hosts_settings.go hosts_view.go hosts_hooks.go hosts_hooks_test.go locales && git commit -m "Install and remove agent hooks per host from the UI"`
