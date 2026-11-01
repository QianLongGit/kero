# Kero Attach Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `kero attach [-t target]` enters a session's terminal from any terminal (a phone over SSH), passing the program's original bytes both ways — first frame is the full Mirror restart, `Ctrl+B` `d` detaches, a dropped connection reattaches and repaints — plus `kero new -s` as an alias of `-n`.

**Architecture:** One optional field each on `proto.Hello`/`proto.Welcome` (`raw`) selects a raw attach. The daemon still builds the first frame through the Mirror restart path, then `output()` tees the original bytes to raw attachments under the pane lock (same `out` channel, same drop-kill); `frame()` skips raw attachments. The CLI (`internal/cli/attach.go`) is byte plumbing: raw mode, `FrameClaim` + pixel `FrameResize` from `TIOCGWINSZ`, a pure byte-stream prefix interceptor for the `Ctrl+B` chord, and reattach-on-clean-EOF with backoff. The phone terminal is the emulator.

**Tech Stack:** Go; `internal/proto`, `internal/client`, `internal/daemon`, `internal/cli`; `golang.org/x/term` (IsTerminal/MakeRaw/Restore) and `golang.org/x/sys/unix` (TIOCGWINSZ) — both already dependencies.

**Spec:** `specs/2026-11-02-kero-attach-design.md` (read it first — the plan argues from it)

## Global Constraints

- Comments/doc comments in English prose explaining why, matching neighboring code (project CLAUDE.md).
- No `proto.Version` bump: `Hello.Raw`/`Welcome.Raw` are optional fields, exactly the growth the constant's own comment licenses (spec §Protocol, proto.go:23-27).
- Every changed line traces to the spec; no drive-by refactors; root package (`order.go` etc.) is NOT touched — the `updatedKey` comparison is copied into `internal/cli` because the root imports it (spec §Commands).
- Every task is TDD: failing test first, then minimal code. Task gate = its package tests green; whole-plan gate (Task 9) = `go vet ./...` && `go test ./internal/...`.
- Pre-existing failures: `TestCommandInForeground` and `TestCommandTellsExit` (`internal/daemon/shell_test.go`, zsh-on-this-machine problem) already fail before any change — ignore exactly those two; everything else must be green.
- Test isolation: daemon tests via `testDaemon` (`t.Setenv("KERO_CONFIG", t.TempDir())`, browser_test.go:16) and the `transferConn` client plumbing (transfer_test.go:32-45); cli tests set `KERO_CONFIG` into a temp dir as events_test.go:14 does.
- No git commits, branches, or pushes (user commits; a task ends at its green verification).
- The `go` binary may need PATH/GOROOT setup before running tests: if `which go` fails, use `GOROOT=$(ls -d ~/sdk/go* | head -1); export GOROOT` and `$GOROOT/bin/go` (or `~/sdk/go/bin/go` directly).

## Review Focus

