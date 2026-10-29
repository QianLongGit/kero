# Files View Download & Upload over a Raw Transfer Stream — Design

Date: 2026-10-29
Status: awaiting review

## Intent and why

The Files view in the side panel already lists and edits files on remote
(SSH) hosts, but moving files between the local machine and the host still
means dropping to a terminal and running `scp`/`rsync`. This adds two
context-menu actions — **Download…** (file or folder to the local machine)
and **Upload Here…** (local files into a remote folder) — backed by a
dedicated binary transfer stream so files of any size work, with real
byte-level progress and cancellation.

The partner explicitly picked route **C** (raw yamux stream, binary
payloads) over reusing `fs.read`/`fs.write` (32 MB JSON-base64 cap) or a
chunked JSON-RPC variant. GB-scale files and honest progress are the point.

## Goals

- **Download file**: file row → Download… → native Save dialog → stream
  the file to the chosen path.
- **Download folder**: folder row → Download… → native directory picker →
  recursive tar download into `dest/<name>` preserving structure, mode and
  mtime (best effort).
- **Upload**: folder row → Upload Here… → native Open dialog (multi-select
  files) → stream each file into that remote folder, then refresh the tree.
- Byte-level progress (current name, MB done / MB total, arc indicator)
  with a cancel control.
- Works over every transport a host connection already uses (`ssh`,
  `wsl`, tailnet `http`/`https`) — the stream is just another yamux stream.
- Old remote kerod fails cleanly through the existing "unknown method →
  update host" path.

## Non-goals / anti-goals

- No resume of interrupted transfers; cancel discards the partial temp
  file.
- No directory upload in v1 (the request was "upload files"; tar-upload of
  trees is a natural follow-up once this ships).
- No drag-and-drop upload onto the Files view (the panel already routes
  drops to project-open); no simultaneous transfer queue. One transfer at
  a time, app-wide, others get a busy toast.
- No overwrite prompts: uploads dedup names (`x copy.txt` style, daemon
  side); downloads write to a temp file in the destination directory and
  rename into place, so a picked-existing Save name is simply overwritten —
  that is what the Save dialog means.
- `proto.Version` does not bump: the addition is backward compatible
  (see Protocol).

## Constraints

- Repo rules: one feature per root-package file (`files_transfer.go` +
  `files_transfer_test.go`), English comments, minimal diff, existing
  menu/toast/spinner idioms, `locales/{en,zh-Hans,ja}.json` in sync and
  `mygo generate` for `catalog_gen.go` (never hand-edit).
- The stream must die gracefully against an old remote daemon (every
  deployed kerod out there today).
- CI runs `go vet ./...` and `go test ./internal/...`; root-package tests
  run locally.

## Prior art

- `files_open.go` `materialize` — fs.stat + fs.read into a local cache,
  watcher write-back: the existing "download" for single files, 32 MB cap.
- `paste.go` `remotePaste` — dedicated `client.ConnectVia(h.dialer())`
  connection on a goroutine, toast feedback, caps `maxFile` 32 MB /
  `maxUpload` 64 MB: the existing "upload" via `paste.file`.
- `files_menu.go` `pasteFileInto` — client-side dedup probing with
  `x copy` naming, refresh via `fetchDir` after write.
- `frame.go` binary framing (kind byte + u32 length, 16 MB cap) — used by
  attach streams; reusable, but this design passes raw bytes with a
  declared length instead (no per-16 MB chunking needed).
- `statusIconP` (view.go:916) — arc progress via `ui.Path` + `StrokePath`
  + `Trim(0, frac)`; the visual pattern for the transfer indicator.

## Approaches considered

1. **Reuse `fs.read`/`fs.write`** — zero protocol change, but hard 32 MB
   per-file cap and base64-in-JSON overhead; rejected by the partner.
2. **Chunked JSON-RPC** (`fs.read` with offset/limit variants) — arbitrary
   size, progress by bytes, but every chunk pays JSON envelope + base64
   and the daemon needs offset bookkeeping; rejected by the partner.
3. **Raw yamux stream with binary payloads** (chosen) — open a new stream
   on the existing `remote.Conn`, exchange short JSON header lines, then
   stream raw bytes. Full bandwidth of the mux (256 KB windows; the
   session-wide deflate stays on — it cannot be turned off per stream and
   a second uncompressed mux connection would mean a second ssh process
   per transfer), progress is free (count bytes against a declared
   total), cancel is stream close. Costs: a new connection kind in
   `serveHello` and a small wire protocol, and old daemons must be
   handled (they are, below).

## Protocol (the core of the design)

One new connection kind. Everything else reuses existing envelopes.

### Stream setup

