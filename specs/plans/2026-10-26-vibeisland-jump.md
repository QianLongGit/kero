# Vibe Island Jump Rules Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Clicking an agent session in Vibe Island's notch panel jumps Kero frontmost to the exact session and pane, via a `kero://focus` URL scheme; the rule installs from Kero's settings; delivered as a signed DMG.

**Architecture:** Vibe Island opens `kero://focus?session=…&tty=…&cwd=…`; mygo delivers it to `OnOpenURL`. A pure resolver matches the pane in the merged state (session id → tty → cwd), then the app selects the session, focuses the pane, shows the window. The pane's pty device name rides the pane state; a settings row writes the rule JSON under `~/.vibe-island/integrations/`.

**Tech Stack:** Go 1.27, mygo framework (urlSchemes / OnOpenURL / IsDev), kero's proto/daemon/root packages, mygo build (signed + notarized DMG).

**Spec:** `specs/2026-10-26-vibeisland-jump-design.md` — the plan argues from it; executors read both.

## Global Constraints

- Comments/doc comments: English prose explaining why, matching existing files.
- One feature per file in the root package: `xxx.go` + `xxx_test.go`.
- Tests isolate the user environment: `t.Setenv("KERO_HOME", t.TempDir())` + temp `KERO_CONFIG`.
- `proto.Version` does NOT bump: `Pane.TTY` is an additive optional field.
- Never open a new pane or guess on no-match: fall back to `showWindow()` only.
- An empty URL parameter counts as absent: skip that matching level.
- Gates for every task: `go vet ./...`, `go test ./internal/...`; root package additionally `go test .` and `go tool mygo vet .`.
- Branch: `claude/vibeisland-jump`. Commits: English imperative, capitalized, no conventional-commit prefixes, no emoji.
- Go toolchain: `env -u GOROOT -u GOPATH GOPROXY=https://goproxy.cn,direct ~/sdk/go1.27.3/bin/go …` (a stale `GOROOT` env and the default proxy both fail on this machine).
- Template locked by the spec: `kero://focus?session={session_id}&tty={tty}&cwd={cwd}`.

## Review Focus

1. Empty `cwd=` (Vibe Island resolves empty variables to empty strings) must not match dir-less panes — skip the level. Test: Task 2, `TestResolveJump` case "empty params skip their level".
2. A remote host's `/dev/ttys003` is a different device than the local one — tty matches local panes only. Test: Task 2, case "tty ignores remote panes".
3. URL before daemon state (cold start) must wait, and be dropped only once the local host's state arrived without a hit. Test: Task 3, `TestTakePendingJump`.
4. `cwd` with spaces arrives percent-encoded (`/Users/foo/my project`). Test: Task 2, `TestParseJumpURL` case `%20`.
5. Scheme/host arrive in any case (`kero://FOCUS`); wrong scheme or host is ignored. Test: Task 2, `TestParseJumpURL` cases "uppercase host", "wrong scheme", "wrong host".

---

### Task 1: Pane carries its terminal device

**Files:**
- Modify: `internal/proto/proto.go:211` (Pane struct, after `PID`)
- Modify: `internal/daemon/pty_unix.go:16-23` (ptyProc struct), `:34-60` (startPTY)
- Modify: `internal/daemon/pty_windows.go:22` (ptyProc struct — field only, never set)
- Modify: `internal/daemon/session.go:169` (start())
- Test: `internal/daemon/session_test.go` (new test at the end)

**Interfaces:**
- Consumes: `startPTY` already returns the slave name from `openMaster()` (pty_unix.go:35).
- Produces: `proto.Pane.TTY string` (`json:"tty,omitempty"`) — `/dev/ttys012`, set at every (re)start of the pane's pty (a restored pane gets the new device's name; the assignment lives in `start()`, which runs again on restore).

- [ ] **Step 1: Write the failing test**

Append to `internal/daemon/session_test.go`, following `TestAgentPane`'s `newPane` scaffolding (session_test.go:222-240):

```go
// A pane tells the terminal device its agent runs on, so an outside
// jump can name it: set again when a restored pane's pty is a new one.
func TestPaneTTY(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /dev/ttys on windows")
	}
	t.Setenv("KERO_CONFIG", filepath.Join(t.TempDir(), "config"))
	d := &Daemon{changeCh: make(chan struct{}, 1), saveCh: make(chan struct{}, 1), panes: map[string]*pane{}}
	d.viewed.Store(new(string))
	p, err := newPane(d, "s", proto.Pane{ID: "a", Dir: t.TempDir(), Status: proto.Idle, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.term.Close() })
	if !strings.HasPrefix(p.info.TTY, "/dev/ttys") {
		t.Errorf("pane tty %q, want a /dev/ttys* device", p.info.TTY)
	}
}
```