1. **Old daemon degradation** — a daemon that skips `raw` serves a Mirror stream; the CLI must still write those frames to stdout verbatim and not crash or misbehave. Pinned by Task 8 Step 1 (`TestAttachLoopMirrorFallback`).
2. **`Ctrl+B` alone is eaten** — a prefix with no follow-up byte must reach the program as nothing (tmux's own behavior, spec §CLI). Pinned by Task 6 Step 1 (`feed` of a lone `0x02` forwards nothing).
3. **Laggy chord past 500 ms** — prefix discarded alone, the late `d` forwarded as ordinary input, no detach loop. Pinned by Task 6 Step 1 (timeout rows).
4. **Pane exit must not reattach** — `FrameClosed` (stream gone) exits 0; only a clean EOF without `Gone` reconnects, else a dead pane loops forever. Pinned by Task 8 Step 1 (`TestAttachLoopExitsOnFrameClosed`).
5. **stdin ending exits** — SSH dying closes stdin too; the loop must exit, not reconnect into a void. Pinned by Task 7 Step 1 (`TestAttachLoopStopsOnStdinEnd`).

---

### Task 1: `proto.Hello.Raw` / `proto.Welcome.Raw`

**Files:**
- Modify: `internal/proto/proto.go:30-61` (Hello and Welcome structs)
- Test: `internal/proto/proto_test.go` (add)

**Interfaces:**
- Consumes: nothing.
- Produces: `Hello.Raw bool \`json:"raw,omitempty"\`` (attach only: the client asks for the program's original bytes), `Welcome.Raw bool \`json:"raw,omitempty"\`` (echo: the daemon honored it). Tasks 2, 3, 7 read these.

- [x] **Step 1: Write the failing test**

Add `TestRawFieldJSON` to proto_test.go (style of the file's table tests): marshal `Hello{Kind: "attach", Session: "x", Raw: true}` → output contains `"raw":true`; marshal `Hello{Kind: "attach"}` → output does not contain `"raw"`; unmarshal `{"kind":"attach","raw":true}` into a `Hello` → `Raw == true`; unmarshal `{"version":3}` into a `Welcome` → `Raw == false` (an old daemon's answer leaves it out).

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/proto/ -run TestRawFieldJSON -v`
Expected: FAIL — no `Raw` field.

- [x] **Step 3: Implement**

Add `Raw bool \`json:"raw,omitempty"\`` to `Hello` after `Client` (proto.go:42) with a comment saying it asks an attach connection for the program's original bytes; the same field on `Welcome` after `Rows` (proto.go:60) with a comment saying it echoes the ask when the daemon serves raw bytes — an older daemon leaves it out, and the client then reads the ordinary Mirror stream. No `Version` bump (Global Constraints).

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./internal/proto/`
Expected: PASS (whole package).

---

### Task 2: `client.AttachViaRaw`

**Files:**
- Modify: `internal/client/client.go:313-330` (beside `AttachVia`)
- Test: `internal/client/stream_test.go` (add)

**Interfaces:**
- Consumes: Task 1 `Hello.Raw`; existing `Dialer` (client.go:32), `handshake` (client.go:115).
- Produces: `func AttachViaRaw(d Dialer, id string) (*Stream, error)` — identical to `AttachVia` except the Hello carries `Raw: true`. The returned `Stream` already exposes the mode through `Stream.Welcome.Raw` (Welcome is a field, client.go:298 — nothing to record by hand). Task 7 dials through this.

- [x] **Step 1: Write the failing test**

Add `TestAttachViaRawAsks` to stream_test.go: a `net.Pipe` pair; a goroutine serving the daemon side reads one JSON line into `proto.Hello`, asserts `Kind == "attach"`, `Session == "p1"`, `Raw == true`, then writes `{"version":<proto.Version>,"raw":true}` + `\n`. A one-shot `Dialer` whose `Dial()` returns the client end. `AttachViaRaw(d, "p1")` → `err == nil`, `stream.Welcome.Raw == true`.

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/client/ -run TestAttachViaRawAsks -v`
Expected: FAIL — `AttachViaRaw` undefined.

- [x] **Step 3: Implement**

A parallel function below `AttachVia` (client.go:319): same body, Hello `{Kind: "attach", Session: id, Raw: true}`. Doc comment: a sibling of AttachVia rather than opts plumbing — the minimal change; the Welcome's Raw echo tells the CLI which mode it got (spec §Commands). Do not touch `AttachVia`.

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./internal/client/`
Expected: PASS (whole package).

---

### Task 3: Daemon raw branch — first frame, tee, echo, drop-kill

**Files:**
- Modify: `internal/daemon/session.go` (`attachment` struct ~907-918, `output` 209-233, `frame` 687-705)
- Modify: `internal/daemon/daemon.go:792-843` (`serveAttach`)
- Test: `internal/daemon/attach_test.go` (new)

**Interfaces:**
- Consumes: Task 1 `proto.Hello.Raw`/`Welcome.Raw`.
- Produces: `attachment.raw bool` (pane-mu-guarded like the other attachment fields). Raw attachments: first frame still from `s.attach`'s Mirror restart (unchanged code path), `output()` tees `p` verbatim to each raw attachment inside the pane lock right after `term.Feed`, `frame()` skips them, `serveAttach` echoes `welcome.Raw`. Input/resize/claim/close paths unchanged. The existing `send` drop-kill (session.go:924-934) covers raw overflow — no new queue.

- [x] **Step 1: Write the failing tests**

New `internal/daemon/attach_test.go`, `testDaemon` scaffolding, and an `attachConn(t, d, paneID string, raw bool) (net.Conn, *bufio.Reader, proto.Welcome)` helper modeled on `transferConn` (transfer_test.go:32-45): `net.Pipe` + `go d.serve(there, identity{...})` + `writeJSON(Hello{Kind: "attach", Version: proto.Version, Session: paneID, Raw: raw})` + read Welcome. Panes come from `d.create(proto.CreateParams{Command: []string{"sh", "-c", `printf 'BEFORE-ATTACH'; cat`}})` (a real PTY per queries_unix_test.go's pattern; poll `p.readText(true, 0)` until it contains `BEFORE-ATTACH`).

- `TestRawAttachFirstFrameAndWelcome`: attach with `raw: true` → `Welcome.Raw == true`; the first `proto.ReadFrame` is `FrameOutput` whose payload contains `BEFORE-ATTACH` (scrollback replayed) and the restart sequence `"\x1b[H\x1b[2J"` (mirror.go's restart writes SGR+home+clear).
- `TestRawAttachBytesVerbatim`: same setup; write one `FrameInput` `"X\n"` on the connection; read frames until two have arrived; the concatenation of the `FrameOutput` payloads received after the first frame is exactly `"X\r\nX\r\n"` (the tty line discipline echoes, then `cat` writes it back; ONLCR makes both `\r\n`) — a Mirror diff stream would interleave cursor addressing, so byte equality pins "verbatim, not diffs".
- `TestAttachWelcomeNoRawEcho`: attach with `raw: false` → `Welcome.Raw == false`; the pane's later output still arrives as frames (the ordinary path).
- `TestRawDoesNotDisturbMirror`: attach client A raw, client B ordinary (`raw: false`); send `FrameInput` `"X\n"` through A; both A and B receive further `FrameOutput` frames (B's diff loop unaffected); A's concatenated payloads equal `"X\r\nX\r\n"` as above.
- `TestAttachmentSendOverflowFinishes`: construct `&attachment{out: make(chan []byte, 1), done: make(chan struct{})}` directly (same package), fill `out` with one entry, call `send(proto.FrameOutput, []byte("x"))` → `<-a.done` is closed (the raw tee rides `send`, so the drop-kill contract holds).

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/daemon/ -run 'TestRawAttach|TestAttachWelcomeNoRawEcho|TestRawDoesNotDisturb|TestAttachmentSendOverflow' -v`
Expected: FAIL — Welcome has no Raw echo; raw bytes never arrive (no tee).

- [x] **Step 3: Implement**

`session.go`: add `raw bool` to `attachment` with a comment: a raw attachment takes the program's original bytes (`kero attach`) — `output()` tees to it under the pane lock, `frame()` skips it; its first frame is still the Mirror restart. In `output()` (session.go:209-233), right after `s.term.Feed(p)` and still under the lock: loop `for c := range s.clients { if c.raw { c.send(proto.FrameOutput, p) } }` with a one-line comment (same lock as the first-frame snapshot: no gap, no reorder). In `frame()` (session.go:689): `if c.raw { continue }` at the top of the client loop.

`daemon.go` `serveAttach`: after `a := newAttachment(conn)` set `a.raw = hello.Raw`; before encoding the welcome (daemon.go:808), `if a.raw { welcome.Raw = true }` (echo the ask only when the attach succeeded — the error paths return without it). Nothing else: `FrameInput`/`FrameResize`/`FrameClaim` cases and `s.attach`'s Mirror first frame are reused as-is.

- [x] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run 'TestRawAttach|TestAttachWelcomeNoRawEcho|TestRawDoesNotDisturb|TestAttachmentSendOverflow' -v && go test ./internal/daemon/`
Expected: PASS — new tests and the whole package (the two pre-existing zsh failures excepted, Global Constraints).

---

### Task 4: `kero new -s` alias

**Files:**
- Modify: `internal/cli/cli.go:378` (the `new` case's valued-flag map)
- Modify: `internal/cli/cli.go:39-48` (usage block)
- Test: `internal/cli/attach_test.go` (new file, started here — Task 5 extends it)

**Interfaces:**
- Consumes: existing `parse` (cli.go:245) and the `new` case.
- Produces: `-s` accepted everywhere `-n/--name` is, pointing at the same canonical `name` value. `CreateParams.Name` and the daemon's trim-and-store (sessions.go:359) untouched.

- [x] **Step 1: Write the failing test**

Start `internal/cli/attach_test.go` with `t.Setenv("KERO_CONFIG", filepath.Join(t.TempDir(), "config"))` (events_test.go:14 pattern) and `TestAttachUsageText`: `strings.Contains(usage, "kero attach")` is false today → this test fails until Task 7; to keep this task self-contained, assert instead what it owns: `strings.Contains(usage, "-s, --name <name>")` — the alias line. Expected: FAIL (usage shows only `-n, --name <name>`).

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestAttachUsageText -v`
Expected: FAIL — `-s` absent from usage.

- [x] **Step 3: Implement**

cli.go:378: add `"s": "name"` to the valued map beside `"n": "name"` (a parser alias only — the canonical flag and everything downstream are untouched, spec §Commands). Usage block: change the `-n, --name <name>` line (cli.go:41) to read `-n, -s, --name <name>   name it` keeping the column alignment of the neighbors. No behavior beyond the map entry: registration-by-constant; the flag mechanism itself is already covered by existing parse usage, and end-to-end `kero new -s` lands in Task 9's manual QA list.

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestAttachUsageText -v && go vet ./internal/cli/`
Expected: PASS.

---

### Task 5: CLI target selection — `-t` via `find`, bare attach most-recently-active

**Files:**
- Create: `internal/cli/attach.go`
- Test: `internal/cli/attach_test.go` (extend)

**Interfaces:**
- Consumes: existing `find(st, ref) (target, error)` (cli.go:841) and `target.pane`/`target.s`; `proto.Session` fields `Activity`/`Since`/`Created`/`ID`/`Focus`.
- Produces: `func pickAttach(st proto.State, ref string) (paneID string, err error)` — `ref == ""` picks the most recently active session then its focus pane; otherwise `find` semantics (id, id prefix, name, display name, pane id; ambiguous → the candidates error, exit 3). Also the unexported helper `func lastActive(s proto.Session) int64` — the copied three-line `updatedKey` chain (order.go:122-130; copied, not imported: root imports internal/cli). Task 7 consumes `pickAttach`.

- [x] **Step 1: Write the failing tests**

Extend attach_test.go (KERO_CONFIG isolation as in Task 4), a table over `proto.State` values:

- bare, one session (`Activity: 5`) → its focus pane id.
- bare, tie on key (two sessions, equal `Activity`) → the smaller `ID` wins (stable tie-break by id, spec §Commands).
- bare, `Activity: 0, Since: 7` vs `Activity: 3` → the `Activity: 3` session; `Activity: 0` everywhere → falls to `Since`, then to `Created` (the `updatedKey` chain).
- bare, empty state → error wrapping `errNotFound`.
- `-t` by session id, by unique id prefix, by pane id (a pane target uses that pane, not the focus pane); `-t` naming several → the ambiguous error listing candidates (find's own).
- `-t` a session whose `Focus` is set → that pane; a session with no panes → an `errNotFound` error ("session … has no pane").

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run TestPickAttach -v`
Expected: FAIL — `pickAttach` undefined.

- [x] **Step 3: Implement**

`internal/cli/attach.go`, package doc absent (cli.go owns it); `lastActive` copied from order.go:122-130 with a comment naming where it is copied from and why (the import cycle). `pickAttach`: `ref != ""` → `find(st, ref)`, use `t.pane` when set, else the session's focus pane (`t.s.FocusPane()`, proto.go:367); `ref == ""` → loop sessions keeping `max(lastActive)`, ties broken by the smaller id, then the winner's `FocusPane()`. Comment the bare case: CLI-side selection over the state list, no daemon change (spec §Commands). Exited sessions are not filtered — the spec does not ask, and a dead target fails at attach with the daemon's own error.

- [x] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestPickAttach -v && go test ./internal/cli/`
Expected: PASS.

---

### Task 6: CLI prefix interceptor — pure byte-stream function

**Files:**
- Modify: `internal/cli/attach.go`
- Test: `internal/cli/attach_test.go` (extend)

**Interfaces:**
- Consumes: nothing.
- Produces: `type prefixFilter struct{...}` and `func (f *prefixFilter) feed(p []byte, now time.Time) (out []byte, detach bool)` — a stateful filter over the stdin byte stream; `out` is what to forward to the pane, `detach` requests a clean close. Constants `prefixWindow = 500 * time.Millisecond`, `escapeQuiet = 10 * time.Millisecond`. Task 7's stdin goroutine calls `feed` with `time.Now()` per read.

- [x] **Step 1: Write the failing tests**

Table tests over a fresh `prefixFilter`, feeding byte slices with explicit `time.Time`s (pure; no sleeps):

- ordinary bytes pass through untouched: `feed("abc", t0)` → `"abc"`, no detach (also: split across feeds, `feed("a",t0)`, `feed("bc",t1)`).
- the chord: `feed([0x02], t0)` → nothing forwarded, no detach (the prefix is held); `feed([0x64], t0+300ms)` → `detach == true`, nothing forwarded.
- invalid chord byte: `feed([0x02], t0)`, `feed(['x'], t0+100ms)` → nothing forwarded, no detach, filter reset (a later `feed("a", t1)` forwards `"a"`).
- window expiry: `feed([0x02], t0)`, `feed([0x64], t0+600ms)` → `[]byte("d")` forwarded — no detach; the prefix alone was discarded (spec's "window expiry discards only the prefix"). Same with any late byte: it goes through as ordinary input.
- a lone prefix stays eaten: `feed([0x02], t0)` then nothing — assert via the previous row's semantics (no output, no detach); pin with a row feeding `0x02` then, 600 ms later, `0x02` again → the second `0x02` opens a new window (nothing forwarded).
- ESC drains a whole sequence: `feed([0x02], t0)`, `feed([0x1b], t0+100ms)` → nothing; `feed([0x1b,'[','A'], t0+105ms)` → nothing (arrow key swallowed whole); `feed(['a'], t0+130ms)` → `"a"` (25 ms of silence > escapeQuiet ends the drain). Drain refreshes: `feed(['b'], t0+108ms)` also swallowed, quiet measured from the last byte.
- drain state ends cleanly: after the drain, ordinary bytes pass (no residue).

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run TestPrefixFilter -v`
Expected: FAIL — `prefixFilter` undefined.

- [x] **Step 3: Implement**

State machine with two flags (`window` + `windowAt time.Time`, `drain` + `lastAt time.Time`); per byte, in order:

1. draining: `now.Sub(lastAt) <= escapeQuiet` → swallow, `lastAt = now`; else clear drain and fall through with this byte.
2. window open and `now.Sub(windowAt) > prefixWindow` → close the window (the prefix was already eaten; this byte falls through as ordinary).
3. window open (fresh): `0x64` → clear window, return detach; `0x1b` → clear window, start drain (`lastAt = now`); any other byte (a second `0x02` included) → swallow it, clear window.
4. ordinary: `0x02` → open window (`windowAt = now`), swallow; anything else → append to `out`.

One feed call processes its whole slice at `now` (a batch is one keyboard read; intra-batch timing is noise). Doc comment carries the why: tmux's repeat-time/escape-time semantics the user asked to keep, and that a laggy chord past the window costs the prefix only — visible, recoverable, rare (spec §CLI).

- [x] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run TestPrefixFilter -v && go test ./internal/cli/`
Expected: PASS.

---

### Task 7: `attachCmd` — registration, refusal, raw mode, claim, resize, main loop

**Files:**
- Modify: `internal/cli/attach.go` (the command)
- Modify: `internal/cli/cli.go:32-170` (usage), `cli.go:173-175` (`Commands`), the `run` switch (beside `case "new"`, cli.go:376)
- Create: `internal/cli/attach_unix.go`, `internal/cli/attach_windows.go`
- Test: `internal/cli/attach_test.go` (extend)

**Interfaces:**
- Consumes: Tasks 1-2 (`AttachViaRaw`, `Welcome.Raw`), Task 5 `pickAttach`, Task 6 `prefixFilter`; existing `dialHost(host)` (remote.go:106) and `client.Local{}` as `client.Dialer`s (a `*remote.Conn` is itself a `Dialer`, mux.go:215).
- Produces:
  - `func attachCmd(st proto.State, args []string, host string) error` — the switch case's body (cli.go:343's `st` and `run`'s `host` in scope).
  - `func attachLoop(dial func() (*client.Stream, error), in io.Reader, out io.Writer, winch <-chan struct{}, size func() (int, int, int, int, error)) error` — the testable core: owns the connect/claim/resize/stdin/stdout loop, no terminal or signal code of its own; `dial` closes over the pane id, `size` yields cols/rows/pixels (tests inject fixed values). Task 8 extends its EOF branch.
  - `func termPixels(fd int) (cols, rows, wpx, hpx int, err error)` — platform files: unix via `unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)` (x/sys, an existing dependency); windows via `term.GetSize` with pixels 0 (pixel geometry is an SSH-phone nicety, absent there).
  - `func winchSignals() <-chan os.Signal` — platform files: unix `signal.Notify` on `SIGWINCH`; windows returns `nil` (no such signal; resizes arrive as input-driven refresh anyway).

- [x] **Step 1: Write the failing tests**

Extend attach_test.go. A `fakeDaemon` helper: `net.Pipe` + a goroutine speaking the attach protocol (read the Hello line, write `{"version":<v>,"raw":true}`, then scripted `proto.WriteFrame`s / close), plus a `dial` func returning the client end — Task 8 reuses it.

- `TestAttachUsageText` (Task 4's file, now full): usage contains `kero attach` and `-t, --target`.
- `TestAttachRefusesNonTerminal`: skip if `term.IsTerminal(os.Stdin.Fd())` (a human running `go test` interactively); `attachCmd(proto.State{}, nil, "")` → error wrapping `errUsage` (exit 2; checked before any dial — a piped attach has no renderer and no size, spec §CLI lifecycle 1).
- `TestAttachLoopWritesFrames`: fakeDaemon sends `FrameOutput("HELLO")` then `FrameClosed` then closes; `in` an empty reader; `attachLoop` returns nil and the `out` buffer contains `HELLO` (frames → stdout verbatim via `io.Copy` — `Stream.Read` yields only `FrameOutput` payloads, tolerates `FrameResized`, ignores `FrameTheme`/`FrameClear` by construction, client.go:332-352).
- `TestAttachLoopDetaches`: `in` = `bytes.NewReader([]byte{0x02, 0x64, 'z'})`; fakeDaemon holds the stream open; `attachLoop` returns nil without writing `'z'` through (assert the daemon side read no `FrameInput` before its EOF); the fake's connection sees EOF (the CLI closed the stream).
- `TestAttachLoopStopsOnStdinEnd`: `in` empty (immediate EOF), fakeDaemon streams forever; `attachLoop` returns (Review Focus 5).
- `TestAttachLoopClaimsAndSizes`: fakeDaemon records client→daemon frames; after connect it must see `FrameClaim` and `FrameResize` with payload `proto.ResizePixelsPayload(80, 24, 0, 0)` (the injected size; winch sends another `FrameResize` — feed the `winch` chan once and assert a second resize frame). Assert the Hello the fake read had `Raw: true`.
- `TestTermPixelsNonTerminal`: `termPixels` on a pipe fd (os.Pipe) returns an error (no winsize on a pipe) — pins the error path before any claim is sent.

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run 'TestAttach|TestTermPixels' -v`
Expected: FAIL — `attachCmd`/`attachLoop` undefined, usage lacks attach.

- [x] **Step 3: Implement**

cli.go registration, three small edits: usage block gets `kero attach [-t target]` after `kero new`'s block with a `-t, --target <target>` line ("which session or pane (default: the most recently active)"); `Commands` gains `"attach"`; the `run` switch (after `connect`/`state`, cli.go:343's group) gains `case "attach": return attachCmd(st, args, host)`.

