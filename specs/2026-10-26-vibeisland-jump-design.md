# Vibe Island Jump Rules for Kero — Design

Date: 2026-10-26
Status: approved approach (A), pending implementation

## Intent and why

Vibe Island shows AI coding agent sessions in the notch; clicking one jumps to
the terminal running it. Kero is not on Vibe Island's native list, so today the
click only activates Kero's window. Vibe Island's custom jump rules let any app
receive exact jump hints (session id, TTY, cwd) through a URL scheme, so the
jump can land on the exact pane. The deliverable is a build Kero DMG the user
installs directly, with the rule setup available from Kero's settings.

## Goals

- Clicking a session in Vibe Island's notch panel brings Kero frontmost with
  the exact session and pane focused.
- A one-click setup in Kero's settings writes the rule file; nothing else for
  the user to do but restart Vibe Island.
- Shipped as a signed, notarized DMG build.

## Non-goals / anti-goals

- Never open a new pane or guess a near one when nothing matches ("If no target
  matches, just activate the app or do nothing" — Vibe Island's own words);
  no-match falls back to plain window activation, exactly today's behavior.
- No tmux support: kero panes are not tmux panes; `tmuxPane` is ignored.
- No pid matching: `Pane.PID` is the shell's pid, Vibe Island's `{pid}` is the
  agent CLI's; matching them would need process-tree walking that tty, session
  and cwd make pointless.
- No Windows/Linux URL work beyond what mygo already does (Vibe Island is
  macOS-only).

## Constraints (Vibe Island contract, verified from docs)

- Rule file: `~/.vibe-island/integrations/<name>.json`, loaded at Vibe Island
  startup only. Vibe Island 1.0.22+, not the Mac App Store edition.
- Fields: `displayName` (optional), `bundleIdentifier` (required, matching
  key), `jumpRule.method` must be `"urlScheme"`, `jumpRule.template`.
- URL: scheme of our choosing, host must be `focus`; variables land as
  URL-encoded query values; empty variables resolve to empty strings.
- Fixed variable set: `{session_id}`, `{raw_session_id}`, `{cwd}`, `{tty}`,
  `{pid}`, `{bundle_id}`, `{tmux_pane}`.
- Matching is by full bundle identifier: `sh.kero` (release),
  `sh.kero.dev` (mygo dev build).
- An explicit rule overrides Vibe Island's built-in Ghostty-kernel detection —
  exactly kero's case (libghostty-vt).
- Known issue on their side: URL schemes only fire for installed, signed apps;
  with multiple copies installed macOS resolves the scheme to whichever
  registered last (dev build caveat below).

## Approaches considered

- **A (chosen): URL scheme + `Pane.TTY`.** Register `kero://`, handle it in the
  GUI, and add the pane's pty slave path to the pane state. Pane-exact
  matching that depends on no user setup; tty is Vibe Island's own "best
  general-purpose key". Cost: one optional proto field, one assignment in the
  daemon.
- **B: URL scheme, zero proto change.** Match by `AgentSession.ID` (needs the
  `kero skill setup` hook) with cwd fallback. Rejected: session-level only,
  multi-pane sessions and same-cwd agents resolve wrong or coarse, and
  exactness depends on the hook being installed.
- **C: shell command via `kero focus`.** Rejected: jump rules execute URLs
  only; `method` has exactly one value, `urlScheme`.

## The build

### 1. Bundle — mygo.json

Add top-level `"urlSchemes": ["kero"]`. mygo writes `CFBundleURLTypes` into
the app bundle's Info.plist; Launch Services registers it on install.

### 2. proto — internal/proto/proto.go

`Pane` gains `TTY string \`json:"tty,omitempty"\``: the pane's terminal device
(`/dev/ttys012`), set at each (re)start of the pane's pty — the assignment
lives in `session.go` `start()`, beside `s.info.PID` (session.go:169), which is
itself overwritten on every start — and stable while the pty lives. A restored
pane gets the new device's name. Additive optional field, so no
`proto.Version` bump (repo rule). Empty on Windows.

### 3. daemon — internal/daemon

- `pty_unix.go`: `ptyProc` carries `tty string`, set in `startPTY` from
  `openMaster()`'s slave name.
- `pty_windows.go`: field stays empty.
- `session.go` `start()`: `s.info.TTY = proc.tty` beside `s.info.PID =
  s.shellPID` (session.go:169).

### 4. GUI handler — new root-package files `jump.go` + `jump_test.go`

- `main.go` registers `mygo.App.OnOpenURL(a.openJump)` before `Run` (pre-Run
  registration is required to receive the URL the app was launched with).
- Accept only scheme `kero` (case-insensitive) and host `focus`
  (case-insensitive too); read query `session`, `tty`, `cwd` (`pid`,
  `tmuxPane` ignored). An empty parameter counts as absent: skip that
  matching level.
- Resolution is a pure function over the merged `*proto.State` plus the set
  of local pane ids. Host ownership is an App-level fact (`App.owner`,
  app.go:88-93; local means the host's name is empty, hosts.go:31), which a
  bare `*proto.State` cannot express. Vibe Island's documented order:
  1. **session**: a pane whose `AgentSession.ID == session` (any host; ids are
     globally unique strings).
  2. **tty**: a local pane whose `TTY == tty` (exact; a remote host's ttys
     are different devices).
  3. **cwd**: panes whose `WorkDir() == cwd` (exact bytes — both sides read
     the path from the same OS). If several match, prefer local, then the
     most recent `Activity`. (My call: a deterministic tie-break among exact
     matches, not a guess.)
  4. none → no target.
- Wiring: target found → `selectSession(session)`, then `setFocus(s, pane)`
  unconditionally — it is idempotent (panes.go:68) and keeps the app's local
  view and kerod's in step — then `showWindow()`. No target → `showWindow()`
  only.
- The jump handler body must run inside `a.update()`, like
  `OnNotificationClick` (app.go:350-353): mygo callbacks arrive on the app
  thread, and root-package UI state is only safe there.
- Cold start: `setState` fires per host (app.go:298) and the URL can arrive
  before any of them. Stash the URL when `a.loaded` is false (app.go:94) and
  resolve it against every incoming state: on a hit, consume and jump; once
  the local host's state has arrived without a hit, consume as no-target and
  still `showWindow()` (same as the no-target behavior; cold-start jumps are
  local by nature — Vibe Island runs beside Kero). The warm path resolves
  immediately through the same pure function.

### 5. Settings — Vibe Island setup row

Follows the "Command Line Tool" group/row/button pattern and the skills rows'
status + remove buttons, both on the Agents settings page
(settings_view.go:988-1028). Which page the new row lands on is the builder's
call; the Agents page (CLI tool, skills) is the natural neighbor.

- Row detail shows the rule file state: installed at
  `~/.vibe-island/integrations/kero.json` (or `kero-dev.json`) or not
  installed.
- Install/Reinstall button: `mkdir -p ~/.vibe-island/integrations`, write

  ```json
  {
    "displayName": "<mygo.App.Name()>",
    "bundleIdentifier": "sh.kero" | "sh.kero.dev" (mygo.IsDev()),
    "jumpRule": {
      "method": "urlScheme",
      "template": "kero://focus?session={session_id}&tty={tty}&cwd={cwd}"
    }
  }
  ```

- Remove button when installed (skills row pattern).
- Group detail mentions: Vibe Island 1.0.22+, restart Vibe Island to load the
  rule.
- Strings in locales en/ja/zh-Hans + `settings_search.go` keywords;
  `locales/catalog_gen.go` regenerated with `mygo generate`.
- The file lives under the real home (`~/.vibe-island`), not `KERO_HOME`;
  the path/content logic is a helper taking a home dir so tests can use a
  temp dir.

### 6. Verification and delivery

- Gates: `go vet ./...`, `go test ./internal/...`, `go test .` (local, first
  run downloads libghostty-vt), and `go tool mygo vet .` for the root-package
  UI changes (stored build values, goroutine captures).
- Manual: `open "kero://focus?cwd=/some/dir"` from a terminal; with Vibe
  Island installed, click a notch session.
- DMG: `go tool mygo build -platform darwin/arm64` — signed + notarized by the
  existing mygo.json config; artifact lands in `build/`. Version stays 2.1.21
  (a local build for direct install; no tag, no CHANGELOG). [my call]

## Tests

- `internal/daemon`: a started session's pane reports a non-empty `TTY` with
  `/dev/ttys` prefix (unix; skipped on Windows).
- `jump_test.go`: resolver table — session hit on any host; tty hits local
  only (remote tty ignored); cwd prefers local then latest Activity; empty
  `session=`/`tty=`/`cwd=` skips its level (an empty cwd never matches
  dir-less panes); no match → nil; prefix mismatches don't hit. URL parsing:
  %-encoded values, empty params, wrong host (`Focus`, `run`), wrong scheme.
  Pending-jump: consumed on a hit from the first arriving state; consumed as
  no-target once the local state arrived without a hit.
- Rule-file helper: write/read/status against a temp home.

## Left to the builder

- Final copy for the settings row; locale wording in ja (terse, existing tone).
- Confirm the DMG artifact name/layout from `mygo build` output.

## Known caveats

- Dev + installed builds both claim `kero://`; macOS hands the URL to whichever
  registered last. While testing with `mygo dev`, quit the installed Kero or
  expect ambiguity. Production users only ever have one.
- The scheme fires only for the installed, signed app — the DMG is fine;
  `go run` builds won't respond to URLs.
