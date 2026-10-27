# Sidebar Smart Ordering & Files Browser Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the three approved features from the spec: (A) session smart ordering with daemon-persisted pins, (B) the right-side Files view with Quick Look, Open With and remote edit-sync-back, (C) the compact block-indent sidebar.

**Architecture:** A fork cherry-pick baseline (sidebar-sort, then files-view) lands first; Feature A adds a `Pin` field + `session.move` RPC in the daemon and an `order.go` display-sort in the root package; Feature C is pure UI density work; Feature B builds the Files view on the picked baseline and adds `fs.write` plus a purego Quick Look bridge. Display order is computed UI-side (auto base from `Pane.Activity/Since/Created`, then pin chains); the daemon only stores pins and project membership.

**Tech Stack:** Go, mygo UI framework, purego (already a dependency, v0.11.1), per-host kerod JSON-RPC protocol.

**Spec:** `specs/2026-10-26-sidebar-sorting-files-browser-design.md` — the plan argues from the spec; executors read both.

## Global Constraints

- `proto.Version` stays 3; new RPCs and optional fields only (spec, Non-goals).
- mygo element lifecycle rules (`.agents/skills/mygo-maintenance/SKILL.md`): no elements in app state, `a.defer_` for collection edits, `a.update` for cross-goroutine UI work, file IO off the UI thread. `go tool mygo vet .` must stay clean.
- Root package: one feature per file (`xxx.go` + `xxx_test.go`), English comments, minimal diff, no drive-by refactors (CLAUDE.md working rules).
- Locales only via `mygo generate`; never hand-edit `locales/catalog_gen.go`. Three locale files must stay in sync.
- Tests isolate the environment with `t.Setenv("KERO_HOME", t.TempDir())` + temp `KERO_CONFIG`.
- Verification gate per task: `go vet ./...`, `go test ./internal/...`, `go test .` (root tests may need first-run download of libghostty-vt; `-short` skips them), `go tool mygo vet .`.
- Development lands directly on `main`; commits are English imperative, capitalized, no conventional-commit prefixes, no emoji. **Never push.**
- Fork source: `git remote add smallnest https://github.com/smallnest/kero && git fetch smallnest`. sidebar-sort range = `6124a1d^..368aa04` (9 commits, root package only). Files view = `ad3fe7b`, `79e8b1c`. `fileOpenPicker` pref = `e5645f4` (on fork main, on neither branch). Temporary analysis clone (reference, read-only): `/tmp/fork-analysis/smallnest-kero`.

## Review Focus

1. Pin cycle (A pinned after B, B pinned after A — impossible via UI, possible via stored data): `orderSessions` must fall back to auto order, not loop. → Task 3 test.
2. Anchor deleted while followers exist: followers revert to auto order silently. → Task 3 test.
3. Editor atomic save (write-temp-then-rename) replaces the inode: the poller stats by path and must survive it. → Task 13 test.
4. Remote file changed concurrently since our read: `fs.write` with stale hash must fail with a conflict error, never silent overwrite. → Task 12 + 13 tests.
5. Old remote kerod without `fs.write` answers `unknown method`: UI shows the existing hostUpdate hint instead of a raw error. → Task 13 test.

---

### Task 1: Cherry-pick the sidebar-sort baseline

**Files:**
- Modify (via picks): `view.go`, `view_test.go`, `icons.go`, possibly `app.go` conflict resolution
- Reference (read-only): `/tmp/fork-analysis/smallnest-kero`

**Interfaces:**
- Produces: `prefs.SidebarGroup` (`"project"|"none"|"date"`), `prefs.SidebarOrder` (`"created"|"updated"`), `projectBadgeColor(id string)`, `hostGroups` as picked from the fork; later tasks build on these names.

- [ ] **Step 1: Add the fork remote and pick**

```bash
git remote add smallnest https://github.com/smallnest/kero 2>/dev/null; git fetch smallnest
git cherry-pick 6124a1d^..368aa04
```

