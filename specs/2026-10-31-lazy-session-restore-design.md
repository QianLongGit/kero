# Lazy Session Restore — Re-running What a Pane Ran — Design

Date: 2026-10-31
Status: approved approach (full-lazy restore + tiered re-run), pending implementation

## Intent and why

When kerod restarts — a server update, a Mac reboot, a crash — every pane's
program ends with it: the PTY master closes and the foreground groups get
SIGHUP. Today the next daemon rebuilds each session under its scrollback
(`restore()`, `internal/daemon/store.go:131`), but two things fall short of
what a terminal for coding agents should do:

1. **Nothing runs again.** Each pane comes back as a bare shell. A pane that
   was attached to a tmux session (`tmux attach -t dev`) comes back detached;
   a pane running a dev server or `htop` comes back to a prompt. Only agents
   with a known session resume (`claude --resume <id>` …, store.go:219).
2. **Restore is eager.** `restore()` runs in `Run` before the Accept loop
   (daemon.go:171), so every pane's shell — and every resumed agent — starts
   the moment the daemon boots, watched or not.

The eager part is not just wasted processes. If restore ever re-ran a tmux
client, a headless attach would appear on the tmux session while nobody is at
kero: the user attaching the same session from a phone would see its view and
size disturbed by a client that shouldn't be there. So the restore of things
that touch shared state must wait until a kero client is actually present:

> The remote host's kerod updates and restarts. The user's phone keeps its
> tmux view undisturbed — no ghost attach. The moment the user's kero app
> reconnects, every session comes back: the tmux client re-attaches, Claude
> Code resumes its conversation, the dev server starts again.

A second scenario needs no new machinery and must keep working (it is a QA
item, not a feature): quitting or force-quitting the kero app leaves the
local kerod and every pane's program running (`internal/daemon/daemon.go`
package comment), so reopening the app attaches where it left off. The user
remembers a case where a local Claude Code session did not survive a kero
force-quit; the plan verifies this and files what breaks as a separate bug.

## Goals

- After a daemon restart, sessions restore **lazily**: the daemon registers
  them (layout, scrollback, metadata) at boot but starts no programs until a
  kero client is viewing — an attach Hello or a `subscribe` on a control
  connection — then materializes all dormant panes at once. A command that
  drives one pane (`send`/`key`/`prompt`/`wait`) wakes only the pane it
  addresses.
- On materialize, each pane runs what it last ran, by precedence:
  1. an agent with a known session resumes it (`resume-agents`, unchanged);
  2. a pane whose foreground was a tmux client re-runs that client's argv
     (`restore-multiplexer`, new, default on);
  3. any other recorded foreground command re-runs when `restore-rerun = all`
     (new setting, `off` | `all`, default `off`);
  4. otherwise a bare shell, as today.
- The last foreground command and its directory are recorded per pane at save
  time (argv + cwd from the same /proc reads `tick` already does), so what
  re-runs is what the user last did interactively — `tmux attach -t dev`
  typed into a shell included — not the pane's creation command.
