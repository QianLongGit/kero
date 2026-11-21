# Data Transfer (Export / Import) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One JSON file carries a user's hosts, settings and agents.toml from one computer's Kero to another's — `kero export` / `kero import` on the CLI, a Settings › Transfer page in the app.

**Architecture:** `internal/settings/transfer.go` owns the format and the merge: Export collects from the live configuration, Import merges into it through the existing `format()`/`write()` path so comments survive, and both ends filter machine-local keys. The CLI commands are thin local commands mounted before `connect()`. The Settings page reuses the files view's `xferSave`/`xferPick` dialog seams and the download-by-path dialog's preview pattern (`dl_by_path.go`).

**Tech Stack:** Go; `internal/settings`, `internal/cli`, root-package MyGo UI; locales JSON catalogs regenerated with `go tool mygo generate`.

**Spec:** team-lead task message (approved design, 2026-11-21). Sessions live on remote hosts and are not migrated; local sessions are not exported; no proto changes, no network sync, no encryption.

## Global Constraints

- `TransferData` JSON fields exactly: `format` ("kero-data"), `version` (1), `exportedAt` (RFC3339), `exportedFrom` (GOOS), `appVersion` (omitempty, from `internal/build`), `hosts` (`[]Host`), `settings` (`map[string]string`, list values "\n"-joined — config values never contain newlines), `agentsToml` (omitempty).
- Local-key blacklist, filtered at BOTH ends: `files-scp-path`, `files-rsync-path`, `files-pinned-app`, `worktree-dir`, `new-session-directory`, `command`.
- Import semantics: hosts upsert by name (case-folded, `Settings.Host` precedent `internal/cli/remote.go:280-284`); settings applied key-by-key via `key.set`; non-empty `agentsToml` replaces `AgentsPath()`'s file; save through `settings.Save` so `format()` keeps comments and unknown lines.
- Import is all-or-nothing: any invalid host, unknown settings key, or value a key rejects → one error, nothing written.
- Export collects only keys that differ from `Default()` (the configuration file's own philosophy — never force the exporter's defaults on the target), and never the `host` key (the `hosts` field carries those).
- `locales/catalog_gen.go` only regenerated via `go tool mygo generate` after editing `en.json`/`ja.json`/`zh-Hans.json` together. English prose comments explaining why, matching existing files.
- Do not commit. Baseline `go test .` before touching root-package code; `TestDialWindows` flakiness is pre-existing.

## Review Focus

1. A hand-edited data file that re-inserts a blacklisted key must still not apply it (double filter) — pinned by TestImportFiltersLocalKeysAgain (Task 1).
2. A data file with an unknown settings key (a newer Kero wrote extra keys into version 1) must refuse the whole import with the configuration untouched — TestImportRefusesBadData (Task 1).
3. Import must keep the target file's comments and unknown lines — TestImportKeepsComments (Task 1).
4. A host differing only in name case ("Build" vs "build") must replace, not duplicate — TestImportHostsUpsert (Task 1).
5. Importing a file into the configuration it was exported from changes nothing (round-trip idempotence) — TestRoundTripIdempotent (Task 1).

---

### Task 1: settings transfer core

**Files:**
- Create: `internal/settings/transfer.go`
- Test: `internal/settings/transfer_test.go`

**Interfaces:**
- Produces (later tasks rely on exactly these):
  - `const TransferFormat = "kero-data"` / `const TransferVersion = 1`
  - `type TransferData struct` with the JSON tags above
  - `func Export() (td TransferData, skipped []string, err error)` — `skipped` names blacklist keys the user actually has set
  - `func ParseTransfer(b []byte) (TransferData, error)`
  - `type TransferSummary struct { HostsAdded, HostsReplaced int; Settings []string; Skipped []string; AgentsToml bool }`
  - `func PreviewImport(td TransferData) (TransferSummary, error)` — merge only
  - `func Import(td TransferData) (TransferSummary, error)` — merge, `Save`, agents.toml write