1. Client opens a stream on the host's yamux session: `h.rc.Open()`
   (what `h.dialer().Dial()` returns). On `wsl`/`http` transports the
   same call lands in the same place — every non-mux-host stream's first
   JSON line reaches `Daemon.serveHello` (daemon.go:655).
2. Client writes `proto.Hello{Kind: "transfer"}` (new const
   `proto.KindTransfer`) as a JSON line, reusing the standard handshake:
   read one `Welcome` line, enforce `Version` and `Welcome.Error` exactly
   like `client.handshake` does.
3. Daemon: new `case proto.KindTransfer:` branch in `serveHello` before
   the control default → `serveTransfer`. **Old daemons coerce the
   unknown kind to control and answer the next line as an RPC** — which
   is the compatibility trick, see next.

### Requests and replies

The request is a normal `proto.Request` JSON line — method
`transfer.download` or `transfer.upload`. Replies are normal
`proto.Message` lines (`{ID, Result, Error}`).

- **Old daemon**: answers `Message{Error: "unknown method …"}` and the
  existing `remoteError` → "update host" flow (files.go:633) presents it.
  The client never sends binary bytes to an old daemon because it always
  reads one reply line first. No version bump, no capability probe.
- **New daemon, download**: validates the path (`hostPath`), stats it.
  File → replies `Message{Result: {dir: false, size, mode}}` then streams
  exactly `size` raw bytes. Folder → pre-walks (`filepath.WalkDir`);
  walk errors reply `Message{Error}`; success replies
  `Message{Result: {dir: true, size: <sum of regular files>, count}}`
  then streams a tar archive (relative entry names, dirs, symlinks,
  mode + mtime; `.git` skipped to match what the view shows) and closes
  the stream. Mid-stream read failures after the header simply close the
  stream; the client reports an incomplete transfer (accepted limitation).
- **New daemon, upload**: one request per file, sequential on the same
  stream. Client: `Request{dir, name, size, mode}` → daemon validates the
  directory and name (early errors are cheap) → `Message{Result: {}}`
  ("ready") → client streams exactly `size` raw bytes → daemon writes to
  a temp file in the target dir, resolves a non-clobbering final name
  (`name`, `name copy.ext`, `name copy 2.ext`…), renames, replies
  `Message{Result: {path: <final>}}`. The two-phase ready hop keeps the
  client from ever blocking on a full yamux window while writing toward
  an already-failed request.

No framing on the payloads: each direction's byte count is declared in
the JSON header, so `io.Copy` runs unbroken. Cancel from either side is
closing the stream; the peer sees a short read / write error.

### Where things live

- `internal/proto/transfer.go` — `KindTransfer`, params/result structs
  (`TransferDownloadParams`, `TransferHeader`, `TransferUploadParams`,
  `TransferUploadDone`).
- `internal/daemon/transfer.go` + `transfer_test.go` — `serveTransfer`,
  walk/tar, temp+rename write, daemon-side dedup (reuse `fsWrite`'s
  temp+rename shape, fs.go:175-189).
- Root package `files_transfer.go` + `files_transfer_test.go` — the
  client side speaks the wire directly on the `net.Conn` from
  `h.rc.Open()` (root already imports `internal/proto` heavily; no new
  internal client module).

## UI

### Menu items

- `fileMenu` (files_menu.go:33), gated on `h.remote()` (any remote host —
  ssh, wsl, tailnet — not just ssh; same plumbing, same need):
  - file rows: `Download…` between the Open With group and Copy.
  - folder rows: `Download…`, `Upload Here…` before Paste.
- The tree root is not a row, so uploading into the shown root needs the
  root-dir label in the panel header (side.go, the `shortDir` text at
  :197-199) to carry a context menu with `Upload Here…` (wrap in a Row if
  `ui.Text` has no `ContextMenu`). Same `h.remote()` gate.

### Flow

- Download file: `mygo.Dialog.Save{DefaultPath: <name>}` → goroutine
  stream → temp file in the destination directory → rename.
- Download folder: `mygo.Dialog.Open{Directory: true, CreateDirectories:
  true}` → `dest/<name>` (or `dest/<name> copy` if it exists) → stream
  untar with entry-name sanitizing (reject absolute and `..` paths,
  files via temp+rename, mode/mtime best effort).
- Upload: `mygo.Dialog.Open{Multiple: true}` (files only) → stream each
  sequentially → collect final paths → `fetchDir` refresh + summary toast.
- Dialogs block until dismissed and may be called from any goroutine
  (mygo contract); open them from the transfer goroutine, not the UI
  frame.

### Progress

