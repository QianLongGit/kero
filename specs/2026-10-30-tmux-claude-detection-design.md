# Claude Code inside tmux — Detection and Events — Design

Date: 2026-10-30
Status: approved approach (tmux identification + hook events), pending implementation

## Intent and why

Users run Claude Code inside tmux on remote hosts and attach to it from a kero
pane. Today kero is blind to those sessions. The pane's foreground process is
the tmux client, so `detectAgent` never fires and the whole detection stack —
screen rules, sidebar status, Needs-you notifications, changes panel — stays
off. tmux also swallows OSC 7501/133/9;4 and title sequences, and `kero
report`'s RPC fallback cannot attribute a pane (the pid walk ends at the tmux
server). The main detection input, the rendered screen, does arrive: the tmux
client forwards the grid 1:1. Two changes unlock what already exists: identify
the agent through tmux's own topology, and let Claude Code's hooks report
events over the unix socket with tmux-aware attribution. Hooks self-install
into the global settings.json, must never disturb Claude Code when kero is
broken, and must never damage the user's settings.

## Goals

- A pane whose foreground is a tmux client gets `Pane.Agent` from the tmux
  session's panes, so the existing screen-rule detection (package detect)
  lights up: sidebar status, Needs you, notifications, changes panel.
- Claude Code hooks (UserPromptSubmit, Notification, Stop, SessionEnd, plus
  the existing SessionStart) report to kerod through `$KERO_SOCKET`; kerod
  attributes the pane via tmux topology when `$KERO_PANE` is absent; state
  flows through the existing record pipeline (`pane.report`'s
  parseStatus/applyReport), keeping its priority over screen guesses.
- Hooks are ensured automatically: kerod at startup checks
  `~/.claude/settings.json` (respecting `CLAUDE_CONFIG_DIR`) and adds what is
  missing — additive, idempotent, non-destructive (every foreign key and hook
  survives), atomic write.
- The user can also drive hooks per host from the GUI: Settings›Hosts shows a
  hooks row (status + install/uninstall), and the host's context menu carries
  the same two actions. Both call `hooks.setup`/`hooks.remove` on that host's
  kerod — the same machinery the startup ensure uses.
- Silent-failure guarantee: any hook failure — kero binary missing, kerod
  dead, socket hanging — exits 0 with no output; Claude Code never sees an
  error and never waits past the entry's `timeout`.

## Non-goals / anti-goals

- No tmux session management UI (listing/attaching tmux sessions from the
  sidebar) — a separate project if ever wanted.
- No OSC passthrough work (`allow-passthrough`, `set-titles`): hooks plus
  screen rules cover state; progress and title stay missing inside tmux.
- No per-tool-call hooks (PreToolUse/PostToolUse): they sit in the tool
  call's synchronous path; the four new events above (with SessionStart)
  carry the state transitions.
- Never re-add hooks after an explicit `kero skill remove`: a removal marker
  is respected by the auto-ensure.
- No tmux dependency on hosts without tmux: every tmux query no-ops with a
  short timeout; the feature degrades to today's behavior.

## Constraints (verified in code)

- Detection is daemon-side. `pane.tick()` scans the pane's foreground process
  (session.go:314-359) and `detectAgent` matches it (daemon.go:559-590);
  `pane.look()` feeds the rendered screen to package detect
  (session.go:394-457); declared records outrank screen guesses
  (session.go:395-401).
- `Pane.TTY` already exists — the pane's pty slave path, set at each pane
  (re)start (proto.go:216-218, session.go:171). It is the join key to tmux's
  `client_tty`.
- `hookCmd` already parses `hook_event_name` from the hook payload but bails
  unless `$KERO_PANE` is set and the event is SessionStart (hook.go:35-44).
- The settings merge (`agentHook.edit`, hook.go:270-344) is atomic, preserves
  foreign content, drops kero's own stale handlers before adding fresh ones,
  and sets `timeout: 10` — but is hardcoded to `hooks.SessionStart`
  (hook.go:286, 325).
- `agentPane()` walks the pid parent chain (sessions.go:158-202); inside tmux
  it reaches the tmux server and misses.
- `pane.report` applies a parsed status record to a pane (rpc.go:347-363);
  `pane.agentSession` registers the agent's own session id (rpc.go:376-398).
- kerod strips `TMUX`/`TMUX_PANE` from pane environments (daemon.go:468-472),
  so a `$TMUX` value seen by a hook genuinely means tmux.
- Adding optional JSON-RPC params needs no `proto.Version` bump (repo rule).

## Approaches considered

