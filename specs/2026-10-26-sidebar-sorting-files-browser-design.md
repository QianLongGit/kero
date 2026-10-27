# Sidebar Smart Ordering & Files Browser — Design

Date: 2026-10-26
Status: pending approval

## Intent and why

The left sidebar shows projects and sessions in daemon array order with no way
to rearrange them, at a density that wastes vertical space, and there is no way
to browse or open files without leaving the terminal. The user wants three
things: (A) drag/arrow reordering of sessions that persists and keeps manually
placed sessions glued behind their anchor while new sessions still auto-sort;
(B) a right-side Files view with in-kero open, macOS Quick Look, and edit-sync
back for remote hosts; (C) a compact, tree-styled sidebar. A reference
implementation exists in the smallnest/kero fork (grouping + sorting, files
view). We adopt its patterns where they fit and build the rest ourselves.

## Goals

### A — session smart ordering

- Reorder sessions by drag or hover arrows; ordering survives restart and is
  consistent across clients.
- A manually placed session is *pinned after* an anchor session and always
  follows it. New sessions still enter by the auto rule (fork's `created` /
  `updated` setting); pinned chains are never split by inserts — an insert
  aimed inside a chain lands after the chain's tail.
- Dragging a session into another project group moves it to that project.
- A context-menu action resets a session to auto order.

### B — right-side files browser

- `Cmd+Shift+F` toggles a Files view in the existing right side panel; the tree
  comes from the pane's host kerod (`fs.list`), so local and configured remote
  hosts behave the same.
- Single-click selects; double-click opens in kero (fork's `fileOpenPicker`
  setting: preview / file pane / `$EDITOR`); Space opens macOS Quick Look on
  the selection (native `QLPreviewPanel`).
- A context menu "Open With": system default app, VSCode, or pick from
  installed apps.
- Remote files materialize into a local cache directory; any edit to the cached
  file — by any editor — uploads back after a debounce, guarded by a read-time
  content hash.

### C — compact tree sidebar

