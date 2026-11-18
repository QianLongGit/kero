# Files Transfer Method (builtin / scp / rsync) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A global setting picks how the Files view moves files with a remote host — Kero's own transfer stream (default), or a locally spawned `scp`/`rsync` line that bypasses kerod.

**Architecture:** One `Settings` field drives a single predicate at the two flow entrances (`downloadFrom`/`uploadTo`); when it holds, new root-package flows in `files_transfer_ext.go` spawn `scp`/`rsync` through an injectable process seam and reuse the existing one-slot `transferState`, ✕-cancel, failure toast and relist machinery unchanged. Command lines are pure functions over `remote.Addr`; an ssh probe per host (cached) gates rsync.

**Tech Stack:** Go stdlib (`os/exec`, `bufio`, `strings`); mygo `ui` (one segmented row); locales via `go tool mygo generate`.

**Spec:** this plan is self-contained — the adjudicated decisions below are the spec; there is no separate design doc. Executors read this file only.

## Adjudicated decisions (verbatim from the user — do not relitigate)

1. Settings gains a global `FilesTransfer`: `"builtin"` (default) / `"scp"` / `"rsync"`, as a third-choice segmented row in Settings › Files. The transfer UI itself — menus, pickers, Save dialog, toasts — changes not at all.
2. `scp`/`rsync` modes spawn a process locally that connects to the host directly, bypassing kerod and `remote.Conn`; the command line is assembled in the GUI from the host's `remote.Addr`.
3. Ext paths apply only when `addr.Scheme == "ssh"` and the remote is not Windows; everything else silently falls back to the built-in stream, the fallback decided in exactly one place.
4. rsync mode: before each transfer, probe the remote over ssh with `rsync --version`, cached per host in-process. A failed probe aborts this transfer with a dedicated toast — no fallback.
5. `scp`/`rsync` uploads overwrite same-named remote files outright (no `dedupName` check); downloads still take the local `pasteName` dedup for the destination name.
6. scp downloads land in a local temp (`.kero-dl-*`) then rename, keeping the atomic semantics; the temp's failure paths all `os.Remove` it under defer. scp writes no machine-readable progress: an upload's `x.total` is the local files' stat sum, set before the run (the built-in's own move, files_transfer.go:540-548) with `done` still, a download's total is unknowable → `total=0` spinner (xferArc already spins). rsync parses `--info=progress2` frames from **stdout** (`\r`-framed, emitted without a tty too; stderr carries only errors) for real progress — its percent backs out a **download's** total (`done*100/pct`), while an upload's is the stat sum.
7. Reuse the single slot `a.side.files.xfer`, the ✕ (now bound to Kill), `errXferCancelled`, the `xferFailed` toast, and the post-transfer `relistDir`.
8. Directory downloads align with the built-in `--exclude=.git` where the tool can: rsync excludes `.git`; scp cannot exclude anything — that difference is documented in code, no workaround.
9. scp argv carries no `--` (traditional scp does not know it; local paths come from pickers and never start with `-`), and every scp **remote** path is shell-quoted — the `path` of `host:path` is re-read by the remote shell, so a space breaks it — wrapped in double quotes with `\`, `"`, `$` escaped inside: `user@host:"/path with space/"`. Local paths ride argv untouched; rsync formats its own remote side.
10. Two ways a chosen external transfer does not run, two answers: the environment's word — scheme not ssh, or a Windows remote — falls back to the built-in stream silently, no toast; the tool's own absence — scp/rsync not on this machine (`exec.LookPath` misses), rsync not on the host (probe fails) — ends the transfer where it stands (`xferFailed`, or the rsync toast), never falls back. The user chose the tool; a silent switch of method would hide the problem.
11. rsync's remote side is protected not by quoting but by `--protect-args` (`-s`) on every rsync argv: the remote spec rides a shell line rsync itself re-splits, and `-s` carries the arguments across that hop untouched, end to end — spaces included. rsync 3.0+ (2008) has it, and the probe has already answered for rsync being there at all. The probe argv carries no paths and needs no `-s`.

## Global Constraints