attach.go `attachCmd`: parse flags `{"t": "target", "target": "target"}`; `if !term.IsTerminal(os.Stdin.Fd()) { return usageErr("kero attach needs a terminal") }`; `paneID, err := pickAttach(st, f.vals["target"])`; build the dialer — `host == ""` → `client.Local{}` wrapped as `func() (*client.Stream, error) { return client.AttachViaRaw(client.Local{}, paneID) }`, else `dialHost(host)` held for the command's life (its `Dial` opens a fresh stream per attempt) with dial errors wrapped `%w, errUnreachable` (exit 4, matching `run`'s connect error class, cli.go:324-333; one line, no special ErrVersion branch — the earlier control connection already caught version skew); `term.MakeRaw(os.Stdin.Fd())` + `defer term.Restore` (every exit path restores before the shell prompt returns, spec §CLI lifecycle 3); `attachLoop(dial, os.Stdin, os.Stdout, winchSignals(), func() (int, int, int, int, error) { return termPixels(int(os.Stdin.Fd())) })` under the deferred restore. Assemble from the spec's three precedents: connection + target resolution as `kero new` (cli.go:376-420), the long-run select as `kero events` (events.go:253), stream lifecycle as `kero __bridge` (remote.go:406-477).

attach.go `attachLoop` (single connection version — Task 8 adds the retry):