- A `sidebar.density` setting (`compact` default, `standard` = today's look).
- Compact: row height 26, group header 24, 12.5px text; group children drawn
  as a block with a 2px group-color bar on the left (color from the same
  palette as the project badge).

## Non-goals / anti-goals

- No file support for panes where the user manually typed `ssh` inside a shell
  — kero only sees PTY bytes there. Accepted by the user.
- No SFTP: all remote file IO goes through per-host kerod RPC.
- No in-kero editable file view; editing happens in external editors and syncs
  back. (The in-kero preview/file pane stays read-only.)
- No third density step; two states only.
- No pane-level rows in the sidebar; the hierarchy stays project → session.
- `proto.Version` stays 3 — additive RPCs and optional fields only.
- No Quick Look API added to mygo; the purego bridge lives in kero.

## Constraints

- mygo element lifecycle rules (`.agents/skills/mygo-maintenance/SKILL.md`):
  no elements in app state, stable handles, `a.defer_` for collection edits,
  `a.update` for cross-goroutine UI work, file IO off the UI thread;
  `go tool mygo vet .` must stay clean.
- Root package: one feature per file (`xxx.go` + `xxx_test.go`); comments in
  English; minimal diff to existing code.
- `internal/` changes run in CI (`go vet ./...`, `go test ./internal/...`);
  root tests run locally (`go test .`, `KERO_HOME` tempdir pattern).
- Locales only via `mygo generate`; never edit `catalog_gen.go`.
- New actions need the full chain: `actions.All` entry, `actionTitles`,
  `a.run` case, menu item, three locale files, regenerate.
- Development lands directly on main (user instruction); commits only when the
  user asks.

## Prior art (smallnest fork) and patch strategy

- Clean patches live on `sidebar-sort` (12 commits: project groups, 8-color
  badges, sort/group menu, project-level drag, tests) and `feat/files-view`
  (Files tab, fs client, file-pane protocol bits ≈ 60 lines). The
  `fileOpenPicker` pref comes from a third commit, `e5645f4`, that lives on
  the fork's main but on neither branch — pick it separately.
- Getting them: `git remote add smallnest https://github.com/smallnest/kero &&
  git fetch smallnest`, then cherry-pick from `smallnest/sidebar-sort` and
  `smallnest/feat/files-view` (network fetch done by the implementer at build
  time; the temporary analysis clone lives at `/tmp/fork-analysis/smallnest-kero`).
- **Range precision**: the sidebar-sort feature is exactly the nine dev commits
  `6124a1d^..368aa04`, touching only `icons.go`(+1), `view.go`(+238),
  `view_test.go`(+207) — zero `internal/` changes. A raw
  `b1edf9c..sidebar-sort` diff mis-baselines (that branch was cut from an
  upstream base two days older than our `b1edf9c`, and a raw diff reads that
  drift as deletions); always pick
  the individual commits.
- The fork's `dev` branch merges upstream up to `35966b0`; our tree is
  at `b1edf9c` + 4 local commits. **Cherry-pick per commit against our tree and
  resolve conflicts by hand; never take whole-file diffs** (they drag in
  upstream drift such as the removed Ended list).
- Already in our tree: `fs.list` / `fs.stat` / `fs.read` RPCs, `paste.file`
  (upload-path precedent), `git.revert` (hash-lock precedent), the side panel
  shell (`side.go`), and the generic `ui.Drag` / `ui.Drop` machinery.
- The fork updated its mygo version; if `ui.Outline` / editor behavior differs
  from ours, adapt rather than copy.

## Approaches considered

1. **Where manual order lives.** Daemon-side pins (chosen) vs UI-side prefs
   (fork's `projectOrder`). Sessions belong to kerod ("the sessions themselves
   are kerod's"); pins in `state.json` give restart and multi-client
   consistency with one source of truth. UI prefs would fork the truth in two.
2. **Anchor semantics.** Sticky anchor chains (chosen, per user) vs the fork's
   "manual items after all auto items" simplification vs free-floating manual
   order. Chains express "always behind that session" exactly.
3. **Sync-back model.** Unified cache-file watcher with debounce (chosen) vs
   editable in-kero pane with explicit save vs fsnotify dependency. The user
   wants edits from *any* editor to sync; polling `mtime+size` by path avoids a
   new dependency and folds naturally into debouncing.
4. **Quick Look route.** purego `QLPreviewPanel` (chosen) vs `qlmanage -p`
   subprocess vs adding a mygo API. The user picked the native panel
   experience; qlmanage shows a detached window and is semi-deprecated.
5. **Shortcut.** `Cmd+Shift+R` is taken by `pane.restart` (both platforms), so
   Files gets `Cmd+Shift+F` on macOS (user's pick; free there). On
   Windows/Linux `Ctrl+Shift+F` is taken by `find.open`, so the PC default is
   `Ctrl+Alt+F` — the fork's own choice, free in the pcKeys table.
6. **Tree style.** Three mockups (guide line, per-row glyphs, block color bar);
   the user chose the block color bar.

## Feature A — session smart ordering

### Data model (daemon, persisted in `state.json`)

- `savedSession` gains `Pin string`: `""` = auto order; a session id = pinned
  after that session; reserved value `"head"` = pinned before the group's first
  item (the drag-to-top case; the drag-to-bottom case needs no special value —
  pinning after the current last session covers it).
- `Pin` travels to clients as an optional `Session` field (no version bump).

### RPC

- `session.move` with params `{Session, Pin, Project?}`: sets the pin and,
  when `Project` is present, moves the session to that project (the
  cross-group "move house" drag). Modeled on `project.move`. The daemon stores
  pins and membership; it does **not** compute display order. The daemon
  validates that `Project` exists on that host and rejects unknown projects —
  cross-host moves are impossible by construction (a session belongs to the
  kerod that owns its PTY).

### UI ordering (`order.go`, root package)

- Display order = per project group: base auto sort (`created` asc / `updated`
  desc, the fork's `sidebarOrder` pref) → then lift each pinned session out of
  the base order and place it directly after its anchor (chains resolve
  anchor→tail; a cycle, impossible via UI but defended anyway, falls back to
  auto). `"head"` pins sort before the group's first item.
- The auto-sort timestamps already exist in our tree, no protocol work:
  `Pane.Since/Activity/Created` (`internal/proto/proto.go:194-198`) are
  promoted into `Session` through its embedded `Pane` (the lead pane's
  snapshot). `updated` order = the fork's fallback chain `Activity > Since >
  Created` (last PTY output, else last status change, else creation).
- Anchors missing (deleted session) or in a different group (only possible in
  `date` grouping, where groups are immutable date buckets and cross-group
  drags are disabled) → the pin is ignored for display.
- Stable sort; recomputed per state push, O(n log n).

### Interactions

- Drag: `ui.Drag`/`ui.DragOver`/`ui.Drop` on session rows (fork's project-drag
  visuals: 35% opacity while dragging, accent insert line). Valid targets are
  groups and rows **on the same host** — the fork's same-host guard carries
  over unchanged, because a session cannot leave its host's kerod; dropping on
  another group's header pins to `"head"`, dropping on rows pins after that
  row; within the host it issues the move-project variant.
- Hover arrows on selected/hovered rows: up/down = re-pin to the neighbor.
- Context menu: "Reset to auto order" (sets `Pin` to `""`).
- ⌘1–9 numbering and folding follow the computed order; unaffected.

### Tests

- `internal/daemon`: `session.move` pin/project persistence, restore after
  restart, old-client field tolerance.
- Root: `orderSessions` unit tests (chains, head, tail, cross-group, date mode,
  deleted anchor) and `ui.NewTester` drag simulations.

## Feature B — right-side files browser

### Entry and tree

- `side.go`: register `"files"` as a second side view (tabs in the side
  header); `files.toggle` action, `Cmd+Shift+F` / `Ctrl+Alt+F`.
- `files.go` (root): tree from `fs.list` on the current pane's host, rooted at
  the pane's git root / workdir (fork behavior), adapted from the fork's
  client (concurrency 4, 50ms refill, skip `.git`, dotfiles always shown). Single-click
  select, double-click open per the `fileOpenPicker` pref ported from the fork
  (preview / file pane / `$EDITOR`; file pane needs the fork's `Pane.File`
  protocol bits cherry-picked).

### Quick Look (`quicklook_darwin.go`)

- purego bridge: shared `QLPreviewPanel`, data source with the file's URL,
  `makeKeyAndOrderFront`-style presentation. Works on local paths and cached
  remote paths. Non-darwin builds: Space is a no-op (preview still available
  via double-click).
- **Spike first** (throwaway): prove panel ownership and key-event flow inside
  mygo's event loop before committing to the design; a fallback is shipping
  Space = in-kero preview only.

### Open With (context menu)

- "Open" (default app) via existing `mygo.Shell.OpenPath`.
- "Open in VSCode" via `open -a "Visual Studio Code" <path>`, falling back to
  the `code` CLI.
- "Choose App…" enumerates handlers via purego LaunchServices
  (`LSCopyApplicationURLsForURL`-family) with app names (+ icons where cheap);
  fallback if enumeration proves brittle: a curated list plus a browse of
  `/Applications`. On Windows/Linux, "Open in VSCode" goes through the `code`
  CLI and "Choose App…" is hidden (darwin-only); plain "Open" stays
  cross-platform via `mygo.Shell.OpenPath`.

### Cache and sync-back (`files_open.go` + daemon `fs.write`)

- Opening a **remote** file (any path: preview, file pane, Quick Look, Open
  With) first downloads it via `fs.read` into
  `proto.Dir()/files/<host-hash>/<path-hash>-<name>` and records
  `{local path → host connection, remote path, sha256 of content read}`.
  Local files use their real path; no cache, no sync.
- A poller (500ms) stats mapped local files by path (survives editor atomic
  saves that replace the inode). On change, it waits for 1s of quiet, then
  calls the new `fs.write` RPC `{Path, Content, Hash}` where `Hash` is the
  sha256 of the content last read/written. The daemon rejects with a conflict
  error when the remote file's current hash differs; the UI then offers
  overwrite or re-download — never silent overwrite.
- Success updates the recorded hash. Deleted local cache files drop the
  mapping. Cap at the `fs.read` limit (32MB).
- Old remote kerods answer `unknown method` to `fs.write`; surface the
  existing hostUpdate hint.

### Tests

- `internal/daemon`: `fs.write` round-trip, hash conflict, size cap.
- Root: cache mapping lifecycle, debounce timing (fake clock), conflict dialog
  flow via `ui.NewTester`.

## Feature C — compact tree sidebar

- Setting `sidebar.density`: `compact` (default) | `standard`, via the 5-place
  settings convention; `standard` renders exactly today's layout.
- `view.go` density-aware row metrics (26/24/12.5px vs 34/28/13.5px today) and
  the block-indent style in `groupView`/`sessionRow`: children of a group get a
  left 2px bar in `projectBadgeColor`, children inset within the block. Custom
  drawing follows the `statusIcon` precedent (`p.Line`/`p.Rect`).
- Folded groups, selection, ⌘1–9, drag affordances all unchanged.

## Build order

1. Cherry-pick `sidebar-sort` onto main; resolve; adapt tests.
2. Feature A on top of 1 (pins in daemon + `order.go` + drag/arrows).
3. Feature C (density setting + block indent) — small, independent of A's
   logic, same files as A's visuals.
4. Cherry-pick `feat/files-view` (Files tab + file-pane protocol bits); rebind
   its shortcut to `Cmd+Shift+F`; then Quick Look spike → implementation, Open
   With menu, cache/sync + `fs.write`.

Each step ships green (`go vet`, `go test ./internal/...`, `go test .`,
`go tool mygo vet .`).

## Risks

1. **QLPreviewPanel via purego** — the largest unknown (NSApplication/first
   responder interplay with mygo's loop). Spike before feature commitment;
   graceful fallback documented above.
2. **LaunchServices enumeration** — fallback path defined.
3. **Cherry-pick conflicts** from fork↔our-tree drift — per-commit picks,
   manual resolution, tests as the arbiter.
4. **Watcher edges** — atomic saves, deleted files, 32MB cap handled by
  path-stat polling + mapping lifecycle.
5. **Remote write conflicts** — hash lock + explicit dialog.
6. **Re-sort cost on full-state pushes** — stable O(n log n) recompute.

## Calls I made (defaults, change any)

- Debounce 1s; poll 500ms; density default `compact`; date-grouping ignores
  cross-group pins; a moved anchor carries its whole chain; `fs.write` capped
  at 32MB; hash = sha256 of content at read time; `"head"` as the pin-to-top
  sentinel.

## Left to the builder

- Exact purego binding set for QLPreviewPanel/LaunchServices (from the spike).
- Final settings key names and locale strings.
- Side panel tab order (Changes vs Files) and tab visuals.
- Icons for files/directories (mygo `FileIcon` availability permitting).