Resolve conflicts against our tree (ours has Vibe Island commits the fork lacks; theirs may reference upstream symbols we don't have — prefer our hunks for unrelated drift, keep the feature hunks). Do NOT take whole files.

- [ ] **Step 2: Verify the picks**

Run: `go vet ./... && go test ./internal/... && go test . -run 'TestProject|TestSidebar|TestCollapse' -v`
Expected: all PASS. Run: `go tool mygo vet .` — clean.

- [ ] **Step 3: Commit any conflict resolutions folded into the picks**

Cherry-pick auto-commits; resolutions amend in place. Verify: `git log --oneline -10` shows the 9 picked commits.

### Task 2: `Pin` field + `session.move` RPC (daemon)

**Files:**
- Modify: `internal/proto/proto.go` (Session/savedSession side), `internal/daemon/rpc.go`, `internal/daemon/store.go`, `internal/daemon/sessions.go`
- Test: `internal/daemon/rpc_test.go` (extend; follow existing test file naming)

**Interfaces:**
- Produces:
```go
// internal/proto/proto.go
// In Session (optional, no version bump):
Pin string `json:"pin,omitempty"` // "", a session id, or "head"
type MoveSessionParams struct {
    Session string  `json:"session"`
    Pin     string  `json:"pin"`
    Project *string `json:"project,omitempty"` // when set, move session to this project
}
```
- Produces: RPC method `"session.move"` returning `done` result; rejects unknown project with an error; persists via the existing state store.

- [ ] **Step 1: Write failing tests** — pin round-trip through `session.move` and a daemon restart (store→restore keeps `Pin`); `Project` move to another project on the same host; `Project` naming a missing project returns an error; old-state tolerance: a pre-pin `state.json` (no `Pin` field) loads with `Pin == ""`, and wire JSON omits `pin` when empty. Follow the existing `project.move` test pattern in `internal/daemon/rpc_test.go` (see `rpc.go:496` for the pattern being mirrored).

- [ ] **Step 2: Run** `go test ./internal/daemon/ -run 'TestSessionMove|TestSessionPin' -v` — FAIL (method missing).

- [ ] **Step 3: Implement** — add the two proto declarations above (and `Pin` to `savedSession` in `store.go`), a `case "session.move"` in `rpc.go` delegating to a small `moveSession` in `sessions.go` that validates the project exists on that host, sets pin/project, and triggers the existing save loop. English comments explaining why display order is not computed here (see spec Feature A).

- [ ] **Step 4: Run** the same tests — PASS. Then `go vet ./... && go test ./internal/...` — PASS.

- [ ] **Step 5: Commit** — `Add session pins and the session.move RPC` (`git add` the four files + test).

### Task 3: `order.go` display ordering

**Files:**
- Create: `order.go`, `order_test.go` (root package)

**Interfaces:**
- Consumes: `proto.Session` (with promoted `Pane.Created/Activity/Since` and the new `Pin`), `prefs.SidebarOrder`.
- Produces:
```go
// orderSessions returns sessions of ONE group (same project, or same date
// bucket) in display order: auto base (order pref) then pin chains.
func orderSessions(group []proto.Session, order string) []proto.Session
// updatedKey(s proto.Session) int64 = Activity>0?Activity : Since>0?Since : Created
```

- [ ] **Step 1: Write failing tests** in `order_test.go`: created asc / updated desc bases; pin follows anchor across the base order; chain (A→B→C) follows head B; `"head"` sorts before the group's first item; insert cannot split a chain (a newly unpinned session sorts into the base order while the chain stays glued — assert chain adjacency); deleted anchor → follower back to auto; pin pointing outside the group (date mode) → ignored; flat (`none`) mode treats the whole host as one group and keeps pins active; pin cycle → both fall back to auto (Review Focus 1, 2). Table-driven, sessions built with literal timestamps.

- [ ] **Step 2: Run** `go test . -run TestOrder -v` — FAIL (no order.go).

- [ ] **Step 3: Implement `orderSessions`** — stable sort by base key; then iterate base order emitting each session followed by its followers resolved as chains (map anchor→followers; visited set guards cycles); `"head"` chains emitted first. One pass, O(n log n).

- [ ] **Step 4: Run** `go test . -run TestOrder -v` — PASS.

- [ ] **Step 5: Commit** — `Compute sidebar display order from auto sort plus pins`.

### Task 4: Sidebar drag, arrows, reset-to-auto UI

**Files:**
- Modify: `view.go` (`sessionRow`, `sessionMenu`), `app.go` (small glue if needed)
- Test: `view_test.go` (extend)

**Interfaces:**
- Consumes: `orderSessions` (Task 3), `session.move` RPC client call (mirror how `project.move` is invoked from the picked `moveProject`), `ui.Drag/ui.DragOver/ui.Drop` (panes.go:221-232 pattern).
- Produces: `type sessionDrag struct{ host, session, project string }`; `func (a *App) moveSession(id, pin string, project *string)` calling the RPC via `a.update`.

- [ ] **Step 1: Write failing tests** with `ui.NewTester`: drag row X onto row Y (same host) → `session.move{Pin: Y}`; drag onto another group's header (same host) → `{Pin: "head", Project: <that group>}`; drag across hosts → no call; hover-arrow up/down on a row → pin to neighbor; context menu "Reset to auto order" → `{Pin: ""}`. Model on the picked `TestProjectDrag`.

- [ ] **Step 2: Run** `go test . -run 'TestSessionDrag|TestSessionArrows|TestSessionReset' -v` — FAIL.

- [ ] **Step 3: Implement** — `Drag(sessionDrag{…})` + `DragOver` insert line + `Drop` on rows/headers (same-host guard `d.host == g.host`, 0.35 opacity while dragging, matching the picked project drag); small up/down arrow buttons visible on hover; one menu item. Wire `sidebar()` to render via `orderSessions` per group (per date bucket in date mode; in flat `none` mode the whole host is one group and pins stay active). Add one small test asserting ⌘1–9 `visibleOrder` matches the rendered order.

- [ ] **Step 4: Run** the tests — PASS; then `go test . && go tool mygo vet .` — PASS.

- [ ] **Step 5: Commit** — `Reorder sessions by drag, arrows, and reset in the sidebar`.

### Task 5: `sidebar.density` setting + compact metrics

**Files:**
- Modify: `internal/settings/settings.go` (field, Default, normalize), `internal/settings/config.go` (keys table), root `settings_view.go` (row + search entry), `view.go` (metrics)
- Test: `internal/settings` tests (extend) + `view_test.go`

**Interfaces:**
- Produces: `Settings.SidebarDensity string` — key `sidebar.density`, values `"compact"` (default) | `"standard"`; `view.go` gains `type densityMetrics struct{ rowH, hdrH, fontPx float32 }` and `func (a *App) sidebarDensity() densityMetrics` returning `{26, 24, 12.5}` or `{34, 28, 13.5}`.

- [ ] **Step 1: Write failing tests** — settings round-trip + default compact + normalize of unknown values to compact; a view test asserting `sidebarDensity()` values for both settings. Follow the 5-place convention (field → keys → Default → normalize → UI row/search).

- [ ] **Step 2: Run** `go test ./internal/settings/ -run TestSidebarDensity -v && go test . -run TestSidebarDensity -v` — FAIL.

- [ ] **Step 3: Implement** the five places + swap `rowH`/`headerH`/font literals in `sessionRow`/`groupView` to read `sidebarDensity()`. `standard` must render exactly as before (regression: existing view tests unchanged).

- [ ] **Step 4: Run** the tests — PASS; `go test .` full — PASS (no regression in standard mode).

- [ ] **Step 5: Commit** — `Add the sidebar density setting with compact as the default`.

### Task 6: Block-indent group rendering (compact mode)

**Files:**
- Modify: `view.go` (`groupView`, `sessionRow` layout)
- Test: `view_test.go` (extend)

**Interfaces:**
- Consumes: `(a *App).sidebarDensity()` (Task 5), `projectBadgeColor` (Task 1 picks).
- Produces: compact rendering where a group's children form one block: left 2px vertical bar in `projectBadgeColor(id)` spanning the block, children inset 14px + bar + 8px; standard mode unchanged.

- [ ] **Step 1: Write a failing test** asserting the compact tree geometry: children x-offset smaller than standard's (derive the expectation from `rowInset` view.go:30 + badge width rather than a magic number); assert the block bar is drawn (use the picked badge/geometry test patterns; assert via the tester's layout/bounds, as `TestSidebarDateGroups` does).

