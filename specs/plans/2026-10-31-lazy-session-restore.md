# Lazy Session Restore Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** After a kerod restart, sessions register dormant and start nothing; the first viewing client (attach Hello or subscribe/watch RPC) materializes every pane, re-running what it last ran (agent resume > tmux client > `restore-rerun` policy > bare shell).

**Architecture:** Split `restorePane` (store.go) into boot-time registration (metadata + `.vt` history, no process) and `pane.materialize` (spawn, precedence table); record each pane's last foreground argv/cwd/tmux-ness into additive `savedPane` fields with a retention write-back; hook `Daemon.wakeDormant` into the attach and subscribe paths only, with a pane-scoped wake for the drive RPCs. Two new settings (`restore-multiplexer`, `restore-rerun`) ride the existing settings chain.

**Tech Stack:** Go; internal/daemon (PTY + terminal emulator), internal/settings (Ghostty-style config), root package mygo UI; locales via `go tool mygo generate`.

**Spec:** `specs/2026-10-31-lazy-session-restore-design.md` (read it first — the plan argues from it)

## Global Constraints

- Comments/doc comments in English prose explaining why, matching neighboring code (project CLAUDE.md).
- No `proto.Version` bump, no new RPC, no new `proto.Pane` field (spec).
- `locales/catalog_gen.go` is generated — never hand-edit; change `locales/{en,ja,zh-Hans}.json` then `go tool mygo generate` (project CLAUDE.md).
- Daemon tests isolate `KERO_CONFIG` into `t.TempDir()` (`testDaemon`, internal/daemon/browser_test.go:16); root tests isolate `KERO_HOME` + `KERO_CONFIG` (`testApp`, view_test.go:31).
- Root-package tests are NOT in CI: run locally with `go test .` (first run downloads libghostty-vt; `-short` skips those); CI set is `go vet ./...` + `go test ./internal/...`.
- Minimal diff: every changed line traces to the spec; no drive-by refactors (project CLAUDE.md working rules).
- No git commits/branches in task steps (session rule: user commits); a task ends at its green verification.

## Review Focus

1. **Double restart while dormant** (two server updates back to back) must not lose `restoreCmd`/`tmux` — pinned by Task 2 Step 1 (retention test).
2. **One-shot RPCs must not wake panes** (`kero ls` → ghost tmux attach) — pinned by Task 4 Step 1 (`state` leaves panes dormant).
3. **Second viewing client must not double-start** — pinned by Task 3 Step 1 (materialize twice, same PID) and Task 4 Step 1 (two subscribes).
4. **`resume-agents` flipped between boot and wake** — materialize re-reads settings; pinned by Task 3 Step 1 (matrix: agent set, ResumeAgents=false → shell).
5. **`RestoreCmd` whose binary is gone / command fails** — the interactive shell degrades to a prompt, pane stays alive; pinned by Task 3 Step 1 (bogus argv → proc non-nil, pane not dropped).

---

### Task 1: Settings fields and config keys

**Files:**
- Modify: `internal/settings/settings.go` (Settings struct + `Default()`)
- Modify: `internal/settings/config.go` (configKeys table)
- Test: `internal/settings/config_test.go` (add cases)

**Interfaces:**
- Produces: `Settings.RestoreMultiplexer bool` (config `restore-multiplexer`, default `true`), `Settings.RestoreRerun string` (config `restore-rerun`, values `off`|`all`, default `off`) — later tasks read these names.

- [ ] **Step 1: Write the failing tests**

Add to `internal/settings/config_test.go`, following the existing parse/format/round-trip cases (config_test.go:13-53):

- parse: `restore-multiplexer = false` → `RestoreMultiplexer == false`; `restore-rerun = all` → `RestoreRerun == "all"`.
- default: empty file → `RestoreMultiplexer == true`, `RestoreRerun == "off"`.
- invalid: `restore-rerun = sometimes` → parse reports a problem and the field stays `"off"` (choiceKey semantics, config.go:60-71).
- format round-trip: set `RestoreRerun = "all"`, `RestoreMultiplexer = false` → formatted file contains exactly those two lines among the changes; re-parse returns them.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/settings/ -run TestParse -v` (plus the format test name chosen)
Expected: FAIL — unknown fields/keys.

- [ ] **Step 3: Implement**

`settings.go`: add the two fields to `Settings` beside `RestoreSessions`/`ResumeAgents` (settings.go:30-31); set defaults in `Default()` (settings.go:150-151). `config.go`: `boolKey("restore-multiplexer", ...)` modeled on the `restore-sessions` entry (config.go:117); `choiceKey("restore-rerun", ...)` modeled on `diff-layout` (config.go:305) with choices `off`,`all`. Copy the comment voice of the neighbors.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/settings/`
Expected: PASS (all package tests, not only the new ones).