- Settings UI gains the two new rows in the existing Sessions group, with
  en/ja/zh-Hans strings; behavior otherwise unchanged for existing users
  (defaults preserve today's shell-only restore; agents keep resuming).
- A daemon holding only dormant sessions does not idle-exit, and does not run
  a single program before a client views it.

## Non-goals / anti-goals

- No OTTY-style "terminal restore protocol" (programs declaring their own
  restart via escape sequences). The precedence table above covers kero's
  model; the protocol can be added later if programs adopt it.
- No whitelist tier for re-running (`off`/`all` only). Three tiers is
  configurability without a user yet; a whitelist key can be added without a
  migration if it is ever wanted.
- No persistence of the pane's creation `Env` (already transient today), no
  GUI layout persistence (the daemon owns layout; the app already restores
  selection via `prefs.Selected`).
- No change to `proto.Version`, no new RPC, no new proto.Pane field. Dormant
  panes are panes with `Restored: true` and no process; materialize is
  synchronous inside the connection path, so clients never observe a
  half-state.
- Never re-run anything on a daemon boot with no client connected — a
  one-shot CLI (`kero ls`, `kero events`) must not wake panes, for the same
  ghost-attach reason.
- No tmux management beyond re-running the user's own client argv: kero never
  invents attach commands, never kills or creates tmux sessions.

## Constraints (verified in code)

- Restore today: `Daemon.restore` (store.go:131) → `restorePane`
  (store.go:181): `newPane` + feed `sessions/<id>.vt` + the
  `── restored · session ended <when> ──` divider + `p.start(resumeCommand(sp.Agent), nil)`
  (store.go:208). `savedPane` (store.go:39) already carries
  `Command []string` (the pane's creation argv) but restore ignores it
  deliberately ("what ran in it may not want to run again unasked",
  store.go:206).
- `pane.start(command []string, env []string)` (session.go:140) runs a
  command inside an interactive login shell (`shellArgs`, shellcmd.go:29):
  `cmd; printf OSC-133;D; exec shell -l` — when the command ends the session
  **falls back to the shell**, so a re-run `tmux attach` behaves exactly like
  one the user typed: detach lands on a prompt. Windows shells and PowerShell
  get the same contract (`-NoExit` / `/K`).
- Foreground tracking exists: `pane.tick` (session.go:310) reads
  `s.proc.Foreground()` into `s.fgPGID`, with `procName`/`procArgs`/`procCwd`
  per platform (proc_linux.go, proc_darwin.go `kern.procargs2`,
  proc_windows.go) and the tmux predicate `tmuxProc` (tmux.go:113, matching
  comm `tmux: client`). `tick` already no-ops on `s.proc == nil`
  (session.go:313) — a dormant pane is safe in the tick loop as-is.
- `AgentSession` is live-tracked and dropped when its process ends
  (session.go:340-350), so `state.json` holds the session id of whatever
  agent was running at the last save; `resumeCommand` (store.go:219) covers
  claude/codex/pi/opencode.
- Client arrival: every connection lands in `serveHello` (daemon.go:659);
  attach Hellos dispatch to `serveAttach` (daemon.go:729), control
  connections call RPCs, and the GUI's state stream is `subscribe`/`watch`
  (rpc.go:37-50). `Run` restores before the Accept loop starts (daemon.go:171),
  so registration always completes before any client can connect — the
  materialize hook never races boot.
- Idle exit: `tickLoop` (daemon.go:322-354) exits after 30s only when there
  are no sessions, no connections, and no listener. Dormant sessions live in
  `d.sessions`, so a daemon holding them stays up; a bridge-spawned kerod
  with dormant sessions will not vanish before the GUI returns.
- Persistence is already crash-friendly: `saveState` debounced 300ms on
  change, histories every 10s, both forced on stop (store.go:61-79,
  daemon.go:177-184); `writeAtomic` everywhere. `savedState.Version` is 2;
  unknown JSON fields are ignored by older readers, so additive fields need
  no version bump (same rule as proto).
- Settings plumbing: `Settings` field + `Default()` (settings.go:30,150) →
  config key (`boolKey`, config.go:117) → `settingRow` in the Sessions group
  of the general page (settings_view.go:489-517) → locales en/ja/zh-Hans →
  `settings_search.go:31`. Existing keys `restore-sessions` and
  `resume-agents` are the naming family; saves are immediate, no button.
- Server identity/lifecycle: update replaces the install dir and restarts;
  systemd (`Restart=always`) or the successor takeover relaunch within
  seconds (listen.go:467-478, daemon.go:224-234). The GUI reconnects on its
  own (300ms after a control drop, hosts.go:454-466) and treats a new
  PID+Started as the same host — no GUI change is needed for any of this.

## Approaches considered

**A. Full-lazy restore with tiered re-run (chosen).** Boot registers
sessions and feeds history but starts nothing; the first viewing client
(attach Hello or subscribe/watch) materializes everything. One rule, one
code path, local and remote identical, zero programs before a viewer, and
the tmux problem disappears by construction. Cost: a `dormant` split of
`restorePane` and a trigger hook.

**B. Per-pane laziness on first attach.** Materialize a pane only when it is
attached/viewed. Cheapest in processes, but the GUI attaches only visible
panes (LRU of 12, app.go:808), so tmux sessions in unopened sidebar rows
would stay detached — the opposite of "reconnect and everything is back".
Rejected.

**C. Lazy only for tmux-class panes; everything else eager as today.**
Smallest diff, but two behaviors to explain, agents still run unwatched, and
the re-run policy would interact with eagerness in confusing ways (a rerun
command that must not start unwatched is not only tmux). Rejected.

Re-run policy tiers: OTTY ships off/whitelist/all; we ship **off/all**
(KISS — the whitelist has no user yet and needs config + UI surface), and
the **default is off**: re-running arbitrary commands unasked can repeat
one-shot work, so it stays opt-in per user. `restore-multiplexer` defaults
**on** (an attach is idempotent from the user's point of view — it is what
the pane was doing — and it is the core ask).

## Design

### 1. Dormant registration at boot

`restore()` keeps its current shape (projects, sessions, layout, `sameLeaves`
fallback) but `restorePane` splits in two:

- **`restorePane` (boot)**: `newPane` with the same metadata as today, feed
  the `.vt` history and the restored divider, set `p.dirty = true`, and leave
  the pane **dormant**: `proc == nil`, `Status: proto.Idle`, `Restored:
  true`. Browser and file panes are inert already — restored as today, no
  dormancy. The session goes into `d.sessions` as today (this is what keeps
  the daemon from idle-exiting and the State truthful).
- **`pane.materialize(set settings.Settings)`**: decides the start command
  by the precedence table and calls the existing `p.start(cmd, nil)`, in
  the recorded cwd when `RestoreDir` is set. It is idempotent per pane:
  `pane.start` does not check for a running process (`restart` does,
  session.go:731), so materialize claims the pane under `pane.mu` first —
  a pane already started by another path (a `kero restart` racing the wake)
  is skipped. A start error drops the pane through `d.removePane` (it is
  registered, unlike a boot-time restorePane failure) and the session
  shrinks or vanishes exactly as today's restore does. After the wake
  drains, one `d.changed()` broadcasts the live state.

The decision is a pure function over the saved pane and settings —
`materializeCommand(set, sp) []string` — so the whole policy is unit-testable
without a PTY:

```
browser/file pane      → (nothing to start)
Agent != nil && resume-agents  → resumeCommand(Agent)        // unchanged
Tmux && restore-multiplexer    → RestoreCmd                  // the client's own argv
RestoreCmd != nil && restore-rerun == all → RestoreCmd
otherwise                      → nil                          // bare shell
```

`restore-sessions = false` keeps today's meaning: no sessions registered at
all, dormant or otherwise.

### 2. What is recorded, and when

`savedPane` gains two additive fields (no version bump; older daemons ignore
them, older state reads as empty → today's behavior):

```go
RestoreCmd  []string `json:"restoreCmd,omitempty"`  // argv of the last foreground program
RestoreDir  string   `json:"restoreDir,omitempty"`  // its cwd, when it differs from Dir
Tmux        bool     `json:"tmux,omitempty"`        // the foreground was a tmux client
```

`saveState` fills them per pane: when `fgPGID != 0 && fgPGID != shellPID`,
`procArgs(fgPGID)` and `procCwd(fgPGID)` — the same reads `tick` performs,
at most a tick stale, under `pane.mu` as the rest of the snapshot. `Tmux` is
`tmuxProc(procName(fgPGID))`. A pane whose foreground is the shell records
nothing: it restores as a shell in its (already tracked) `Dir`. On macOS
argv[0] is the full executable path (`kern.procargs2` opens with it); it is
kept as recorded — a binary that has moved degrades to the shell, never to
a wrong command.

A dormant pane records what it was restored with: `restore` keeps the saved
values on the pane (unexported fields), and `saveState` writes them back for
any pane without a foreground process. Without that write-back the next
save — the 10s ticker or any change — would blank the fields, and a daemon
restarting twice in a row (two updates back to back) would lose the re-run
information for good.

Re-running argv is faithful because it is the user's own command line —
`tmux attach -t dev` re-runs as `tmux attach -t dev`. (Argv carries no
pipes or redirections — those live in the shell: a pane running
`cat a | grep b` records the pipeline's first stage, and a re-run without a
redirection writes to the terminal rather than the file, which is the safe
direction.) Agents are the
exception by precedence: an agent with a known session resumes rather than
starting a new conversation, and an agent without one (detection gap)
degrades to its raw argv under `restore-rerun = all`, which still lands the
user back in the tool.

### 3. The materialize trigger

One hook, in the connection path, gated by a `d.dormant []*session` list
(sessions still holding unstarted panes):

- **attach Hello** (`serveAttach` entry, daemon.go:707) and
- **`subscribe` RPC** (rpc.go:37) — the GUI's state stream; what `kero
  attach`'s control connection uses before attaching.

Both mean *a client is viewing panes now*. `watch` is deliberately **not** a
trigger: its only caller is `kero events`, a passive monitor that "is no
app… nor do they count as an app being open" (daemon.go:52-55), so a
scripted `kero events --once` must not wake panes — same ghost-attach reason
as every other one-shot RPC (`state`, `create`, `conversations`,
`host.info`). Commands that **drive a pane** — `send`, `key`, `prompt`,
`wait` — wake *only the pane they address*: today those commands always
reach a shell, and scoping the wake keeps that contract without letting a
scripted poke wake the world (a `send` to a tmux pane re-attaches that tmux,
which is what the sender asked for). The hook
is synchronous (spawning a PTY is a fork; dozens of panes stay in the
milliseconds) and idempotent: it drains `d.dormant` under `d.mu`, so the
second client through finds it empty. The subscribe handler itself answers
under `d.mu` (rpc.go:41-49); the wake runs *before* that critical section,
so the first state a subscriber receives already shows live panes — and so
the wake never takes `d.mu` while the caller holds it. A failed pane start drops that pane
and the session shrinks or vanishes exactly as today's restore does.

Sequence walkthrough, the user's own scenario:

1. Server kerod updates → old daemon saves state and dies → panes' programs
   end (phone's tmux client on its own connection: untouched).
2. New kerod boots: registers sessions, feeds history, starts **nothing**;
   the phone's tmux view and size are unaffected.
3. kero app reconnects (300ms–1s), subscribes → materialize: the tmux pane
   re-runs `tmux attach -t dev` inside its shell, Claude Code panes resume
   their conversations, everything else per policy.
4. If kero never returns, nothing runs; the daemon sits with dormant
   sessions, state.json fresh.

### 4. Settings

| key | type | default | meaning |
|---|---|---|---|
| `restore-multiplexer` | bool | `true` | re-run a pane's tmux client on restore (its own argv) |
| `restore-rerun` | enum `off`\|`all` | `off` | re-run any recorded foreground command on restore |

Both render in the general page's Sessions group beside `restore-sessions`
and `resume-agents` (settings_view.go:489-517), follow the existing
field→key→row→locales→search chain, and are read by the daemon at
materialize time via `settings.Load()`, which re-reads the file when its
mtime moves (settings.go:236-252) — so a flipped toggle affects the *next*
restore, the natural reading of the setting.

### 5. What deliberately does not change

- GUI code: reconnection, state rendering, attach, the restored marker — all
  existing. `prefs.Selected` already survives; IDs are preserved by restore.
- proto: nothing. Dormant panes look exactly like restored panes that have
  not been viewed yet (which today is a transient nobody renders
  differently).
- Idle exit, successor takeover, systemd unit, update flow, bridge spawn.
- The local force-quit path: kerod survives app death; panes never die; no
  restore involved. (QA item below.)

## Testing

Internal (CI, `go test ./internal/...`, KERO_HOME in t.TempDir()):

- `materializeCommand` matrix: agent+resume on/off, tmux on/off, rerun
  off/all, empty RestoreCmd, browser pane — the whole precedence table as a
  table test.
- Dormant boot: save a state with panes → new Daemon → panes present,
  `proc == nil`, history fed, daemon does not idle-exit while dormant
  (tickLoop condition with dormant sessions).
- Recording: a pane with a foreground command (spawn a short-lived program
  in the test PTY) → `saveState` → state.json carries `restoreCmd`/`tmux`.
- Retention: a dormant daemon saves again (the 10s ticker) and a second
  restart still carries `restoreCmd`/`tmux` — the double-update case.
- Materialize: attach Hello and a subscribe RPC each start everything
  exactly once (two clients → single spawn set); `state`-only and `watch`
  clients leave panes dormant; a `send` to one pane wakes that pane alone;
  started panes run the expected argv (readable from the
  pane's shell after start, or via a stub shell that writes its argv out).
- Settings: defaults, parsing, invalid enum value falls back to `off`.

Root package (local, `go test .`): settings rows render and toggle; search
keywords match; per the mygo skill, `ui.NewTester` drives the rows.

QA on a real machine (the user's two memories, made precise):

- Remote: pane in `tmux attach`, phone attached to the same tmux session;
  update the server's kerod → phone undisturbed → kero reconnects → pane
  re-attached, Claude Code pane resumed, size negotiation owned by the real
  clients again.
- Local: Claude Code running, force-quit the app, reopen → the same Claude
  Code session, untouched (no restore should occur). If it does not hold,
  that is a separate bug with its own investigation.

## What is left to the builder

- The enum setting's exact widget (segmented row vs. two radio rows) —
  follow whichever multi-value setting already in settings_view.go renders
  simplest; strings in all three locales.
- Whether the materialize hook lives as a small method on Daemon called from
  the two sites, or the sites call into a shared `wakeDormant()` — keep it
  one function either way.
- Test scaffolding reuse: the existing session/store tests' helper for a
  short-lived foreground program; extend rather than rebuild.
- CHANGELOG entry at release time; no RELEASING.md change (no proto bump).