- [ ] **Step 2: Run** `go test . -run TestCompactBlock -v` — FAIL.

- [ ] **Step 3: Implement** — in compact mode wrap children in one container that draws the 2px bar (Draw hook with `p.Rect`, precedent `statusIcon` view.go:678) and insets rows; no per-row glyphs.

- [ ] **Step 4: Run** `go test . && go tool mygo vet .` — PASS.

- [ ] **Step 5: Commit** — `Render compact sidebar groups with a color bar block indent`.

### Task 7: Cherry-pick the files-view baseline + fileOpenPicker

**Files:**
- Modify (via picks): `side.go`, new `files.go` + tests from fork, `internal/proto/proto.go` (`Pane.File`, `CreateParams.File`, `FilePane()/Shown()`), `internal/daemon/{sessions.go,session.go,store.go}`, `panes.go`, plus `e5645f4`'s `fileOpenPicker` pref bits
- Reference: `/tmp/fork-analysis/smallnest-kero`

**Interfaces:**
- Produces: side view id `"files"` with the fork's Files tab, fs client, tree, preview/file-pane/$EDITOR opening, `prefs.FileOpen`; shortcut as picked (`CmdOrCtrl+Alt+F` — Task 8 rebinds).

- [ ] **Step 1: Pick** `git cherry-pick ad3fe7b 79e8b1c e5645f4` — resolve conflicts per-commit against our tree (our tree already has the fs RPCs; keep ours there). If `e5645f4` depends on other fork-main commits, port just the picker hunks by hand into a follow-up commit rather than pulling extra history.