- **A + B (chosen): tmux topology identification, plus hook events.** A alone
  is blind to non-visible tmux windows and limited by tmux's coarse
  `pane_current_command`; B alone cannot turn on the screen rules (the pane's
  agent must be known). Together each covers the other's gap, and both reuse
  existing machinery: detectAgent, the record pipeline, conversations.
- tmux passthrough configuration only — rejected: fixes no identification,
  needs user config, and `kero report` inside tmux still cannot attribute.
- kerod as a tmux control-mode client — rejected for now: a control-mode
  client buys per-pane fidelity the hooks deliver cheaper; revisit together
  with sidebar tmux management.
- transcript watching — rejected: kero has no file-watch infrastructure;
  hooks are push, not poll.

## The build

### 1. tmux topology snapshot — new `internal/daemon/tmux.go`

- `snapshot()` execs (context deadline ~1s, `PATH` lookup; no tmux → feature
  off) two queries and parses them into structs:
  - `tmux list-clients -F '#{client_tty}\t#{client_session}'`
  - `tmux list-panes -a -F '#{pane_tty}\t#{session_name}\t#{session_id}\t#{window_active}\t#{pane_active}\t#{pane_current_command}\t#{pane_start_command}'`
- Cached with a ~2s TTL, shared by all panes, refreshed only while at least
  one pane's foreground is `tmux`; any error keeps the last snapshot briefly,
  then yields empty — never fatal, never blocks a tick.
- Join keys: `client_tty` == `Pane.TTY` (string compare, both are pty slave
  paths on the same host); `pane_tty` == a reporting hook's tty.

### 2. Agent identification through tmux — `session.go` tick()

- In tick, when the foreground process is `tmux` and `agent == ""`: find the
  session whose client tty equals `s.info.TTY`, take that session's panes,
  and run `detectAgent` over each `pane_current_command` — for runtime names
  (node/bun/…) also over `pane_start_command`; prefer the active pane of the
  active window, else any pane that yields an agent. Set `agent`/`proc`
  exactly as a direct hit would (session.go:335-337, 350-359) so
  `detect.Known` turns on and the screen rules read the forwarded grid.
- When the tmux client exits (`proc == ""`), today's end-of-program cleanup
  (session.go:339-349) stands unchanged.

### 3. Hook command handles all events — `internal/cli/hook.go` hookCmd

- Drop the `$KERO_PANE`/SessionStart-only gate (hook.go:44): act on the
  managed events when either `$KERO_PANE` is set (direct pane, today's path)
  or `$TMUX` is set (tmux attribution). Keep the `agent_id` guard — subagent
  events stay silent.
- Dispatch, mapped onto the existing report states:
  - SessionStart → today's agentSession registration + additionalContext;
  - UserPromptSubmit → working;
  - Notification → message mentions permission → blocked (kind permission,
    message); otherwise → idle with the message;
  - Stop → done;
  - SessionEnd → clear.
- Best-effort tty: resolve the controlling terminal's name (Linux: readlink
  of an opened `/dev/tty` fd under /proc/self/fd; darwin: F_GETPATH; empty on
  failure) and pass `$TMUX` verbatim, both in the RPC params. `$TMUX`'s value
  (`socket,pid,index`) matches nothing in the snapshot — it is informational
  only; attribution keys off the tty.
- Deadlines: the hook's whole RPC phase — connect and every call — shares a
  short budget (≤2s), so the entry's `timeout: 10` never bites. Any error —
  connect, call, parse, or budget exhausted — exits 0 and prints nothing;
  only a successful SessionStart prints additionalContext, exactly as today.

### 4. Attribution and report — `internal/daemon/rpc.go`, `sessions.go`

- New method `pane.agentEvent` with params `{PID, Agent, Session, Dir, Event,
  Message, Tmux, TTY}` (additive JSON fields; no version bump).
- Attribution order:
  1. TTY equals a live pane's `Pane.TTY` (direct pane);
  2. TTY equals a snapshot `pane_tty` → that session's clients →
     `client_tty` equals a pane's TTY;
  3. today's `agentPane()` pid-walk heuristics;
  4. away: register the conversation only, as today.
- On attribution: set `info.Agent` when empty (an npm-run claude whose
  `pane_current_command` said "node" gets named here), register
  `AgentSession` on SessionStart as today, and apply the event through the
  existing `parseStatus`/`applyReport` path so one record pipeline serves
  OSC 7501, `kero report`, and hooks.

### 5. Multi-event install and guarded command — hook.go edit()/command()

- `agentHook` gains its event list (claude: SessionStart, UserPromptSubmit,
  Notification, Stop, SessionEnd; codex keeps SessionStart only);
  `edit()` generalizes the hardcoded
  "SessionStart" (hook.go:286, 325) to loop the events, per-event
  idempotence check, same drop-stale-then-add flow — an upgrade replaces the
  old single-event entry in place. `each()` (hook.go:226-239) iterates all
  managed events.