1. `st, err := dial()` → error returns it.
2. `st.Claim()`; `cols, rows, w, h, err := size()` — on error (a pipe, a lost tty) skip sizing, the daemon keeps its last size; else `st.ResizePixels(cols, rows, w, h)` (the claim is what arbitrates; Hello's Cols/Rows stay advisory, spec §CLI lifecycle 4).
3. One stdin goroutine, started once per connection but reading from the shared `in`: `Read` into 4 KB, `filter.feed(buf, time.Now())`, forward `out` bytes with `st.Write` (`FrameInput`), on `detach` close `st` and signal the loop to return nil, on read error signal return (stdin gone — Review Focus 5). On reconnect (Task 8) this goroutine ends with the dead stream and a fresh one starts against the new stream — the shared `in` is only ever read by one goroutine at a time.
4. Main `select`: stdin-detach → nil; stdin-error → nil; `winch` → `st.ResizePixels(size())` again; copy-done (`io.Copy(out, st)` in its own goroutine) → return nil (Task 8 refines EOF vs `st.Gone`); SIGINT arrives as a context/signal handled in `attachCmd` around the loop (cancel → return nil; same exit path as detach, spec §CLI lifecycle 6).

attach_unix.go / attach_windows.go: `termPixels` and `winchSignals` as in Interfaces, each with a one-line why comment (windows: `x/sys/unix` has no `TIOCGWINSZ`; SIGWINCH is unix-only, windows sizes ride the next resize ask).

- [x] **Step 4: Run tests and gates to verify they pass**

Run: `go test ./internal/cli/ -run 'TestAttach|TestTermPixels|TestPrefixFilter|TestPickAttach' -v && go test ./internal/cli/ && go vet ./internal/cli/`
Expected: PASS. Also `GOOS=windows go build ./...` (the platform split compiles).

---

### Task 8: Auto-reattach on clean EOF

**Files:**
- Modify: `internal/cli/attach.go` (`attachLoop`)
- Test: `internal/cli/attach_test.go` (extend)

**Interfaces:**
- Consumes: Task 7 `attachLoop` (single-connection), `Stream.Gone` (client.go:304-306, closed on `FrameClosed`).
- Produces: the retry envelope around Task 7's connection body — backoff 100 ms doubling, capped 3 s, unbounded attempts, reset to 100 ms after a successful attach; reconnect = fresh `dial()` → first frame is again the Mirror restart (the screen is rebuilt wholesale, never garbled); exit instead on stdout write error, stdin end, SIGINT, detach, or `Gone` (spec §CLI lifecycle 6, §Backpressure).

- [x] **Step 1: Write the failing tests**

Reusing Task 7's `fakeDaemon`:

- `TestAttachLoopReattaches`: two scripted connections — the first serves `Welcome` + `FrameOutput("ONE")` then closes the pipe (a clean drop with no `FrameClosed`); the second asserts its Hello again carries `Raw: true`, sees `FrameClaim` + `FrameResize` (re-claimed and re-sized on entry), then serves `FrameOutput("REDRAW")` and holds; `attachLoop` returns nil after a `detach` fed through `in`; `out` contains `ONE` and `REDRAW` (repainted from the new first frame).
- `TestAttachLoopExitsOnFrameClosed`: one connection serving `FrameOutput` then `FrameClosed` then close; `dial` fails the test if called a second time; `attachLoop` returns nil (Review Focus 4 — pane exit is an exit, not a reconnect).
- `TestAttachLoopMirrorFallback`: one connection answering `Welcome` **without** `raw` (old daemon) and streaming `FrameOutput` payloads; `out` receives them verbatim; `attachLoop` exits cleanly on `FrameClosed` (Review Focus 1 — degradation, no error path).
- `TestAttachLoopExitsOnStdoutError`: `out` a writer failing every write; connection streams a frame; `attachLoop` returns the write error without dialing again (stdout stopped being writable — SSH died).

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run 'TestAttachLoopReattaches|TestAttachLoopExitsOnFrameClosed|TestAttachLoopMirrorFallback|TestAttachLoopExitsOnStdoutError' -v`
Expected: FAIL — the single-connection loop returns on the first EOF (`TestAttachLoopReattaches`), and the others may pass vacuously; `Reattaches` is the driver.

- [x] **Step 3: Implement**

Wrap Task 7's per-connection body in `for { ... }` in `attachLoop`: on copy-done, `select { case <-st.Gone: return nil; default: }` — `Gone` means the pane ended (exit 0); otherwise, if the copy failed with a write error, return it; else the daemon dropped the connection (`out` overflow drop-kill, session.go:924-934) or the link died — `st.Close()`, sleep the backoff, `dial()` again (a dial error is not fatal here: same backoff, unbounded attempts), resetting the backoff once a connection's copy ran. Constants `reattachBase = 100 * time.Millisecond`, `reattachMax = 3 * time.Second`. Comment the contract: the daemon may kill a lagging raw attachment at any time; this loop is what makes that invisible (spec §Backpressure). The stdin goroutine of the dead stream returns with it; the new connection starts a fresh one against the shared `in`.

- [x] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestAttach|TestPrefixFilter|TestPickAttach' -v && go test ./internal/cli/`
Expected: PASS (the reconnect test runs its first retry at 100 ms — fast).

---

### Task 9: Daemon integration test + whole-plan gate + QA notes

**Files:**
- Test: `internal/daemon/attach_test.go` (extend — Task 3's tests are the unit layer; this adds the full-path walk)

**Interfaces:**
- Consumes: everything above.

- [x] **Step 1: Write the failing test, then make it pass with what exists**

`TestAttachRawEndToEnd` (extend Task 3's file): `testDaemon` → `d.create(CreateParams{Command: ["sh","-c","printf 'SCROLL-MARK\\n'; cat"]})` → poll `readText(true, 0)` for `SCROLL-MARK` → raw attach over `net.Pipe` via `d.serve` (the Task 3 helper) → assert, in order: `Welcome.Raw`, first `FrameOutput` contains `SCROLL-MARK` and the restart sequence, `FrameInput` `"ping\n"` then concatenated later `FrameOutput` payloads equal `"ping\r\nping\r\n"`, then close the client end of the pipe, attach **again** (new pipe, new Hello), and its first frame repaints `SCROLL-MARK` and everything since (the daemon-side half of reattach: any later attach gets the full state back). This test is expected to pass already if Tasks 3 and 7 are green — its failure is the finding; if it fails, the gap it exposed is the fix.

- [x] **Step 2: Run it**

Run: `go test ./internal/daemon/ -run TestAttachRawEndToEnd -v`
Expected: PASS.

- [x] **Step 3: Full gate**

Run: `go vet ./... && go test ./internal/...`
Expected: all PASS except the two pre-existing zsh failures (`TestCommandInForeground`, `TestCommandTellsExit`), which are out of scope (Global Constraints). Also `GOOS=windows go build ./...` once more.

- [x] **Step 4: Spec walkthrough against the diff**

Re-read `specs/2026-11-02-kero-attach-design.md` §Design line by line against `git diff`; every sentence maps to a change or an existing mechanism the spec names. Discrepancies → fix or record in the delivery report.

- [x] **Step 5: QA notes for the human**

Manual, on real hardware (CI cannot): phone terminal over SSH — scrollback by native gesture, `Ctrl+B` `d` detach, `kero new -s name`, resize on rotate (SIGWINCH), `cat` of a big file over a throttled link (drop → sub-second repaint), OSC query double-answer behavior with a GUI client attached (spec §Risks last row). List these in the delivery report.