Implementation shape: `merge(td) (Settings, TransferSummary, []string)` does the work for both Preview and Import; hosts validated with `remote.ValidName` + `remote.ParseAddr` (address rewritten as parsed); settings keys sorted for deterministic problems; blacklist keys counted into `Skipped`.

- [ ] **Step 1: Write failing tests** in `internal/settings/transfer_test.go`. Helper `isolate(t)` = `t.TempDir()` + `t.Setenv("KERO_HOME", dir)` + `t.Setenv("KERO_CONFIG", filepath.Join(dir, "config"))`. Tests:
  - `TestExportFiltersLocalKeys` — config with `host = devbox=me@devbox`, `files-scp-path = /usr/local/bin/scp`, `command = /bin/zsh`, `theme = mist`, comment line: Export's `skipped` = [command files-scp-path] (sorted), `td.Settings` has `theme: "mist"`, no blacklisted keys, `td.Hosts` = the one host.
  - `TestExportSkipsDefaultsAndHostKey` — default-only config: `td.Settings` empty/nil, no `host`/`listen`-by-accident keys, `Format`/`Version`/`ExportedFrom`/`ExportedAt` filled, `AppVersion` non-empty in tests via build info.
  - `TestExportReadsAgentsToml` — agents.toml beside config → `AgentsToml` its text; absent → "".
  - `TestParseTransferRejects` — broken JSON; `format: "nope"`; `version: 2` → three distinct errors, each mentioning what's wrong.
  - `TestImportHostsUpsert` — target has `build` host; data has `BUILD` (new addr, color) + `new` host → replace not duplicate, counts 1/1.
  - `TestImportSettingsApplyAndDoubleFilter` — data's settings include `theme` and `files-scp-path` → theme applies, scp-path lands in `Skipped`, not applied.
  - `TestImportAgentsTomlReplaceAndKeep` — non-empty replaces the file; empty leaves an existing one.
  - `TestImportKeepsComments` — target config has `# my comment` + unknown key line; after Import both still in the file text.
  - `TestImportRefusesBadData` — three files: unknown settings key; `font-size = abc`; host `Addr` that `remote.ParseAddr` rejects → error each, file bytes unchanged.
  - `TestRoundTripIdempotent` — Export from config A, Import into A, file bytes equal (mtime aside).
- [ ] **Step 2: Run** `go test ./internal/settings/ -run 'Export|Import|ParseTransfer|RoundTrip' -v` — expect compile failure (no transfer.go).
- [ ] **Step 3: Implement** `internal/settings/transfer.go` per Interfaces; imports `kero/internal/build` (verified no cycle), `os`, `path/filepath`, `runtime`, `slices`, `sort`/`slices.Sort`, `strings`, `time`, `encoding/json`, `fmt`, `kero/internal/remote`.
- [ ] **Step 4: Run** same command — PASS. Then `go test ./internal/... && go vet ./...` — clean.

### Task 2: `kero export`

**Files:**
- Create: `internal/cli/export.go`
- Modify: `internal/cli/cli.go` (usage block line after `kero config …`, `Commands` at :184, `run()` local switch at :289), `internal/cli/remote.go:60` (`localOnly`)
- Test: `internal/cli/export_test.go`

**Interfaces:**
- Consumes: `settings.Export`, `settings.TransferFormat` (tests).
- Produces: `func exportCmd(args []string) error`.

Behavior: flags `{"o": "out", "output": "out"}`; extra positional → `usageErr`. Default name `kero-data-YYYYMMDD.json` in cwd. `-o -` writes the JSON to stdout. Else `os.MkdirAll(filepath.Dir(p))` + `os.WriteFile(p, b, 0o644)`. Marshal with `json.MarshalIndent(td, "", "  ")` + trailing newline. Success summary on **stderr**: hosts count, settings count, agents.toml present/absent, skipped local keys count (+ names).