- Unix command line becomes `'<abs kero>' hook claude 2>/dev/null || true`:
  stderr gone, exit 0 even with the binary missing — Claude Code sees
  nothing. stdout untouched, so SessionStart's additionalContext still flows.
  Windows keeps today's unguarded form (cmd syntax differs; hookCmd itself
  never fails — known caveat).
- `h.ours()` (hook.go:199-201) and `state()` (hook.go:205-223) switch from
  suffix-equality to prefix matching so the guarded form and older forms all
  count as ours.

### 6. Auto-ensure, on-demand install, removal marker

- The hook install machinery (agentHook, edit/install/remove, command/ours,
  the marker) moves out of package cli into a shared package
  (`internal/agenthooks`) so the daemon's startup ensure and its RPC handlers
  reuse it without a daemon→cli import (cli imports daemon for `daemon
  serve`; the cycle forbids the reverse). `kero skill setup|remove` and its
  tests keep working through thin calls into the package.
- kerod startup (the `kero __kerod` / service entry) runs the same install as
  `kero skill setup --agent claude`: add missing events, refresh outdated
  command lines. Non-fatal, logged on failure, once per start. Local and
  remote hosts get it alike — same binary, same code path.
- New RPCs `hooks.setup`, `hooks.remove`, and `hooks.state` (params
  `{agent string}`, claude for now; result `{state string}` —
  current/outdated/missing) run the shared machinery on whichever kerod
  receives them; the GUI drives them per host. `remove` writes the marker,
  `setup` clears it.
- `kero skill remove --agent claude` and the RPC both write a marker (a
  `no-agent-hooks` file under `proto.Dir()` — kero's own directory, beside
  its socket and state, never a new file in the user's home) that the
  auto-ensure respects; re-enabling is an explicit install (setup clears the
  marker). [my call — self-healing must not fight an explicit removal]
- GUI (root package): Settings›Hosts (`hosts_settings.go` `hostRow`) gains a
  hooks state row (Install/Remove buttons, state fetched per host over
  `hooks.state` when the page opens, refreshService pattern), and the
  sidebar host context menu (`hostMenu`) gains the same two actions; the
  menu also becomes reachable in the normal state by attaching it to
  `hostTag` (today it exists only on the trouble-state host header).
- Removing a host (`removeHostConfirmation` → `removeHost`) first uninstalls
  that host's hooks (best-effort `hooks.remove`, bounded wait, never blocks
  or cancels the removal — the host leaves kero's management, so its hooks
  must not stay behind under the daemon's auto-ensure), then removes the
  host.

## Tests

- `tmux.go`: snapshot parser against fixture output (tabs, missing fields,
  garbage lines); TTL caching with an injected runner; absent tmux binary.
- tick: a pane with foreground "tmux" gains Agent from a fake snapshot
  (injected), prefers the active pane, clears when no pane yields an agent.
- `pane.agentEvent`: attribution table — direct TTY; tmux pane_tty →
  client_tty → pane; `Tmux` session fallback; away; record application
  (working/blocked/done/clear); Agent bootstrap from the event.
- hook `edit()`: multi-event merge — adds missing events beside foreign
  hooks, preserves unknown keys verbatim, replaces outdated kero entries,
  removes cleanly, respects the removal marker; guarded command round-trips
  through `ours()`/`state()`.
- `hookCmd`: per-event dispatch; no `$KERO_PANE` with `$TMUX` set → RPC
  carries Tmux/TTY; any RPC failure → exit 0, empty stdout/stderr (except
  successful SessionStart).
- Gates: `go vet ./...`, `go test ./internal/...`; manual QA on a real
  remote host inside tmux, with screenshots as evidence (repo rule).

## Left to the builder

- Validate the exact tmux format fields (`pane_start_command` vs
  `pane_current_command` for an npm-installed claude) against a real tmux 3.x.
- ctty name resolution helper details (Linux/darwin).
- Entry wiring for the auto-ensure without an import cycle (cli helper vs
  extracting edit() into a shared package).
- Locale strings, should the skills row text change.

## Known caveats

- Claude in a tmux session no kero pane is attached to: hooks register the
  conversation but no pane shows status — today's away behavior.
- Two kero instances (dev + installed) both auto-ensure; their `ours()`
  matcher recognizes each other's entries, so the last daemon to start owns
  the command path — same as `kero skill setup` today.
- Windows hosts: no tmux, and the guard is unix-only — a missing binary can
  still surface a hook error line there.
- tmux's `pane_current_command` shows "node" for npm-run claude;
  identification then leans on `pane_start_command` and, failing that, on
  the first hook event.