- [ ] **Step 2: Verify** `go vet ./... && go test ./internal/... && go test . && go tool mygo vet .` — PASS.

- [ ] **Step 3: Confirm** `git log --oneline -4` shows the picks (plus optional hand-port commit).

### Task 8: Rebind `files.toggle` to Cmd+Shift+F / Ctrl+Alt+F

**Files:**
- Modify: `internal/actions/actions.go` (Keys + pcKeys), root `actions.go` only if the picked action id differs
- Test: existing action tests; `internal/actions` table test if present

**Interfaces:**
- Produces: `files.toggle` with `Keys: "Cmd+Shift+F"` (macOS) and `pcKeys` override `"Ctrl+Alt+F"`. No other action's keys change (`pane.restart` keeps `CmdOrCtrl+Shift+R`).

- [ ] **Step 1: Write/extend a failing test** asserting the action table has `files.toggle` with exactly these bindings and no duplicate key with `find.open`/`pane.restart`.

- [ ] **Step 2: Run** `go test ./internal/actions/ -run TestFilesToggle -v` — FAIL.

- [ ] **Step 3: Implement** the two key-table edits; run `mygo generate` if locale strings changed (they should not — the action already exists from the picks).

- [ ] **Step 4: Run** the test + `go test ./internal/...` — PASS.

- [ ] **Step 5: Commit** — `Bind the files toggle to Cmd+Shift+F and Ctrl+Alt+F`.

### Task 9: Quick Look spike (throwaway)

**Files:**
- Create (temporary, deleted before commit): `spike_quicklook.go` with `//go:build spike` tag + a note file `spike-notes.md` (uncommitted)

**Interfaces:**
- Produces: a Go/No-Go verdict + the minimal binding list for Task 10. Not part of the shipped tree.

- [ ] **Step 1: Build a spike** using purego: resolve `QLPreviewPanel.sharedPanel` (or `+[QLPreviewPanel sharedPreviewPanel]`), a minimal `QLPreviewPanelDataSource` (two methods, count=1, `previewItemAt` returning the URL), `makeKeyAndOrderFront` on the app's key window. Trigger from a temporary debug menu item. Confirm the panel appears over the kero window, shows the file, and closes on Esc, without breaking mygo's event loop.