- [ ] **Step 1: Failing tests** `internal/cli/export_test.go` (env-isolated like Task 1): `TestExportCmdWritesFile` (config with a host + theme + `files-scp-path` → `-o path` → file parses via `settings.ParseTransfer`, blacklist absent, host present), `TestExportCmdStdoutDash` (capture os.Stdout via pipe; `-o -`), `TestExportCmdUsage` (positional arg → error wrapping errUsage).
- [ ] **Step 2: Run** — fail (exportCmd undefined).
- [ ] **Step 3: Implement** export.go + wiring (usage line, Commands += "export", run case `case "export": return exportCmd(args)`, localOnly += "export": true). remote_test.go's localOnly assertions unaffected.
- [ ] **Step 4: Run** `go test ./internal/cli/ -run Export -v` — PASS; `go vet ./...` clean.

### Task 3: `kero import`

**Files:**
- Create: `internal/cli/import.go`
- Modify: same wiring points as Task 2 (`cli.go` usage/Commands/run, `remote.go` localOnly)
- Test: `internal/cli/import_test.go`

**Interfaces:**
- Consumes: `settings.ParseTransfer`, `settings.PreviewImport`, `settings.Import`.
- Produces: `func importCmd(args []string) error`.

Behavior: boolean flags `{"y": "yes", "yes": "yes"}`; exactly one positional else `usageErr("kero import <file> [--yes]")`. Read file → ParseTransfer → PreviewImport → print preview (exported-from line with appVersion; `will merge N hosts: A new, R replaced`; `will apply M settings` + `(skipping K local settings: …)` when K>0; agents.toml replaced/unchanged). Confirm by `[y/N]` on stdin unless `--yes` or stdin is not a TTY (`term.IsTerminal`); non-y → print "cancelled", return nil. Then `settings.Import` → print result line. Any error from parse/preview/import returns as error (exit 1); the configuration is untouched (Task 1's all-or-nothing).

- [ ] **Step 1: Failing tests** `internal/cli/import_test.go`: `TestImportCmdMerges` (target config: host `a` + `# comment`; data file: host `a` new addr + host `b` + theme + hand-added `files-rsync-path`; run `importCmd([]string{file})` — stdin not a TTY under `go test`, so no prompt — merged hosts/theme, rsync-path untouched, comment kept), `TestImportCmdBadFile` (garbage file → error, config unchanged), `TestImportCmdFutureVersion` (version 2 → error naming the version), `TestImportCmdUsage` (0 positionals → errUsage).
- [ ] **Step 2: Run** — fail. **Step 3: Implement** import.go + wiring (+= "import"). **Step 4:** `go test ./internal/cli/ -run Import -v` PASS; `go test ./internal/...` green.

### Task 4: Settings › Transfer page (GUI + locales)

**Files:**
- Create: `transfer.go`, `transfer_test.go` (root package)
- Modify: `settings_view.go` (settingsPages entry after `hosts` at :53; page switch at :315; dialog render beside `confirmDialog` at :340), `settings_search.go` (two index entries), `locales/en.json` + `ja.json` + `zh-Hans.json`; regenerate `locales/catalog_gen.go`

**Interfaces:**
- Consumes: Task 1's API; existing seams `a.xferSave func(defaultPath string) (string, error)` and `a.xferPick func(dir, multiple bool) ([]string, error)` (app.go:213-215 — no app.go changes); `a.apply(settings.Load(), false)` for post-import refresh (apply already calls `syncHosts()` and invalidates); `a.update`; `a.ss.toast` + `c.Toast`.
- Produces: `type importPreview struct { open bool; data settings.TransferData; sum settings.TransferSummary; err string; busy bool; gen int }`; settingsState field `xfer importPreview`; `func (a *App) transferPage(c *ui.Context, pal *palette)`; `func (a *App) transferExport()`; `func (a *App) transferImportPick()`; `func (a *App) transferImportView(c *ui.Context, pal *palette)`; `func (a *App) transferImportApply()`.

Locale keys (all three files, camelCase, args as in existing keys): `pageTransfer`, `transferExport`/`transferExportDetail`/`transferExportButton`, `transferImport`/`transferImportDetail`/`transferImportButton`, `transferNote` (group note: sessions live on their hosts; addresses in plain text; local settings stay), `transferImportTitle`, `transferImportFrom(from, version)`, `transferImportHosts(n, added, replaced)`, `transferImportSettings(n)`, `transferImportSkipped(k, names)`, `transferAgentsReplace`, `transferAgentsKeep`, `transferExported(path)`, `transferImported(hosts, settings)`.

Page rows in one `settingsGroup` (title `locales.PageTransfer()`, note `locales.TransferNote()`): Export row → `a.transferExport()`; Import row (primary button) → `a.transferImportPick()`. Export: `go func { p, err := a.xferSave("kero-data-" + time.Now().Format("20060102") + ".json"); … write marshal … }` with `a.update` toasting `TransferExported(p)` or the error. Pick: `go func { paths, err := a.xferPick(false, false); read; ParseTransfer; PreviewImport; }` then `a.update` opens `importPreview{open: true, data: td, sum: sum, gen: old.gen + 1}` (whole-struct reset, gen monotonic — dl_by_path pattern); a parse/preview error toasts instead of opening. View: `ui.DialogBase` + `floating(panel.Width(480)…)` exactly as `dlByPathView`; Cancel (esc) closes; Import button plain (NOT defaultButton — focus rules), `ui.Spinner` while `busy`; apply errors show in red (`pal.danger`) with the dialog open. `transferImportApply`: gen-guarded goroutine → `settings.Import` → `a.update`: close dialog, `a.apply(settings.Load(), false)`, toast `TransferImported`. Search entries: `{"transfer", locales.TransferExport(), "export backup copy move migrate data another computer windows mac kero-data"}` and `{"transfer", locales.TransferImport(), "import merge restore kero-data json data file"}`.

- [ ] **Step 1: Baseline** `go test . -count=1` (record failures, if any, before any root change).
- [ ] **Step 2: Failing tests** `transfer_test.go` using `testApp(t)` + `a.updateFn = func(fn func()) { fn() }` seam for goroutine marshalling:
  - `TestTransferExportWritesFile` — fake `a.xferSave` → tmp path; config with a host; `a.transferExport()`; wait (poll) for file; `settings.ParseTransfer` round-trip; `a.ss.toast` = `TransferExported`.
  - `TestTransferImportPreviewAndApply` — fake `a.xferPick` → data file (host replace + new + theme + blacklisted key); `a.transferImportPick()`; wait `a.ss.xfer.open`; `ui.NewTester(a.settingsView, 820, 620)` (setWin nil is safe — `windowControls` nil-guards) with `a.ss.page = "transfer"`; `tt.Frame()`; `tt.HasText` the preview lines; click Import by label; wait: `settings.Load().Hosts` merged, `a.set` refreshed, dialog closed, toast set.
  - `TestTransferImportBadFileToasts` — fake pick → garbage file; wait toast contains "not a Kero data file"; `!a.ss.xfer.open`.
- [ ] **Step 3: Run** — fail (no transfer.go).
- [ ] **Step 4: Implement** locales (3 JSON files) → `go tool mygo generate` → verify `git diff locales/catalog_gen.go` additions only → transfer.go + settings_view.go/settings_search.go wiring.
- [ ] **Step 5: Run** `go test . -run Transfer -v` PASS; `go test . -count=1` no NEW failures vs Step 1; `go tool mygo vet .` clean.

### Task 5: Full verification + CLI round-trip

- [ ] `go vet ./...` clean; `go test ./internal/...` all green; `go test .` vs baseline; `go tool mygo vet .` clean.
- [ ] Manual round-trip script (no repo commit): temp `KERO_HOME`/`KERO_CONFIG` under `/tmp` quota care — use `$(mktemp -d)`; build `go build -o /tmp/…/kero .` once; scenario: config A (host devbox, theme, files-scp-path set, agents.toml, comment) → `export -o` → inspect JSON → mutate config (different host, different theme) → `import <file>` (piped "y") → verify merged hosts/settings/comment via `kero config show` equivalent (grep config file) → export again → re-import → idempotent. Capture output for the delivery report.
- [ ] Delivery report to team-lead: file list with signatures, test output excerpts, round-trip transcript, open items (unknown-key policy = refuse; `listen` transfers as-is per approved blacklist).