(`filepath`, `runtime`, `strings` are already imported there; mirror the scaffolding `TestAgentPane` actually uses — if it builds panes differently, follow that.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestPaneTTY -v` (with the Global Constraints toolchain prefix)
Expected: FAIL — `p.info.TTY` is `""` (field does not compile yet → add the field first or expect a compile error; either failure is the right red).

- [ ] **Step 3: Implement**

- `proto.go`: after `PID      int  \`json:"pid,omitempty"\`` add:

```go
	// TTY is the pane's terminal device (/dev/ttys012), as a jump from
	// outside names it, set whenever the pane's pty (re)starts.
	TTY string `json:"tty,omitempty"`
```

- `pty_unix.go`: `ptyProc` gains `tty string`; in `startPTY`, set it from `slaveName` (where `p := &ptyProc{…}` is built or right after).
- `pty_windows.go`: add the `tty string` field to its `ptyProc` (both files define the struct; only unix sets it).
- `session.go` `start()`: beside `s.info.PID = s.shellPID` (line 169) add `s.info.TTY = proc.tty`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/daemon/ -run TestPaneTTY -v`
Expected: PASS

- [ ] **Step 5: Full internal gates**

Run: `go vet ./... && go test ./internal/...`
Expected: clean, all pass (no proto version bump, no golden JSON breakage).

- [ ] **Step 6: Commit**

```bash
git add internal/proto/proto.go internal/daemon/pty_unix.go internal/daemon/pty_windows.go internal/daemon/session.go internal/daemon/session_test.go
git commit -m "Carry each pane's terminal device in its state"
```

---

### Task 2: Parse and resolve a kero://focus URL

**Files:**
- Create: `jump.go` (root package)
- Test: `jump_test.go`

**Interfaces:**
- Consumes: `proto.Session` (`.Panes`, `.ID`), `proto.Pane` (`.ID`, `.AgentSession *proto.AgentSession`, `.TTY`, `WorkDir() string` — ProcessDir first, else Dir), `proto.State` (`.Sessions`).
- Produces (Task 3 consumes these exact names):

```go
type jumpQuery struct{ session, tty, cwd string }
func parseJumpURL(u string) (jumpQuery, bool)
// ok=false unless scheme is kero and host is focus, both case-insensitive;
// query params session/tty/cwd, URL-decoded; absent or empty → "".
func resolveJump(st *proto.State, local func(paneID string) bool, q jumpQuery) (session, pane string)
// "" , "" when nothing matches. Cascade, Vibe Island's documented order:
// 1. session: any pane (any host) whose AgentSession.ID == q.session
// 2. tty: a pane where local(p.ID) && p.TTY == q.tty (exact)
// 3. cwd: panes where p.WorkDir() == q.cwd (exact); several match → prefer
//    local, then the most recent Activity (first in state order breaks a
//    remaining tie); an empty parameter skips its level.
```

- [ ] **Step 1: Write the failing tests**

`jump_test.go`, table-driven, pure functions — no App needed. Build one `proto.State` fixture with: a local session `sL` (panes: `pAgent` with `AgentSession: &proto.AgentSession{Agent: "claude", ID: "ses_1"}`, `TTY: "/dev/ttys001"`, `Dir: "/repo"`; `pIdle` with `TTY: "/dev/ttys002"`, `Dir: "/tmp"`, older `Activity`), a remote session `sR` (pane `pRemote` with `TTY: "/dev/ttys001"` — same name, remote — and `Dir: "/repo"`; pane `pRecent` with `Dir: "/repo"`, newer `Activity`). `local` = membership in a map of local pane ids. Cases:

- session hit: `q{session: "ses_1"}` → (`sL`, `pAgent`)
- session on remote-only id: `q{session: "ses_2"}` with a remote pane carrying it → hit (any host)
- tty hit: `q{tty: "/dev/ttys001"}` → (`sL`, `pAgent`) not `pRemote` (local only)
- tty of a remote-only device: no local match → falls through to cwd level (document the cascade: with `q{tty: "/dev/ttys009", cwd: "/repo"}` → hits the local `/repo` pane)
- cwd exact: `q{cwd: "/tmp"}` → (`sL`, `pIdle`)
- cwd several local matches → most recent `Activity` wins
- cwd local vs remote same dir → local wins
- empty params skip their level: `q{cwd: ""}` with a dir-less pane (`Dir: ""`) → no match; `q{}` → no match
- prefix does not match: `q{session: "ses_1x"}`, `q{tty: "/dev/ttys00"}` → no match

`TestParseJumpURL`: `kero://focus?session=a&tty=%2Fdev%2Fttys003&cwd=%2FUsers%2Ffoo%2Fmy%20project` decodes to the right values; `kero://FOCUS?cwd=/x` ok; `KERO://focus?cwd=/x` ok; `kero://run?cwd=/x` → false; `iterm://focus` → false; missing params → ""; empty value `cwd=` → "".

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run 'TestParseJumpURL|TestResolveJump' -v`
Expected: FAIL (jump.go has no such functions — compile error is the right red).

- [ ] **Step 3: Implement in `jump.go`**

Package `main`, English why-comments. `net/url.Parse` + `Query()`; `strings.EqualFold` for scheme and host. Resolver walks `st.Sessions` then `s.Panes`; each level fully scans before falling to the next; cwd level collects candidates and picks by (local first, Activity desc, state order). No App dependency — pure.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -run 'TestParseJumpURL|TestResolveJump' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add jump.go jump_test.go
git commit -m "Resolve a kero://focus URL to its pane"
```

---

### Task 3: Wire the URL into the app

**Files:**
- Modify: `jump.go` (add the App methods), `app.go:93-95` (App struct: one field), `app.go` end of `setState` (one call), `main.go:99` (one registration)
- Test: `jump_test.go`

**Interfaces:**
- Consumes: Task 2's `jumpQuery`/`parseJumpURL`/`resolveJump`; `App.update(fn)` (app.go:226 — runs fn on the main thread; no-ops while `a.win == nil`), `App.setState(h *host, st proto.State)` (app.go:298), `App.local() *host` (hosts.go:198, `h.name == ""` is this machine), `App.owner map[string]*host` (app.go:92 — session/project/pane id → host), `App.selectSession(id)` (app.go:520), `App.setFocus(s *proto.Session, pane string)` (panes.go:64 — idempotent), `App.showWindow()` (tray.go:111), `testApp(t)` (view_test.go:26).
- Produces:

```go
func (a *App) openJump(url string)          // the OnOpenURL callback
func (a *App) takePendingJump(h *host)      // called at the end of setState
// App gains: pending *jumpQuery
```

- [ ] **Step 1: Write the failing tests**

In `jump_test.go`, using `testApp(t)` (its `a.st` holds one session of two panes; `a.win` is nil, `a.loaded` false — the cold-start shape). Assign the panes' `TTY`/`Dir`/`AgentSession` directly in `a.st` first. For host plumbing, build `&host{}` (local: empty name) and set `a.owner[<pane id>]` for the local panes. Tests:

- `TestOpenJumpColdStartWaits`: `a.openJump("kero://focus?cwd=/tmp")` with `a.loaded == false` → `a.pending != nil`, nothing selected.
- `TestTakePendingJumpHits`: `a.pending = &jumpQuery{…}` matching a pane → `a.takePendingJump(&host{})` → pending cleared, `a.selected` is the session, `a.focus[session]` is the pane.
- `TestTakePendingJumpDropsAfterLocalState`: pending that matches nothing; `takePendingJump(&host{})` (a local host) → pending cleared, selection unchanged.
- `TestTakePendingJumpKeepsForRemote`: pending matching nothing local; `takePendingJump(&host{name: "remote"})` → pending kept.
- `TestOpenJumpWarm`: set `a.win` via `ui.NewTester` only if `view_test.go` already builds windows in tests cheaply; otherwise cover the warm path through `takePendingJump` and leave the warm branch as a two-line delegate (`openJump` → `a.update(func(){ … })` with the same `jumpTo` helper) exercised by the cold tests' helper. Prefer whatever `view_test.go` makes easy — do not build new UI scaffolding.

`jumpTo(q)`: `s, p := resolveJump(&a.st, localPred, q)`; no hit → `a.showWindow()`; hit → `selectSession(s)`, then `setFocus(a.session(s), p)` unconditionally (idempotent, keeps app view and kerod in step), then `showWindow()`. `localPred(paneID)`: `h := a.owner[paneID]; return h != nil && h.name == ""`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run 'TestOpenJump|TestTakePendingJump' -v`
Expected: FAIL (methods/field don't exist).

- [ ] **Step 3: Implement**

- `app.go` App struct (near `st proto.State`, line 93): `pending *jumpQuery // a kero://focus URL that waits for the state to resolve it`.
- `jump.go`:

```go
func (a *App) openJump(url string) {
	q, ok := parseJumpURL(url)
	if !ok {
		return
	}
	// mygo hands launch URLs over once the windows are up, so the
	// window a.update needs is there; only the state may lag.
	a.update(func() {
		if !a.loaded {
			a.pending = &q
			return
		}
		a.jumpTo(q)
	})
}
```

Wait — `a.update` no-ops while `a.win == nil`, which would drop a URL that beats the window. mygo delivers launch URLs after ready (its app.go handleReady opens windows first), so in practice `a.win` exists; still, guard explicitly: if `a.win == nil`, set `a.pending` outside the update (the mygo callback runs on the app thread; the cold tests exercise exactly this). Final shape: `if a.win == nil { a.pending = &q; return }` then the `a.update` above.

- `takePendingJump(h)`: `if a.pending == nil { return }`; resolve against `&a.st`; hit → consume + `jumpTo(*a.pending)`; no hit → if `h == a.local()` (or `h.name == ""`) → consume, `jumpTo` no-target (window still comes up); else keep pending.
- `app.go` `setState`: last line before `a.notify(first)`… after the reselect block, add `a.takePendingJump(h)` (setState's own thread discipline carries it).
- `main.go` beside `OnSecondInstance` (line 99): `mygo.App.OnOpenURL(a.openJump)`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -run 'TestOpenJump|TestTakePendingJump|TestParseJumpURL|TestResolveJump' -v`
Expected: PASS

- [ ] **Step 5: Root gates**

Run: `go vet ./... && go test . && go tool mygo vet . && go test ./internal/...`
Expected: all clean (`mygo vet` checks stored build values and goroutine captures — the `a.update` wrapping is what it looks for).

- [ ] **Step 6: Commit**

```bash
git add jump.go jump_test.go app.go main.go
git commit -m "Jump to the pane a kero://focus URL names"
```

---

### Task 4: The Vibe Island rule, installed from settings

**Files:**
- Modify: `jump.go` (rule helpers, bottom of file), `settings_view.go:69` (settingsState field), `:139-141` (refreshAgents), `:1028`-ish (new group after the skills rows, Agents page), `settings_search.go` (one entry), `locales/en.json`, `locales/ja.json`, `locales/zh-Hans.json`, `locales/catalog_gen.go` (regenerated)
- Test: `jump_test.go`

**Interfaces:**
- Consumes: `mygo.App.Name()`, `mygo.IsDev()`, `proto.Name()` ("kero" / "kero-dev"), `settingsGroup`/`settingRow`/`button`/`a.toastIn` (settings_view.go), `cli.SkillStates` rows as the visual pattern, `a.ss` refresh pattern.
- Produces:

```go
func vibeIslandSetup() (displayName, fileName, bundleID string)
// displayName = mygo.App.Name(); fileName = proto.Name() + ".json"
// ("kero.json"/"kero-dev.json"); bundleID = "sh.kero" (+".dev" when mygo.IsDev())
func vibeIslandRule(displayName, bundleID string) []byte // indented JSON, stable field order
func vibeIslandRulePath(home, fileName string) string    // ~/.vibe-island/integrations/<fileName>
func installVibeIslandRule(home, displayName, fileName, bundleID string) error
func removeVibeIslandRule(home, fileName string) error
func vibeIslandRuleState(home, fileName string, want []byte) string // "missing"|"current"|"outdated"
// settingsState gains: vibeRule string (that state), refreshed in refreshAgents
```

Rule JSON the spec locks (field order as written):

```json
{
  "displayName": "Kero",
  "bundleIdentifier": "sh.kero",
  "jumpRule": {
    "method": "urlScheme",
    "template": "kero://focus?session={session_id}&tty={tty}&cwd={cwd}"
  }
}
```

- [ ] **Step 1: Write the failing tests**

In `jump_test.go` (pure file/JSON tests, temp home):

- `TestVibeIslandRule`: `vibeIslandRule("Kero", "sh.kero")` equals the exact JSON above (bytes; `json.MarshalIndent` of a struct with those fields in that order).
- `TestVibeIslandInstallState`: on a temp home — state "missing"; `installVibeIslandRule` → state "current", file exists at `vibeIslandRulePath` with mode 0644 and the rule bytes; overwrite with other bytes → "outdated"; `removeVibeIslandRule` → "missing".

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run 'TestVibeIsland' -v`
Expected: FAIL (functions undefined).

- [ ] **Step 3: Implement helpers in `jump.go`**

`os.MkdirAll(dir, 0o755)` + write (plain `os.WriteFile` 0644 — the file is a fresh integration, not a config users edit); state compares bytes to `want`. Then the settings row in `settings_view.go` on the Agents page, after the skills group, mirroring the CLI group (settings_view.go:988-1003) with the skills rows' Install/Update/Remove button trio (settings_view.go:1010-1028):

- group: `locales.SetVibeIsland()` / detail `locales.SetVibeIslandDetail()`
- row: `locales.SetVibeIslandRow()`, detail = the rule path when current, else `locales.SetVibeIslandNotInstalled()`
- Install/Update button when state ≠ current, Remove when it is; on click run the helper (a file write — no goroutine needed) and refresh `a.ss.vibeRule` in place; failures go to `a.toastIn(c, err.Error())` like the skills rows.
- `refreshAgents` (settings_view.go:139) also sets `a.ss.vibeRule = vibeIslandRuleState(home, fileName, rule)` — compute `home` with `os.UserHomeDir()` there; ignore its error as the skills code does with what it can't control (an unwritable home just shows "missing").

Locale keys (en shown; ja/zh-Hans terse translations of the same):
`"setVibeIsland": "Vibe Island"`, `"setVibeIslandDetail": "Clicking an agent in Vibe Island's notch panel jumps to its session and pane. Needs Vibe Island 1.0.22 or newer, and its restart after a change."`, `"setVibeIslandRow": "Jump rule"`, `"setVibeIslandNotInstalled": "Not installed: Vibe Island only raises Kero's window."`

`settings_search.go`: one entry `{"agents", locales.SetVibeIsland(), "vibe island notch jump focus url scheme integration"}` (match the page id the CLI row's page uses — check the neighbors).

- [ ] **Step 4: Regenerate the catalog and run tests**

Run: `go tool mygo generate && go test . -run 'TestVibeIsland' -v`
Expected: `locales/catalog_gen.go` gains the four keys per language; tests PASS. Never hand-edit `catalog_gen.go`.

- [ ] **Step 5: Root gates**

Run: `go vet ./... && go test . && go tool mygo vet . && go test ./internal/...`
Expected: all clean.

- [ ] **Step 6: Commit**

```bash
git add jump.go jump_test.go settings_view.go settings_search.go locales/
git commit -m "Offer Vibe Island's jump rule in settings"
```

---

### Task 5: Declare the scheme, verify, ship the DMG

**Files:**
- Modify: `mygo.json` (top-level)

**Interfaces:**
- Consumes: everything above.
- Produces: `"urlSchemes": ["kero"]` in mygo.json; the DMG in `build/`.

- [ ] **Step 1: Add the scheme**

In `mygo.json`, after `"identifier"`, add `"urlSchemes": ["kero"],` (mygo writes `CFBundleURLTypes` into the bundle's Info.plist; Launch Services registers it on install).

- [ ] **Step 2: Full gates**

Run: `go vet ./... && go test ./internal/... && go test . && go tool mygo vet .`
Expected: all clean. (`go test .` first run downloads libghostty-vt; `-short` skips those if network-bound.)

- [ ] **Step 3: Build the DMG**

Run: `go tool mygo build -platform darwin/arm64` (toolchain prefix from Global Constraints; signing identity and notary profile are already in mygo.json — the keychain must be unlocked as usual for this machine).
Expected: a signed, notarized `Kero` app + DMG under `build/` (note the exact artifact names for the report).

- [ ] **Step 4: Manual smoke test (evidence for the report)**

- `open "kero://focus?cwd=<a dir of a running session>"` → Kero comes frontmost with that session's pane focused.
- `open "kero://focus?session=<an AgentSession id from kero ls --json>"` → same.
- `open "kero://focus?cwd=/nonexistent"` → window frontmost, selection unchanged.
- Install the built DMG, install the rule from settings, restart Vibe Island, click a notch session → exact pane. (If Vibe Island isn't installed on this machine, say so in the report and mark this step skipped.)

- [ ] **Step 5: Commit**

```bash
git add mygo.json
git commit -m "Declare the kero URL scheme"
```