---

### Task 2: Dormant registration + schema fields + retention

**Files:**
- Modify: `internal/daemon/store.go` (`savedPane`, `restore`, `restorePane`, `saveState`)
- Modify: `internal/daemon/session.go` (pane retention fields)
- Test: `internal/daemon/dormant_test.go` (new)

**Interfaces:**
- Consumes: nothing new.
- Produces: `savedPane` fields `RestoreCmd []string` (`json:"restoreCmd,omitempty"`), `RestoreDir string` (`json:"restoreDir,omitempty"`), `Tmux bool` (`json:"tmux,omitempty"`); pane unexported fields `restoreCmd []string`, `restoreDir string`, `restoreTmux bool` (Task 3/5 read them); panes restored with `proc == nil` (dormant); `Daemon.dormant []*session` (Task 4 drains it).

- [ ] **Step 1: Write the failing tests**

New `internal/daemon/dormant_test.go`, helpers per `testDaemon` (browser_test.go:16) and the hand-written-state pattern (rpc_test.go:410-425). `savedState.Version` stays 2.

- `TestRestoreRegistersDormant`: write a state.json with one session, one terminal pane (`Dir`, `Cols`/`Rows`, `restoreCmd: ["tmux","attach","-t","dev"]`, `tmux: true`) and a matching `sessions/<id>.vt` file; `d.restore()`; assert: pane exists in `d.panes`, `p.proc == nil`, `p.info.Restored`, history divider present (`p.term` screen contains `restored · session ended`), session is in `d.dormant`, and `len(d.sessions) > 0` (idle-exit guard is the session count, daemon.go:332).
- `TestDormantRetentionAcrossRestart`: after the restore above, call `d.saveState()` (what the 10s ticker does); start a second `testDaemon` on the same `dir` and `restore()`; assert the pane's retained `restoreCmd`/`restoreTmux` survive the round-trip (read state.json or the new pane's fields).
- `TestRestoreBrowserPaneIsNotDormant`: URL pane from state.json → restored fully at boot (no dormancy), matching today's behavior (store.go:183-189).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/daemon/ -run 'TestRestoreRegistersDormant|TestDormantRetention|TestRestoreBrowserPane' -v`
Expected: FAIL (restore starts panes eagerly today; fields don't exist).

- [ ] **Step 3: Implement**

store.go:
- Extend `savedPane` with the three fields (comment: what a materialize re-runs, and that macOS argv[0] is a full path, kept as recorded).
- `restorePane` (store.go:181): keep metadata + `.vt` feed + divider + `p.dirty = true`; **stop calling `p.start`**; carry `sp.RestoreCmd/RestoreDir/Tmux` onto the pane's retention fields; the function no longer fails on start (drop that path; `newPane` error still drops).
- `restore` (store.go:131): after building each session, append it to `d.dormant` when it holds ≥1 terminal pane (browser/file-only sessions are not dormant). Keep the `!set.ResumeAgents` Agent-nil-ing at load (store.go:150-153) for pane-info fidelity.
- `saveState` (store.go:81): per pane, when the pane has a live foreground (`fgPGID != 0 && fgPGID != shellPID` — Task 5 fills from /proc; for now fall through to retention), else write the retained `restoreCmd/restoreDir/restoreTmux` back — the dormant pane must not blank its own re-run record on the next save.

session.go: add the three unexported retention fields to `pane` (session.go:31-78) with a comment pointing at store.go.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run 'TestRestore|TestDormant|TestSessionMove|TestProject|TestBrowser' -v && go test ./internal/daemon/`
Expected: PASS — new tests and every existing restore-dependent test (rpc_test.go:135, project_test.go:281, browser_test.go:66) still green.

---

### Task 3: `materializeCommand` + `pane.materialize`

**Files:**
- Modify: `internal/daemon/store.go` (pure function)
- Modify: `internal/daemon/session.go` (method), `internal/daemon/sessions.go` (only if the drop path needs it)
- Test: `internal/daemon/dormant_test.go` (extend)

**Interfaces:**
- Consumes: Task 1 `settings.Settings` fields; Task 2 retention fields; existing `resumeCommand` (store.go:219), `pane.start(command, env []string)` (session.go:140).
- Produces: `func materializeCommand(set settings.Settings, sp savedPane) []string` — the precedence table; `func (s *pane) materialize() bool` — true when the pane is running or got started, false when it must be dropped via `d.removePane`.

- [ ] **Step 1: Write the failing tests**

- `TestMaterializeCommand` (table-driven, modeled on `TestResumeCommand`, session_test.go:197). Rows, with `sp.RestoreCmd` = last fg argv:
  - agent `claude` + `ResumeAgents: true` → `["claude","--resume",id]` (id pattern-valid; also keep the injection guard: an id like `x; rm -rf ~` falls through to the next rule).
  - same + `ResumeAgents: false` → falls to policy: `RestoreRerun: "all"` → `sp.RestoreCmd`; `"off"` → nil.
  - `Tmux: true` + `RestoreMultiplexer: true` → `sp.RestoreCmd` (even with `RestoreRerun: "off"`).
  - `Tmux: true` + `RestoreMultiplexer: false` + `RestoreRerun: "all"` → `sp.RestoreCmd`.
  - no RestoreCmd, no agent → nil regardless of policy.
  - browser pane (`sp.URL != ""`) → nil (nothing to start).
- `TestMaterializeStartsOnce`: dormant pane (Task 2 helper), `p.materialize()` → `p.proc != nil`; second `p.materialize()` → same `p.shellPID`, still one process (idempotent claim under `pane.mu`).
- `TestMaterializeBogusCommandDegradesToShell`: dormant pane with `restoreCmd: ["/no/such/binary-ever"]`, `RestoreRerun: "all"` → `materialize()` true, `p.proc != nil`, pane alive (the interactive shell ran the failing command and fell back — shellArgs contract, shellcmd.go:52-81); do not assert the command's output, only that the pane did not drop.
- `TestMaterializeUsesRestoreDir`: dormant pane with `restoreDir` set to a `t.TempDir()` → after materialize, `p.info.Dir` equals it (start cwd, session.go:148).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/daemon/ -run 'TestMaterialize' -v`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Implement**

`materializeCommand` in store.go beside `resumeCommand`, exactly the spec's table (it re-checks `set.ResumeAgents` so a flip between boot and wake holds); nil Agent and empty RestoreCmd mean shell. `pane.materialize` in session.go: under `s.mu`, if `s.proc != nil || s.closed` → return true; set `s.info.Dir` from `restoreDir` when non-empty; release; `p.start(materializeCommand(...), nil)`; on start error return false (caller drops via `d.removePane` — the pane is registered, unlike a boot failure). Note in the method comment: `pane.start` itself doesn't guard double-starts; the claim above is the guard (restart RPC shares the check, session.go:731).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run TestMaterialize -v && go test ./internal/daemon/`
Expected: PASS.

---

### Task 4: `wakeDormant` + viewing-client triggers

**Files:**
- Modify: `internal/daemon/daemon.go` (`wakeDormant`, attach hook in `serveHello`)
- Modify: `internal/daemon/rpc.go` (subscribe/watch hook)
- Test: `internal/daemon/dormant_test.go` (extend)

**Interfaces:**
- Consumes: Task 2 `d.dormant`, Task 3 `pane.materialize` + drop path.
- Produces: `func (d *Daemon) wakeDormant()` — idempotent, synchronous drain; called from the attach Hello path and the `subscribe` RPC case only (`watch` is NOT a trigger: its only caller is the passive `kero events` monitor, daemon.go:52-55). Plus a pane-scoped wake wired into the drive RPCs (`send`, `key`, `prompt`, `wait`) — those commands still reach a shell, waking only the pane they address.

- [ ] **Step 1: Write the failing tests**

Client plumbing per `transferConn` (transfer_test.go:32-45: `net.Pipe` + `go d.serve` + `proto.Hello{Version: proto.Version}` + read `proto.Welcome`); for a control conn send a `proto.Request` and read the reply, per rpc_test's helpers.

- `TestStateRPCDoesNotWake`: control conn, RPC `state` → reply parsed; assert every restored pane still `proc == nil` (`kero ls` must not ghost-attach tmux).
- `TestWatchDoesNotWake`: control conn, RPC `watch` → panes still `proc == nil` (`kero events` is a passive monitor; same reason as `state`).
- `TestSubscribeWakes`: control conn, RPC `subscribe` → first state reply arrives with the pane's `PID != 0` (wake runs before the state is built) and pane `proc != nil`.
- `TestAttachHelloWakes`: `proto.Hello{Kind: "attach", Session: <pane id>, Cols: 80, Rows: 24}` → after `Welcome`, pane `proc != nil` (discard the FrameOutput that follows).
- `TestSecondClientNoDoubleStart`: after `TestSubscribeWakes`'s flow, open a second control conn + `subscribe` → same `shellPID` as before.
- `TestSendWakesOnlyAddressedPane`: two dormant panes; control conn, RPC `send` targeting pane A (params per rpc.go's `send` case) → pane A `proc != nil`, pane B still `proc == nil`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/daemon/ -run 'TestStateRPCDoesNotWake|TestWatchDoesNotWake|TestSubscribeWakes|TestAttachHelloWakes|TestSecondClient|TestSendWakes' -v`
Expected: FAIL — no wake path exists; restored panes never start.

- [ ] **Step 3: Implement**

`wakeDormant` in daemon.go: swap `d.dormant` to nil under `d.mu`; if empty return; `settings.Load()`; for each session, materialize each pane, drop the false ones via `d.removePane`; end with one `d.changed()`. Hook sites: in `serveHello`'s `case "attach"` before `d.serveAttach` (daemon.go:707); in rpc.go's `subscribe` case **before** its `d.mu.Lock` (rpc.go:41-49 — calling it under the lock deadlocks and subscribers must see post-wake state; `watch` gets NO hook). In the `send`/`key`/`prompt`/`wait` cases, wake the addressed pane only (materialize it, drop via `d.removePane` on false) before the input is delivered — the command must reach a shell as it always did. `d.dormant` itself stays session-granular and is drained only by `wakeDormant`: materialize's idempotent claim (Task 3) makes pane-level accounting unnecessary — a pane already woken by a drive RPC is skipped by the later wholesale drain. Keep the function and its call sites tiny; a comment at each site says why only viewing connections wake panes and why drive RPCs wake just their pane.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/`
Expected: PASS (whole package — the hooks sit on hot paths; watch for `TestWatch`/`TestWatcherBehind` regressions, browser_test.go:209/272).

---

### Task 5: Record the live foreground at save time

**Files:**
- Modify: `internal/daemon/store.go` (`saveState`)
- Test: `internal/daemon/dormant_test.go` (extend)

**Interfaces:**
- Consumes: `procArgs`/`procCwd`/`procName` (proc_*.go), `tmuxProc` (tmux.go:113), `pane.fgPGID`/`shellPID`.
- Produces: `saveState` filling `RestoreCmd`/`RestoreDir`/`Tmux` from the live foreground when there is one (Task 2's retention path covers the rest).

- [ ] **Step 1: Write the failing test**

`TestSaveRecordsForeground`: real PTY per `queries_unix_test.go`/`shell_test.go` scaffolding (`testEnv` HOME/TERM, shell_test.go:106): `pane.start([]string{"sleep","30"}, nil)` (shellArgs runs it interactively), call `s.tick(time.Now())` once so `fgPGID` settles (session.go:310), `d.saveState()`, read state.json → pane's `restoreCmd` == `["sleep","30"]`, `restoreDir` empty or the shell's dir, `tmux` false. Then a plain-shell pane (started with nil command, ticked) → `restoreCmd` empty (foreground is the shell: records nothing).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestSaveRecordsForeground -v`
Expected: FAIL — saveState never writes restoreCmd for a live pane.

- [ ] **Step 3: Implement**

In `saveState`'s per-pane snapshot (store.go:86-95), under the existing `p.mu.Lock`: when `p.fgPGID != 0 && p.fgPGID != p.shellPID`, `RestoreCmd = procArgs(p.fgPGID)`, `Tmux = tmuxProc(procName(p.fgPGID))`, `RestoreDir = procCwd(p.fgPGID)` when it differs from `p.info.Dir`; otherwise the Task 2 retention values. Guard `//go:build unix` is unnecessary — proc_* exists on all platforms; the tmux predicate is a string test.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/`
Expected: PASS.

---

### Task 6: Settings UI rows, locales, search

**Files:**
- Modify: `locales/en.json`, `locales/ja.json`, `locales/zh-Hans.json` (+ regenerate `locales/catalog_gen.go`)
- Modify: `settings_view.go` (Sessions group rows), `settings_search.go`
- Test: `settings_test.go` (extend)

**Interfaces:**
- Consumes: Task 1 `Settings.RestoreMultiplexer`/`RestoreRerun`.
- Produces: two rows in the general page's Sessions group (settings_view.go:489-517).

Before touching root UI code, read `.agents/skills/mygo-maintenance/SKILL.md` (project CLAUDE.md rule), then `go tool mygo vet .` after changes.

- [ ] **Step 1: Write the failing tests**

Extend `settings_test.go` following `TestSettingsPages` (settings_test.go:20) and `TestSegmentedChoiceSticks` (settings_test.go:81):

- `TestSettingsPages` want map: general page shows the two new titles (assert exact en strings below).
- New `TestRestoreRerunChoiceSticks`: `ui.NewTester`, click the `All commands` segment (3× `tt.Frame()`), assert `a.set.RestoreRerun == "all"` and `settings.Load().RestoreRerun == "all"`; click the multiplexer toggle → `settings.Load().RestoreMultiplexer` flips. Raise the tester window height to 1300 like `TestFileOpenSetting` (settings_test.go:111) so the rows are hittable.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run 'TestSettingsPages|TestRestoreRerunChoiceSticks'`
Expected: FAIL — strings absent.

- [ ] **Step 3: Implement**

Locales (key names follow whatever the existing restore-sessions row uses — grep its keys in locales/en.json and match the convention):

- en: `Re-attach tmux sessions after a restart` / detail `A restored pane runs its tmux client again, attaching the session it had` ; `Re-run commands after a restart` / detail `A restored pane runs the command it ended with; an agent resumes its conversation instead` ; segment options `Off` and `All commands`.
- zh-Hans: `重启后重新连接 tmux 会话` / `恢复的窗格会重新运行它的 tmux 客户端，重新连上原来的会话` ; `重启后重新运行命令` / `恢复的窗格会重新运行它最后执行的命令；agent 会话则改为续接` ; `关闭` / `所有命令`。
- ja: `再起動後に tmux セッションへ再接続` / `復元したペインは tmux クライアントを再実行し、元のセッションへ接続し直します` ; `再起動後にコマンドを再実行` / `復元したペインは最後のコマンドを再実行します。エージェントは代わりに会話を再開します` ; `オフ` / `すべてのコマンド`。

Then `go tool mygo generate`. `settings_view.go`: toggle row for multiplexer and a segmented row for rerun (segmented precedent settings_view.go:543-546), both inside the `if s.RestoreSessions {` block beside `resume-agents` (settings_view.go:510-514 — the same nesting the resume row uses); `settings_search.go:31` keywords for both (en + zh terms, matching neighbors' style).

- [ ] **Step 4: Run tests and gates to verify they pass**

Run: `go test . && go tool mygo vet . && go vet ./... && go test ./internal/...`
Expected: all PASS.

---

### Task 7: Whole-branch verification

**Files:** none (verification only)

- [ ] **Step 1: Full gate**

Run: `go vet ./... && go test ./internal/... && go test .`
Expected: all PASS, zero FAIL.

- [ ] **Step 2: Spec walkthrough against the diff**

Re-read `specs/2026-10-31-lazy-session-restore-design.md` §Goals line by line against `git diff`; every goal sentence maps to a change or an existing mechanism named in the spec. Discrepancies → fix or record in the delivery report.

- [ ] **Step 3: QA notes for the human**

The two on-machine QA items the spec pins (remote tmux + phone undisturbed; local force-quit with Claude Code) cannot run in CI — list them in the delivery report as manual steps.