- English comments explaining why, matching neighboring files; minimal diff — the built-in path's behavior untouched. `internal/` sees exactly one change: the Settings field of Task 1, a need of the feature itself. Nothing is exported or edited in `internal/remote` for the transfer — that shifts the kerod build fingerprint (CLAUDE.md) — so the six never-ask `-o` options are copied into `files_transfer_ext.go` as a private constant, not shared.
- One feature per root-package file: new code goes in `files_transfer_ext.go` + `files_transfer_ext_test.go` (this feature's file; `files_transfer.go` only gains the two dispatch lines, the `dirDestName` extraction and the `downloadViaTemp` helper).
- `locales/catalog_gen.go` is generated by `go tool mygo generate` only, never hand-edited; `en.json`, `zh-Hans.json`, `ja.json` change together.
- Root tests isolate with `t.Setenv("KERO_HOME", t.TempDir())` (+ temp `KERO_CONFIG`) via `testApp`, per repo convention; root tests run locally (`go test .`), CI runs `go vet ./...` + `go test ./internal/...`.
- Command assembly is pure functions, unit-tested without spawning; the spawn layer is an App function-field seam, tested with fake processes.
- No over-engineering: no `KERO_SCP`/`KERO_RSYNC` env overrides, no ControlMaster, no scp `-O`, no bandwidth flags, no rsync version parsing beyond "the probe exits 0".
- **No git commits or branches** — the user commits manually; every "Commit" step below is the listed verification commands instead.

## Review Focus

1. **Non-ssh or Windows remote with scp/rsync chosen** — a `wsl`/`http` scheme or a Windows ssh host must silently run the built-in stream, not error and not spawn. Pinned in Task 4 (`TestXferExtApplies`, `TestXferExtFallsBackSilently`).
2. **rsync missing on the remote** — the dedicated "no rsync on {host}" toast aborts that transfer; never a fallback, never the generic failed toast. Pinned in Task 5 (`TestRsyncProbeAbsentAborts`).
3. **✕ vs a running process** — Kill must land as `errXferCancelled` (cancelled toast) even though Wait then reports a signal error; and Wait must not race the cancel's error marking. Pinned in Task 4 (`TestXferExtCancelKillsProc`).
4. **Upload overwrite, not dedup** — an ext upload's remote target is exactly `dir`, never a `name copy` variant and never a pre-check call. Pinned in Task 3 (`TestScpUploadArgs` — no " copy", space-quoted dir) and Task 4 (`TestScpExtUploadOverwrites`).
5. **rsync progress frames that don't parse** — comma-grouped numbers, a `(xfr#…)` tail on the last frame, a first frame with no percent yet: every unparseable frame is skipped, never an error; and frames arrive on **stdout** — a reader wired to stderr would pass no real transfer at all. Pinned in Task 5 (`TestParseRsyncProgress`, `TestRsyncProgressDrivesArc`).

---

### Task 1: The `FilesTransfer` setting — field, default, normalize

**Files:**
- Modify: `internal/settings/settings.go:39` (field, beside `FileOpen`), `:175` (Default), `:475` (normalize)
- Test: `internal/settings/config_test.go`

**Interfaces:**
- Produces (verbatim — every later task reads this through `a.set.FilesTransfer`):

```go
// what the Files view moves files with a remote host by: "builtin" its
// own transfer stream, "scp" or "rsync" a line of its own
FilesTransfer string `json:"filesTransfer,omitempty"`
```

Default value `"builtin"`; normalize `oneOf(&s.FilesTransfer, d.FilesTransfer, "builtin", "scp", "rsync")` (same line style as the FileOpen one at settings.go:475).

- [ ] **Step 1: Write the failing test** in `internal/settings/config_test.go`, beside `TestHostUpdateHintsOptIn`:

```go
// The way the Files view transfers is the built-in stream until the user
// picks scp or rsync; anything else keeps the default.
func TestFilesTransferSetting(t *testing.T) {
	if Default().FilesTransfer != "builtin" {
		t.Fatal("files transfer is not builtin by default")
	}
	s, probs := parse("files-transfer = rsync\n")
	if s.FilesTransfer != "rsync" || len(probs) != 0 {
		t.Errorf("files-transfer = rsync: %v, problems %q", s.FilesTransfer, probs)
	}
	if s, _ = parse("files-transfer = ftp\n"); s.FilesTransfer != "builtin" {
		t.Errorf("files-transfer = ftp kept %q", s.FilesTransfer)
	}
}
```

(The TOML key spelling is whatever the file's existing mapping produces — check how `fileOpen` maps before writing the literal; the JSON tag above is the source of truth.)

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/settings/ -run TestFilesTransferSetting` → FAIL (no such field).
- [ ] **Step 3: Implement** — the field beside `FileOpen` (settings.go:39 area), `"builtin"` in `Default()` beside `FileOpen: "pane"` (settings.go:175 area), the `oneOf` line beside the FileOpen one (settings.go:475 area). Comment style: one line of why, as the neighbours have.
- [ ] **Step 4: Run to verify it passes** — `go test ./internal/settings/`.
- [ ] **Step 5: Verify** — `go vet ./internal/settings/ && go build ./...`.

### Task 2: Locales ×3, the Settings row, search

**Files:**
- Modify: `locales/en.json`, `locales/zh-Hans.json`, `locales/ja.json` (beside the `setFileOpen*` block, ~en.json:420, and the `filesTransfer*` block, ~en.json:713)
- Regenerate: `locales/catalog_gen.go` via `go tool mygo generate`
- Modify: `settings_view.go:595-600` (the Files group), `settings_search.go:39` (index entry)
- Test: `settings_test.go` (beside `TestFileOpenSetting`, settings_test.go:119)

**Interfaces:**
- Produces (locale functions, generated): `locales.SetFilesTransfer()`, `locales.SetFilesTransferDetail()`, `locales.SetTransferBuiltin()`, `locales.SetTransferScp()`, `locales.SetTransferRsync()`, `locales.FilesRsyncAbsent(host string) string`.
- Keys: `setFilesTransfer`, `setFilesTransferDetail`, `setTransferBuiltin`, `setTransferScp`, `setTransferRsync`, `filesRsyncAbsent` (with an `@filesRsyncAbsent` `{"placeholders": {"host": {"type": "String"}}}` block, as `filesUploadToast` has at en.json:722-728).

- [ ] **Step 1: Add the six keys to all three catalogs.** English copy (the other two translate it):

```
"setFilesTransfer": "Transfer files with",
"setFilesTransferDetail": "What the Files view moves files to and from a remote host by. scp and rsync connect from this machine, past the host's Kero; a host without the chosen tool falls back to Kero's own transfer.",
"setTransferBuiltin": "Kero",
"setTransferScp": "scp",
"setTransferRsync": "rsync",
"filesRsyncAbsent": "No rsync on {host}: install it there, or transfer another way",
```

- [ ] **Step 2: Regenerate** — `go tool mygo generate`; `git diff locales/catalog_gen.go` shows only additions.
- [ ] **Step 3: Add the row** in the Files group (settings_view.go:595-600), after the file-open row, same `settingRow`+`segmented` shape as `NewSessionDir`'s three-way row (settings_view.go:527-529):

```go
settingRow(c, pal, locales.SetFilesTransfer(), locales.SetFilesTransferDetail(), func() {
	segmented(c, "files-transfer", &s.FilesTransfer, []string{"builtin", "scp", "rsync"},
		[]string{locales.SetTransferBuiltin(), locales.SetTransferScp(), locales.SetTransferRsync()})
})
```

Persistence needs no code: the row writes `s.FilesTransfer`, the existing watchConfig → applySettings → `settings.Save` path (app.go:1132-1140, app.go:1300) saves it.
- [ ] **Step 4: Add the search entry** after the file-open one (settings_search.go:39):

```go
{"general", locales.SetFilesTransfer(), "files-transfer scp rsync builtin copy move upload download ssh"},
```

- [ ] **Step 5: Write the failing UI test** in `settings_test.go`, modeled on `TestFileOpenSetting` (same tall tester, clicks the "rsync" segment, asserts `a.set.FilesTransfer` and `settings.Load().FilesTransfer` both read `"rsync"` after 3 frames). Name: `TestFilesTransferChoice`.
- [ ] **Step 6: Run** — `go test . -run TestFilesTransferChoice` → PASS (it exercises the row added in Step 3; write the test first if you prefer red-green, but the row is the only code this task has).
- [ ] **Step 7: Verify** — `go build ./... && go vet .`.

### Task 3: Command assembly — pure functions in `files_transfer_ext.go`

**Files:**
- Create: `files_transfer_ext.go` (root package; header comment explains the why: a transfer mode that borrows ssh's own tools instead of the daemon's stream)
- Test: `files_transfer_ext_test.go`

**Interfaces:**
- Consumes: `remote.Addr{Scheme, User, Host, Port, Kero}` (internal/remote/addr.go:31-39) and `remote.SSHCommand()` (transport.go:220-225 — both already exported; nothing else is taken from `internal/remote`, and nothing is added to it).
- Produces (verbatim — Task 4/5 call these; every function returns the argv **after** the program name):

```go
// xferSSHOpts are the never-ask options the bridge's own ssh line carries
// (transport.go's sshArgs), copied here rather than shared: opening
// internal/remote for the transfer would shift kerod's build fingerprint.
// scp and rsync carry the same six, none dropped — a transfer must not
// ask and must notice a dead link exactly as the bridge does. If
// transport.go's list ever changes, this follows by hand.
var xferSSHOpts = []string{"BatchMode=yes", "ConnectTimeout=15",
	"ServerAliveInterval=10", "ServerAliveCountMax=3",
	"ExitOnForwardFailure=yes", "ClearAllForwardings=yes"}

// xferSSHArgs — xferSSHOpts as -o pairs, then ["-p",port] when a.Port
// is set and ["-l",user] when a.User is, mirroring sshArgs's own
// conditionals (transport.go:210-215).
func xferSSHArgs(a remote.Addr) []string

// quoteRemotePath shell-quotes the path of a scp host:path side: the
// remote shell re-reads it — a space breaks it — so it goes out wrapped
// in double quotes with \, " and $ escaped inside. Local paths ride argv
// untouched (a picker's path never starts with "-", and scp takes no --);
// rsync formats its own remote side and is not quoted here.
func quoteRemotePath(p string) string

// scpUploadArgs — scp [-o opts] [-P port] file… [user@]host:"dir"
// (scp passes -o through to its ssh; its port is -P, scp's -p keeps
// times; the user rides the target, scp has no -l user; the dir is
// quoteRemotePath'd).
func scpUploadArgs(a remote.Addr, files []string, dir string) []string

// scpDownloadArgs — scp [-r when a folder] [-o opts] [-P port] [user@]host:"path" dest.
// No .git exclusion: scp has none; the built-in tar's skip (daemon/transfer.go:210-212)
// is a difference of the mode, documented here, not worked around.
func scpDownloadArgs(a remote.Addr, path string, isDir bool, dest string) []string

// rsyncUploadArgs — rsync -a -s --info=progress2 -e "ssh …" file… [user@]host:dir
// -a overwrites in place: no dedup, as scp (the built-in upload's dedupName is
// the daemon's own word, daemon/transfer.go:275, which no scp/rsync line passes
// through). -s (--protect-args) carries the arguments past the remote shell's
// re-reading, spaces in the dir included (decision 11).
func rsyncUploadArgs(a remote.Addr, files []string, dir string) []string

// rsyncDownloadArgs — rsync -a -s --exclude=.git --info=progress2 -e "ssh …" [user@]host:path dest;
// --exclude=.git aligns a folder download with the built-in tar's skip.
func rsyncDownloadArgs(a remote.Addr, path string, isDir bool, dest string) []string

// rsyncProbeArgs — for SSHCommand(): the ssh line that runs "rsync --version"
// on the host, BatchMode keeping it a question no one has to answer.
func rsyncProbeArgs(a remote.Addr) []string
```

Design values the tests must pin:
- `xferSSHArgs`: the six `-o` pairs in the order above, then `-p`/`-l` conditionals; every value space-free — rsync word-splits its `-e` string itself, so a space in any option would break the line (`TestXferSSHOpts` pins the constant's exact contents as the transport.go snapshot).
- rsync's `-e` value is the single string `"ssh " + strings.Join(xferSSHArgs(a), " ")`.
- scp targets are `userHost + ":" + quoteRemotePath(path)` with `userHost = a.User + "@" + a.Host` when User is set, else `a.Host` — the same alias-tolerant form ssh itself takes; rsync targets are `userHost + ":" + path`, unquoted — `-s` guards the hop (decision 11), not quotes.
- `rsyncProbeArgs` appends `"--", a.Host, "rsync --version"` after `xferSSHArgs(a)`.
- Local paths pass through verbatim (no quoting — argv, not a shell).

- [ ] **Step 1: Write the failing tests** in `files_transfer_ext_test.go` — table-driven over `remote.Addr` (bare `{Scheme:"ssh",Host:"srv"}`, `{User:"me",Host:"srv"}`, `{User:"me",Host:"srv",Port:"2222"}`), asserting the exact argv:

```go
func TestScpUploadArgs(t *testing.T) {
	got := scpUploadArgs(remote.Addr{User: "me", Host: "srv", Port: "2222"},
		[]string{"/tmp/a.txt", "/tmp/b.txt"}, "/srv/up")
	want := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=10",
		"-o", "ServerAliveCountMax=3", "-o", "ExitOnForwardFailure=yes", "-o", "ClearAllForwardings=yes",
		"-P", "2222", "/tmp/a.txt", "/tmp/b.txt", `me@srv:"/srv/up"`}
	// reflect.DeepEqual, t.Fatalf the diff
}
```

Plus: `TestXferSSHArgs` (port/user omission), `TestXferSSHOpts` (the constant is exactly the six values above, in order — the copied snapshot), `TestQuoteRemotePath` (`/a b/c` → `"/a b/c"`; `a\b c"d$e` → `"a\\b c\"d\$e"`; empty stays empty), `TestScpUploadArgs` also covers a dir with a space (`/srv/my files` → tail `me@srv:"/srv/my files"`, and no " copy" anywhere — the overwrite pin), `TestScpDownloadArgs` (`-r` present iff isDir, remote side quoted, dest last, verbatim), `TestRsyncUploadArgs` (`-a`, `-s`, `--info=progress2`, `-e "ssh -o BatchMode=yes … -p 2222 -l me"`, files then `me@srv:/srv/my files` — a spaced dir, unquoted, carried by `-s`), `TestRsyncDownloadArgs` (`-s`; `--exclude=.git` present iff isDir; a spaced remote path in the dir case), `TestRsyncProbeArgs` (`--`, host, `rsync --version` at the tail; no `-s` — no path rides it).
- [ ] **Step 2: Run to verify they fail** — `go test . -run 'TestScp|TestRsync|TestXfer|TestQuote'` → FAIL (undefined).
- [ ] **Step 3: Implement** the functions and the constant in `files_transfer_ext.go`. One shared unexported helper for the `-o`-pair expansion is fine; nothing else shares.
- [ ] **Step 4: Run** — `go test . -run 'TestScp|TestRsync|TestXfer|TestQuote'` → PASS.
- [ ] **Step 5: Verify** — `go vet . && go build ./...`.

### Task 4: Spawn layer, dispatch, and the scp flows

**Files:**
- Modify: `app.go:213-215` (seam fields beside `xferDial`), `app.go:251` (init), `files_transfer.go:225-233` (`dirDestName` extraction), `files_transfer.go:426` and `:519` (dispatch lines)
- Create: the rest of `files_transfer_ext.go` (spawn, predicate, flows)
- Test: `files_transfer_ext_test.go`

**Interfaces:**
- Consumes: Task 3's arg functions; `transferState`, `errXferCancelled`, `a.xferFailed`, `a.xferRelease`, `a.update`, `a.relistDir`, `a.xferSave`, `a.xferPick` (files_transfer.go); `pasteName` (files_menu.go:120); the `newFilesHost` fixture and its `updateFn` marshalling (files_open_test.go:60-70).
- Produces (verbatim):

```go
// xferProc is a transfer's own process, as the app spawns it: the lines
// its stdout writes, why it failed on its stderr, and the two ends of
// it — Kill the ✕ takes, Wait the exit the flow takes.
type xferProc interface {
	Stdout() io.Reader
	Stderr() io.Reader
	Kill() error
	Wait() error
}

// defaultXferSpawn runs the command, pipes both streams, starts it.
func defaultXferSpawn(name string, args []string) (xferProc, error)
```

App seam fields (app.go, beside xferDial; init at app.go:251 beside the others: `a.xferSpawn = defaultXferSpawn` — `rsyncOK` starts as an empty map):

```go
xferSpawn func(name string, args []string) (xferProc, error) // a test's stand-in for the process
rsyncOK   map[string]bool                                    // per-host: "rsync --version" answered 0
```

```go
// xferExtApplies says a transfer goes on a line of its own: the user chose
// scp or rsync, the host is ssh — scp and rsync speak no Windows path — and
// it is not Windows. Everything else, silently, is the built-in stream's.
func (a *App) xferExtApplies(h *host) bool

// xferRun runs one command as the transfer: it spawns, arms the ✕ on Kill,
// reads stdout a frame at a time to progress (nil for none), and waits.
// The ✕'s mark on x reads cancelled at the end; any other wait failure
// carries stderr's last words.
func (a *App) xferRun(h *host, x *transferState, name string, args []string, progress func(frame string)) error

// downloadFromExt / uploadToExt — the Download…/Upload Here… flows on a
// line of the host's own (see file header).
func (a *App) downloadFromExt(h *host, path string, isDir bool)
func (a *App) uploadToExt(h *host, dir string)
```

In `files_transfer.go`, two pure-ish helpers (this task's only edit inside it besides the dispatch):

```go
// dirDestName is the name a folder download takes under destDir: its own,
// or the "name copy" pasteName finds beside one of its own. (Extracted
// from downloadDirTo, which keeps its behavior.)
func dirDestName(destDir, name string) string

// downloadViaTemp lands a file download through a temp of its directory,
// renamed to name once into() has run whole: every failure path removes
// the temp under defer, so a download that fails partway leaves the
// directory as it was — nothing of the name it would have taken.
func downloadViaTemp(destDir, name string, into func(tmpPath string) error) (string, error)
```

Dispatch — the first lines of `downloadFrom` (files_transfer.go:426, after the busy gate) and `uploadTo` (:519):

```go
if a.xferExtApplies(h) {
	a.downloadFromExt(h, path, isDir)
	return
}
```

Flow shape (mirroring the built-in flows line for line so the row behaves identically):
- `uploadToExt`: busy gate → claim slot → `FilesUploadToast` toast → `xferPick(false,true)` (walked-away releases) → stat the files, `x.total` = their sum **before the run** (the built-in's own move, files_transfer.go:540-548 — scp writes no progress so `done` holds still at 0, rsync's frames fill it in Task 5) → `exec.LookPath("scp")`; a miss is a failure, not a fallback (decision 10: the choice is deliberate) → `a.xferFailed(x, err)` → `xferRun` with `scpUploadArgs` → done: `xferRelease` + `FilesUploadDone` + `a.relistDir(h, dir)`.
- `downloadFromExt` (scp): busy gate → claim → toast → dialog as the built-in's (dir: `xferPick(true,false)`; file: `xferSave(name)`) → LookPath, same failure-not-fallback → file: `downloadViaTemp(dest, file, tmp → xferRun scp host:path tmp)`, dir: `dest := dirDestName(destDir, name)` then `xferRun` with `scpDownloadArgs(..., true, dest)` → done toasts as the built-in's (`FilesDownloadDone`). A download's total is unknowable to scp → `x.total` stays 0, the arc spins.
- `xferRun`: `a.xferSpawn` → arm cancel (`x.cancel = func() { x.err = errXferCancelled; p.Kill() }` via `a.update`, the exact shape of `xferCancel` at files_transfer.go:591-598) → drain **Stderr** into a bounded buffer (4KB) in a goroutine → if progress != nil, read **Stdout** split on `\r`/`\n` and hand each frame over (rsync's progress rides stdout; stderr only ever carries its errors) → `Wait()`: `x.err != nil` → `return errXferCancelled`; exit error → `return fmt.Errorf("%v: %s", err, tail of stderr)`.

- [ ] **Step 1: Write the failing predicate test** — `TestXferExtApplies`: table over `{set.FilesTransfer, addr.Scheme, windows}` → true only for `scp|rsync` × `ssh` × non-Windows. (Set `f.h.addr = remote.Addr{Scheme: "ssh", Host: "srv"}` on the `newFilesHost` fixture — its host has an empty addr; a Windows host is `info.OS = "windows"` per hosts.go:102-107.)
- [ ] **Step 2: Write the failing fallback test** — `TestXferExtFallsBackSilently`: with `FilesTransfer="scp"` but scheme `wsl` (and a second case, `info.OS="windows"`), `downloadFrom` must reach the built-in path: assert the fake `xferSpawn` is never called while the fake `xferDial` is.
- [ ] **Step 3: Write the failing scp flow tests** (fake `xferSpawn` returning a scripted `xferProc` — a `fakeProc` type holding `out string, errOut string, waitErr error`, its `Wait` blocking on a channel so Kill can be observed):
  - `TestScpExtUploadOverwrites` — two files picked; asserts the spawn saw `scpUploadArgs`' exact argv (target `srv:"/srv"`, no " copy"), `x.total` equals the two files' stat sum while the fake proc still waits, toasts `FilesUploadDone(2, "remote")`, `relistDir` ran (same assertion as TestUploadToRefreshesTree, files_transfer_test.go:1114).
  - `TestScpExtDownloadFileAtomic` — `xferSave` → `/dest/a.txt`; fake proc fails (`waitErr` set, stderr "No such file"); asserts **no** `a.txt` and no `.kero-dl-*` left in /dest (the defer'd removal), toast `FilesTransferFailed(remoteError(…))`, slot released.
  - `TestScpExtDownloadDirDedupsName` — pre-create `dest/proj/`; assert the spawn's dest is `dest/proj copy` (pasteName) and, on success, `FilesDownloadDone`.
  - `TestScpExtDownloadSpins` — during a download run `x.total == 0` (scp cannot size the remote file; the arc spins). An upload's total is the stat sum — pinned above, not here.
  - `TestXferExtCancelKillsProc` — fake proc's Wait blocks; take `x.cancel()` (as TestTransferCancel drives the row's ✕, files_transfer_test.go:1197); assert `Kill` was called, then release Wait → toast `FilesTransferCancelled`, not failed.
  - `TestXferExtMissingBinaryFails` — `t.Setenv("PATH", "")` so `exec.LookPath("scp")` misses (the one failure source; the spawn seam stays for the other cases); assert the `xferFailed` toast (not a fallback, not silence — decision 10), the spawn never called, and the slot released.
  - `TestScpExtPickWalkedAway` — `xferPick` answers empty (the dialog walked away from): the slot is released — `a.side.files.xfer == nil` — with no spawn and no failure toast, as the built-in flow's own walked-away does.
- [ ] **Step 4: Run all to verify they fail** — `go test . -run 'XferExt|ScpExt'` → FAIL.
- [ ] **Step 5: Implement** — the seam fields + init (app.go), `dirDestName`/`downloadViaTemp` (downloadDirTo rewritten onto `dirDestName`, its behavior covered by `TestDownloadDirToExistingName`), the dispatch lines, then `files_transfer_ext.go`'s spawn layer, predicate and the two scp flows.
- [ ] **Step 6: Run** — `go test . -run 'XferExt|ScpExt|DownloadDirTo'` → PASS (the last one proves the extraction changed nothing).
- [ ] **Step 7: Verify** — `go vet . && go test . -short` (whole root package still green).

### Task 5: rsync — probe, cache, progress

**Files:**
- Modify: `files_transfer_ext.go` (probe + the two rsync flows + the parser)
- Test: `files_transfer_ext_test.go`

**Interfaces:**
- Consumes: Task 4's `xferRun`/`xferSpawn`/flows; `locales.FilesRsyncAbsent` (Task 2); `remote.SSHCommand()`.
- Produces (verbatim):

```go
// parseRsyncProgress reads one frame of rsync --info=progress2 as it
// rides stdout: the bytes done, whole, and the percent rsync itself
// figures — 0 before it has one. The frame is rsync's own layout,
// ^\s*([\d,]+)\s+(\d+)%; a frame that does not parse is not an error —
// rsync writes other lines, stderr never carries frames, and the next
// frame is a keystroke away.
func parseRsyncProgress(frame string) (done int64, pct float64, ok bool)

// probeRsync asks the host over ssh whether rsync is there — once per host
// per run: the answer cannot change between two transfers, and its ssh hop
// is a second connection every transfer would otherwise pay. Read on the
// transfer's goroutine only: the one slot serializes the map's users.
func (a *App) probeRsync(h *host) bool
```

Flow deltas from the scp ones (share `downloadFromExt`/`uploadToExt` with a mode switch inside — the dialog/claim/toast skeleton is one code path; only the argv and the progress reader differ):
- Before the first rsync transfer to a host: `probeRsync` (`xferSpawn(SSHCommand(), rsyncProbeArgs(h.addr))`, Wait error → false, cached in `a.rsyncOK[h.name]`). A false probe aborts: `a.update(func() { a.toast(locales.FilesRsyncAbsent(h.name)); a.xferRelease(x) })` — not `xferFailed`, no fallback, no built-in stream touched (decision 10's tool-absence arm).
- Progress: `xferRun`'s stdout frame callback applies `parseRsyncProgress`; `ok && pct > 0` → throttled `a.update` setting `x.done = done` and — **downloads only**, whose total no stat could size — `x.total = done * 100 / pct` (only forward: `max` against the previous values — rsync's first frames restate totals). An upload's total is the stat sum Task 4 already set; percent never rewrites it. Throttle: at most one update per 100ms (`time.Since(last)`, the final frame always passes — rsync refreshes near 10/s, and the UI thread needs a tenth of that).
- rsync upload argv: `rsyncUploadArgs`; downloads: file through `downloadViaTemp` (tmp as rsync's dest, same atomic rename and defer'd cleanup), dir through `dirDestName` + `rsyncDownloadArgs(..., true, root)`.

- [ ] **Step 1: Write the failing parser test** — `TestParseRsyncProgress`, table of real frames:

```
"          1,234,567  45%  10.00MB/s    0:00:01"        → 1234567, 45, true
"              4,096   0%    0.00kB/s    0:00:00"       → 4096, 0, true
"         12,345,678 100%  12.34MB/s    0:00:02 (xfr#1, to-chk=0/3)" → 12345678, 100, true
"" / "rsync: link_stat ..." / "        N bytes sent"    → 0, 0, false
```

- [ ] **Step 2: Write the failing flow tests** (fake `xferSpawn` that counts probe calls and answers them separately from transfer calls, by argv shape — the probe's argv ends in `rsync --version`):
  - `TestRsyncProbeAbsentAborts` — probe's Wait errors → toast `FilesRsyncAbsent("remote")`, slot released, transfer spawn never called.
  - `TestRsyncProbeCachesPerHost` — two uploads; the probe spawn runs once.
  - `TestRsyncProgressDrivesArc` — fake proc writes two frames, `\r` between, to its **stdout** (stderr carries only failure text — a reader wired to stderr would pass this test against a reader that can never see a real transfer, so the frames must go where the real ones do), then exits 0; assert `x.done`/`x.total` reached the parsed pair (1234567/45% → total ≈ 2743460) and the done toast.
  - `TestRsyncDownloadDirExcludesGit` — dir download argv contains `--exclude=.git`; file download's does not.
  - `TestRsyncUploadTotalStaysStat` — an upload under frames claiming a different percent keeps `x.total` at the stat sum (percent backs out only a download's total).
- [ ] **Step 3: Run to verify they fail** — `go test . -run 'Rsync'` → FAIL.
- [ ] **Step 4: Implement** the parser, the probe+cache, the mode switch in the flows, the throttled progress update.
- [ ] **Step 5: Run** — `go test . -run 'Rsync|XferExt|ScpExt'` → PASS.
- [ ] **Step 6: Verify** — `go vet .`.

### Task 6: Whole-tree verification and handoff notes

**Files:** none created; all touched files reviewed.

- [ ] **Step 1: Full gates** — `go vet ./...` and `go test ./internal/...` (CI's pair) and `go test .` (the root package, full — first run may download libghostty-vt; `-short` only if that fetch is impossible).
- [ ] **Step 2: Diff hygiene pass** — `git diff --stat`: expect `internal/settings/settings.go(+3)`, `internal/settings/config_test.go(+~20)`, `settings_view.go(+6)`, `settings_search.go(+1)`, `settings_test.go(+~20)`, `locales/{en,zh-Hans,ja}.json(+6 keys each)`, `locales/catalog_gen.go` (generated), `app.go(+~6)`, `files_transfer.go(+~10 net)`, `files_transfer_ext.go`+`_test.go` (the feature). Anything else — above all anything under `internal/remote/` or any other `internal/` package — is a drive-by to undo.
- [ ] **Step 3: Manual QA note for the user** (write into the final report, not the code): on a real ssh host — scp upload overwrites, scp download of a folder with a same-named local one lands "copy", rsync absent host shows the new toast once, ✕ mid-rsync says cancelled, and the arc fills by rsync's own numbers.

---

## Risks the executor should know (found during planning)

- **scp ≥ 9.0 speaks SFTP by default** — overwriting and `-r` behave the same under it; the plan adds no `-O`. If a host's scp quirk surfaces in QA, that is a new decision, not this plan's.
- **`xferSSHOpts` is a copy, not a share** — `internal/remote` stays untouched (kerod's build fingerprint), so if transport.go's option list ever changes, `xferSSHOpts` follows by hand; `TestXferSSHOpts` pins today's snapshot so the drift is at least loud.
- **rsync `-e` is word-split by rsync, not a shell** — safe only while every option value, port and user is space-free; `TestRsyncUploadArgs` pins the joined string.
- **scp's remote-side quoting is shell-style, not a fortress** — `quoteRemotePath` covers space, `\`, `"`, `$`, which is what a path can meet in the remote shell's double quotes; a path with a backtick or `!` would still trip some shells, and no scp line can do better.
- **The probe and the transfer are two ssh connections** — first rsync transfer per host pays both; that is the documented cost of the cache (decision 4).
- **`h.addr` is empty on the test fixture** (`newFilesHost` builds the host without one) — every ext test sets `f.h.addr` itself; forgetting it makes the predicate silently fall back and the test fail confusingly.
- **KERO_SSH still applies to the probe only** (via `remote.SSHCommand()`); scp/rsync binaries are found by plain `exec.LookPath` — decision: no env overrides (Global Constraints).