- [ ] **Step 2: Record verdict + exact bindings/ObjC signatures that worked** in `spike-notes.md`; message the orchestrator (Go/No-Go + binding list). If No-Go: Task 10 degrades to "Space = in-kero preview" (spec's documented fallback); note that in the plan execution report.

- [ ] **Step 3: Delete the spike files** (`rm spike_quicklook.go spike-notes.md` after the verdict is relayed; the notes' content lives on in the execution report).

### Task 10: Quick Look implementation

**Files:**
- Create: `quicklook_darwin.go` (root package); Modify: the Files view's tree element for the Space shortcut
- Test: `quicklook_darwin_test.go` (compile/smoke level; the panel itself is not automatable)

**Interfaces:**
- Consumes: spike verdict + bindings (Task 9).
- Produces: `//go:build darwin` — `func quickLook(path string) error` presenting a shared `QLPreviewPanel` with the single URL; non-darwin builds compile with Space as a no-op (build-tagged stub `quicklook_other.go` if the call sites need it).

- [ ] **Step 1: Write the failing compile/smoke test** — darwin: `quickLook` exists and returns nil error on a small temp text file (panel presentation asserted only manually; test guards the binding layer's lifetime: two calls in a row reuse the shared panel). Non-darwin CI is not exercised; keep the stub trivial.

- [ ] **Step 2: Run** `go test . -run TestQuickLook -v` — FAIL.

- [ ] **Step 3: Implement** the purego bridge per the spike notes (single shared data source, no per-call allocation of panels; callbacks must not capture elements — SKILL.md rules).

- [ ] **Step 4: Wire Space** on the Files tree's selected row: `it.Shortcut(0, ui.KeySpace)` (API shape per browser.go:424) → if selection is a file on the local host, `quickLook(path)`; directories: no-op. Remote-host routing (download-then-preview) is added in Task 13 Step 4, which routes ALL open paths through `materialize`.

- [ ] **Step 5: Run** `go test . -run TestQuickLook -v && go tool mygo vet .` — PASS.

- [ ] **Step 6: Commit** — `Preview files with Quick Look from the files view`.

### Task 11: Open With menu

**Files:**
- Create: `openwith_darwin.go` (+ `openwith.go` shared); Modify: Files view context menu
- Test: `openwith_test.go`

**Interfaces:**
- Produces:
```go
// openwith.go (all platforms)
func openWithCommand(app, path string) []string // darwin: {"open","-a",app,path}; other: {app, path}
func vscodeCommand(path string) []string        // darwin: {"open","-a","Visual Studio Code",path}; else {"code",path}
// openwith_darwin.go
func applicationsForPath(path string) ([]AppEntry, error) // LaunchServices enumeration
type AppEntry struct{ Name, Path string }
```
- Menu: "Open" (`mygo.Shell.OpenPath`), "Open in VSCode", "Choose App…" (darwin only).

- [ ] **Step 1: Write failing tests** for `openWithCommand`/`vscodeCommand` literal outputs on darwin; `applicationsForPath` smoke test on a `.txt` returning at least one entry containing "TextEdit" (skippable if LS flaky in CI — guard with `t.Skip` on empty, but assert no error shape).

- [ ] **Step 2: Run** `go test . -run TestOpenWith -v` — FAIL.

- [ ] **Step 3: Implement** — purego LaunchServices (`LSCopyApplicationURLsForURL`) per spike-adjacent research; run chosen app via `openWithCommand` + `exec.Command` off the UI thread (`a.update` to report errors).

- [ ] **Step 4: Run** tests + `go tool mygo vet .` — PASS.

- [ ] **Step 5: Commit** — `Add an Open With menu to the files view`.

### Task 12: `fs.write` RPC (daemon)

**Files:**
- Modify: `internal/proto/proto.go`, `internal/daemon/fs.go`, `internal/daemon/rpc.go`
- Test: `internal/daemon/fs_test.go` (extend)

**Interfaces:**
- Produces:
```go
type FSWriteParams struct {
    Path    string `json:"path"`
    Content []byte `json:"content"`
    Hash    string `json:"hash"` // sha256 hex of expected current content; "" = force
}
```
- Produces: RPC `"fs.write"` — writes atomically (temp+rename), rejects `Content` > 32MB, rejects with error `"conflict"` when the file exists and its current sha256 != `Hash` (empty Hash forces). Mirrors `git.revert`'s lock philosophy (spec Approaches 3).

- [ ] **Step 1: Write failing tests** — round-trip write+read; conflict on stale hash (Review Focus 4); force write with empty hash; 32MB cap rejection; write to a new file; missing parent dir error.

- [ ] **Step 2: Run** `go test ./internal/daemon/ -run TestFSWrite -v` — FAIL.

- [ ] **Step 3: Implement** the case in `rpc.go` + helper in `fs.go`.

- [ ] **Step 4: Run** the tests + `go vet ./... && go test ./internal/...` — PASS.

- [ ] **Step 5: Commit** — `Add an fs.write RPC with hash conflict detection`.

### Task 13: Remote file cache + sync-back (`files_open.go`)

**Files:**
- Create: `files_open.go`, `files_open_test.go` (root package)
- Modify: Files view open paths to route through materialization

**Interfaces:**
- Consumes: `fs.read`, `fs.write` (Task 12), `connVersion`/behind-style host capability checks, hostUpdate hint strings.
- Produces:
```go
func (a *App) materialize(host, remotePath string) (localPath string, err error)
// remote: downloads to proto.Dir()/files/<host-hash>/<path-hash>-<base>, records
// {local → host, remotePath, sha256(content)}; local host: returns remotePath itself.
func (a *App) startUploadWatcher() // goroutine: poll mapped files' mtime+size every 500ms; on change wait 1s quiet then fs.write with recorded hash; update hash on success; drop deleted mappings; conflict/unknown-method errors surface a dialog / hostUpdate hint.
```

- [ ] **Step 1: Write failing tests** — materialize remote twice reuses cache (no second read); local host returns path unchanged; watcher: modify cached file → upload fired once with correct hash (fake or real 500ms/1s timers acceptable in-test); second modification → second upload with updated hash; atomic save (write temp + rename over cache path) still detected (Review Focus 3); stale-hash conflict → dialog path invoked, no overwrite (Review Focus 4); unknown-method error → hostUpdate hint path (Review Focus 5); file >32MB rejected before download. Use `KERO_HOME` tempdir and a stub host connection where the RPC client interface allows; otherwise target a real local kerod like existing tests do.

- [ ] **Step 2: Run** `go test . -run TestFilesOpen -v` — FAIL.

- [ ] **Step 3: Implement** — cache map owned by `app` (not elements), watcher goroutine started once, results marshalled with `a.update`; all IO off the UI thread; debounce state per file (last-change time, in-flight upload guard).

- [ ] **Step 4: Route open paths** — double-click preview/file-pane/$EDITOR, Space Quick Look, and Open With all call `materialize` first for remote hosts.

- [ ] **Step 5: Run** `go test . -run TestFilesOpen -v` then full `go test . && go tool mygo vet .` — PASS.

- [ ] **Step 6: Commit** — `Cache remote files locally and sync edits back with fs.write`.

### Task 14: Final regression + manual QA

**Files:** none new.

- [ ] **Step 1: Full gate** — `go vet ./... && go test ./internal/... && go test . && go tool mygo vet .` — all PASS.

- [ ] **Step 2: Manual QA smoke** via `go tool mygo dev` (build takes a while; single run): compact tree renders with color bars; drag a session (local + same-host groups); create a session and confirm auto-insert while chains stay glued; Cmd+Shift+F opens Files; browse a dir; Space previews; double-click opens; edit a cached remote file with an external editor and confirm upload (check remote mtime); trigger a conflict and confirm the dialog. Record results in the execution report (evidence per superpowers verification rules).

- [ ] **Step 3: Update CHANGELOG** if the user's release flow expects it at this stage (default: skip; user cuts releases via RELEASING.md).