`filesState` gains one active-transfer slot (`xfer`): kind, host name,
current file, bytes done, bytes total, cancel func (`conn.Close`), done
flag. The Files view shows a slim transfer row while active — arc
progress in the `statusIconP` style, `name — 12.3 / 45.6 MB`, and a ✕
cancel button — rendered above the tree; while `total` is still 0 (pre-
header) it shows the spinner variant. The side panel header (visible on
both tabs) shows a compact version of the same arc + ✕ while a transfer
runs, so switching to Changes or another tree does not hide an in-flight
transfer; with the whole panel closed the transfer still finishes but
has no cancel control (accepted v1 limitation). The slot is claimed
synchronously at menu-action time on the UI thread (race-free busy
gate) and released if the dialog is canceled. Folder-download progress
is bytes/total clamped at 100% on EOF (the tar container adds ~512 B per
entry). A busy transfer makes further menu actions toast
"transfer in progress".

## Locales

New keys in `en.json` / `zh-Hans.json` / `ja.json` (flat, camelCase,
`{placeholder}` interpolation), then `mygo generate`:
`filesDownload`, `filesUploadHere`, `filesTransferBusy`,
`filesDownloadToast`, `filesUploadToast`, `filesDownloadDone`,
`filesUploadDone`, `filesTransferFailed`, `filesTransferCancelled`.

## Tests

- `internal/daemon/transfer_test.go` (CI): download file (content +
  mode), download folder (tar structure, `.git` skipped, symlink kept,
  entry names relative), upload lands with dedup naming, bad dir / bad
  path → `Message{Error}`, and the old-daemon path — a stream whose Hello
  says `transfer` served by today's control path answers `unknown
  method` (net.Pipe, browser_test.go:273 pattern).
- Root `files_transfer_test.go` (local): the client against a fake
  daemon over `net.Pipe` speaking the same wire — download to disk,
  upload with refresh invoked, cancel mid-stream, progress math; menu
  wiring smoke through `ui.NewTester` where the existing menu tests lead.
- Manual QA on a real SSH host (screenshot evidence): a large (>32 MB)
  file both directions, a folder download, cancel mid-transfer.

## Build order

1. `internal/proto/transfer.go` structs.
2. `internal/daemon/transfer.go` + tests (download file/folder, upload,
   dedup, old-daemon reply).
3. Root `files_transfer.go` client + `net.Pipe` tests.
4. Menu items, dialogs, goroutines, refresh, busy gate.
5. Progress row + cancel.
6. Locales ×3 + `mygo generate`.
7. `go vet ./...`, `go test ./internal/...`, `go test .`, manual QA.

## Risks

- **Old remote kerod** — answered by design (unknown-method reply →
  existing update prompt). The one soft spot: a daemon neither new nor
  ancient that coerces the kind but errors differently; the client treats
  any `Message{Error}` the same way.
- **Throughput** — yamux 256 KB windows cap a single stream; fine for
  WAN, and SSH itself is usually the bottleneck. Not tuned in v1.
- **Mid-stream daemon failure** (folder file unreadable after the walk)
  → stream close → generic "incomplete" error; partial temp discarded.
- **Tar path traversal** — client sanitizes entry names; entries are
  generated from a real walk anyway.
- **Disk full** mid-write — temp+rename on both sides means no half
  files at their final names.

## Calls I made

- Show the menu items on **every** remote host, not only `ssh://` — the
  partner said "SSH 主机" but wsl/tailnet hosts use the identical path
  and excluding them is extra code for no user benefit.
- **One transfer at a time app-wide** (single `xfer` slot); concurrent
  requests get a busy toast. Simplest thing that is honest.
- **Dedup, never overwrite, on upload** — matches the existing paste
  behavior; downloads overwrite only what the Save dialog explicitly
  named.
- **`.git` skipped** in folder downloads to match the view (which hides
  it, files.go:328).
- **No frame layer on payloads** — declared-length raw bytes; framing
  buys nothing here and re-imposes a 16 MB ceiling.
- **Ready-hop on upload** — two tiny JSON lines per file to make early
  errors safe without a concurrent-writer dance.
- **Ride the existing mux session (deflate stays on)** — per-stream
  compression cannot be disabled; a dedicated uncompressed mux
  connection would spawn a second ssh process per transfer. The 256 KB
  yamux window dominates throughput anyway; revisit only if measured
  otherwise.
- **Side panel header carries a compact progress + cancel** while a
  transfer runs, in addition to the Files-view row — otherwise switching
  tabs or trees hides an in-flight transfer with no way to cancel.
- **Busy slot claimed at menu-action time** (synchronously on the UI
  thread), released when the dialog is canceled — no race window between
  the gate check and the goroutine start.

## Left to the builder

- Exact layout/typography of the progress row (follow `filesPreviewRow`
  and the header row idioms).
- Where `serveTransfer` hooks into `serveHello`'s switch so the attach
  default behavior is untouched.
- Locale wording polish (zh-Hans/ja phrasing of the new strings).
- Whether the `shortDir` label needs a Row wrapper for `ContextMenu` —
  check the mygo `ui.Text` surface first.
