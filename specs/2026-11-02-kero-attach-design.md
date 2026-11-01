# Kero Attach — Phone-in, Direct Session Attach — Design

Date: 2026-11-02
Status: approach C (raw pass-through) selected; pending implementation

## Intent and why

The user drives remote hosts from a phone over SSH. Today the loop is:
phone terminal → `tmux attach` → inside that tmux, panes the kerod daemon
owns. The tmux layer exists only as a relay — the phone needs kero's
sessions, and kero already owns the PTYs, so the extra multiplexer is a
detour that breaks the moment the two disagree about what is attached.

What the user asked for, in their words: keep the command grammar and feel
of tmux, because decades of muscle memory live there. Concretely, after
convergence, four operations cover everything the phone ever does:

- **list** — see which sessions exist (`kero ls`)
- **attach** — enter a session (`kero attach [-t target]`; bare `kero attach`
  enters the most recently active one)
- **new** — create a session (`kero new [-s name]`)
- **detach** — leave it (`Ctrl+B` `d`, exactly tmux's prefix chord)

Split panes, window management, copy-mode and the rest stay on the desktop
GUI, as today — the phone never used them. One hard requirement beyond the
four: **scrollback must work by scrolling** — the phone terminal's native
gesture, like the user's `mouse on` tmux setup — with the daemon-side
history (scrollback capacity) doing the retention.

## Non-goals

- No pane splits, window tree, or copy-mode on the CLI — GUI keeps those.
- No second multiplexer inside kero: no prefix command system beyond
  `Ctrl+B` `d`. One chord, one meaning.
- No protocol version bump — this rides optional fields only.
- No OSC 52 / extended-keys / focus-events machinery on our side: raw
  pass-through hands the phone terminal whatever the program emits; the
  phone terminal is the renderer and decides (this is the point of C).

## Approaches considered

**A — keep tmux as the phone's relay, kero wraps it.** Formalize what the
user does by hand: kero manages a tmux session per kero session. Rejected:
two owners of the same terminal state, and the ghost-attach problem the
lazy-restore design just spent effort killing comes back through the back
door. Every kero feature (agent detection, restore, events) would need a
tmux-shaped adapter.

**B — Mirror stream, CLI-side re-rendering.** The GUI's protocol: the
daemon sends render diffs; the client replays them into a local terminal
emulator. For a CLI this means shipping a terminal emulator (ghostty vt or
an ANSI diff-to-absolute-sequence translator) inside `kero attach`, then
painting the phone's terminal from it. Rejected: doubles the rendering
stack — daemon renders, CLI un-renders — and every terminal-mode feature
(2026 sync updates, CSI u, focus events) must be understood by the middle
layer. The Mirror stream's drop-then-reattach semantics also assume a
client that can rebuild from a fresh full frame, which B's translator would
have to reimplement.

**C — raw pass-through (selected).** The daemon attaches the CLI to the
PTY's raw byte stream: one full-state first frame, then original bytes in
both directions. The phone terminal is the terminal emulator. Chosen
because: pass-through is feature-complete by construction (OSC 52, CSI u,
focus events, bracketed paste — anything the program emits arrives
unmangled); the first frame reuses the Mirror restart path, so the
"replay the screen as ANSI" machinery is zero new code; the CLI has no
rendering to do, only byte plumbing.

## Design

### Commands

| tmux habit | kero form | Status |
|---|---|---|
| `tmux ls` | `kero ls` | exists, unchanged |
| `tmux attach` / `tmux attach -t dev` | `kero attach` / `kero attach -t dev` | **new** |
| `tmux new -s dev` | `kero new -s dev` | existing command, `-s` added as an alias of `-n/--name` |
| prefix `Ctrl+B` then `d` | same chord, intercepted client-side | **new** |

`-t` target resolution reuses the CLI's existing `find()` (`internal/cli/cli.go:841`):
session id, unique id prefix, session name, display name, pane id — the same
semantics `kero send` and friends already use, including the ambiguous-match
error that lists candidates (exit 3). The resolved session's focus pane
(`Session.Focus`, `internal/proto/proto.go:321`) is what the attach protocol
wants; `findPane` on the daemon side is untouched.

Bare `kero attach` picks the most recently active session: max over
`Activity > 0 ? Activity : Since > 0 ? Since : Created` — the GUI's
`updatedKey` chain (`order.go:122-130`) — then that session's focus pane.
Stable tie-break by id. `order.go` lives in the root package, which imports
`internal/cli`; the three-line comparison is therefore copied into
`internal/cli`, not imported — the reverse import would be a cycle. This is
CLI-side selection over the state list; no daemon change.

`kero new -s name` is a parser alias only: `-s` joins the valued-flag map
pointing at the same canonical `name` flag `kero new -n` uses today
(`internal/cli/cli.go:41`, parsed at `cli.go:376-420`). `CreateParams.Name`
and the daemon's trim-and-store behavior (`internal/daemon/sessions.go:359`)
are untouched.

The attach connection itself goes through a sibling of `AttachVia`:
`AttachViaRaw(d, id)` in `internal/client/client.go`, identical except the
Hello carries `Raw: true` (today `AttachVia` hardcodes its Hello at
`client.go:324` — a parallel function is the minimal change; no opts
plumbing). The returned Stream records `Welcome.Raw` so the CLI knows which
mode it actually got.

### Protocol

One new optional field on an existing Hello, one on Welcome. No new frame
kinds, no `proto.Version` bump — exactly the growth pattern the constant's
own comment licenses (`internal/proto/proto.go:27`), and the same shape as
the five optional fields Hello already carries.

```
Hello:   Raw bool `json:"raw,omitempty"`
Welcome: Raw bool `json:"raw,omitempty"`   // echo: the daemon honored it
```

- New CLI → new daemon: Hello carries `raw:true`; `serveAttach` takes the
  raw branch and Welcome echoes `raw:true`.
- New CLI → **old** daemon (same Version, doesn't know the field): the JSON
  field is skipped, the daemon serves the ordinary Mirror stream — and a
  Mirror `FrameOutput` payload is itself renderable ANSI (it is what the
  GUI's terminal consumes), so the CLI can still write frames to stdout and
  be readable, minus raw guarantees. Natural degradation, no error path.
- Old CLI → new daemon: no `raw` field, ordinary Mirror attach. Nothing to do.

Why not a new Hello Kind: unknown kinds fall into the control-connection
default in `serveHello` (`internal/daemon/daemon.go:710-722`) — a raw attach
masquerading as control is the dangerous failure mode; a boolean on the
known `attach` kind cannot misroute.

### Daemon side

All inside the existing attach machinery (`internal/daemon/session.go`,
`internal/daemon/daemon.go:792-843`):

1. **First frame.** In `s.attach`, under the pane lock, a raw attachment
   still gets a `Mirror` and one `Frame()` call — the restart path emits
   the full state as ANSI: scrollback from line 0, screen, cursor, SGR,
   keyboard/mouse modes, title, palette (`terminal/mirror.go:267-293`,
   `mirror.go:680-756`). That frame is the first thing on the wire. Zero
   new serialization code; the alt-screen caveat matches the GUI (a
   running vim replays its current screen — the same thing the GUI shows
   on connect).
2. **Raw tee.** The attachment then stops participating in the per-client
   `frame()` diff loop. Instead `output()` (`session.go:209-233`), already
   holding the pane lock, appends the raw bytes to the raw attachment's
   outgoing queue right after `term.Feed` — same lock, so first-frame
   snapshot → subsequent raw bytes have no gap and no reorder. Bytes go
   out under the existing frame-splitting (`AppendFrames`, 1 MB per
   output frame), the existing write loop with its 1s write timeout, and
   the existing `out` channel.
3. **Input, resize, claim, close.** Unchanged paths: `FrameInput` up,
   `FrameResize`/`FrameClaim` both honored (the CLI claims on entry, so
   the phone's size wins — same semantics as the GUI's
   window-size-latest), `FrameClosed` on pane exit, detach simply closes
   the connection and the daemon's existing `fit()` re-derives the size
   once the claiming client is gone.

### CLI side

New file `internal/cli/attach.go`. Skeleton assembled from three existing
precedents: connection + target resolution as in `kero new`
(`internal/cli/cli.go:376-420`), a `select` long-run loop as in
`kero events` (`internal/cli/events.go:253`), stdio stream lifecycle as in
`kero __bridge` (`internal/cli/remote.go:406-477`). `golang.org/x/term` is
already a dependency (`term.IsTerminal` in `cli.go:21`).

Lifecycle:

1. Resolve target (`-t` via `find()`, else most-recently-active), refuse to
   run when stdin is not a terminal (`term.IsTerminal`) — a piped attach
   has no renderer and no size, exit 2 with a message.
2. Connect (`AttachVia`, `internal/client/client.go:319`), sending
   `Raw: true`. Save `Welcome.Raw` as the mode marker.
3. Enter raw mode (`term.MakeRaw` on stdin's fd), `defer` restore. On every
   exit path — detach, pane exit, fatal error, SIGINT while attached — the
   deferred restore runs first; the phone's shell prompt must come back
   clean.
4. Claim and size: send `FrameClaim`, then `FrameResize` with the current
   terminal size; on `SIGWINCH` re-send `FrameResize`. The payload must be
   pixel-form — the daemon's attach loop parses only
   `ParseResizePixels(cols, rows, cellW, cellH)` (`internal/daemon/daemon.go:828`,
   `internal/proto/frame.go:167`) — so the size comes from an ioctl
   `TIOCGWINSZ` (x/term has no pixel API). Over SSH the pixel fields are
   commonly zero; programs that query cell-pixel geometry get 0, which no
   CLI tool meaningfully uses. Hello's Cols/Rows stay advisory as today;
   the claim is what arbitrates.
5. Main loop, two goroutines:
   - **stdin → FrameInput**, with the prefix interceptor inline. Its
     semantics, byte-exact: on byte `0x02` (Ctrl+B), swallow it and wait
     for the next byte inside a ~500 ms window (tmux's repeat-time). That
     next byte decides: `0x64` (`d`) closes the connection cleanly
     (detach); `0x1b` (ESC — an escape sequence or bracketed-paste block
     is starting) discards it **and keeps discarding until 10 ms of input
     silence** (tmux's escape-time), so an arrow key or paste after the
     prefix is swallowed whole, not torn apart into a stray `[A` reaching
     the program; any other byte is discarded alone. Window expiry
     discards only the prefix. Ctrl+B-alone (readline's back-char) being
     eaten matches the tmux experience the user asked to keep. A laggy
     phone typing Ctrl+B then `d` more than 500 ms apart gets both bytes
     forwarded as ordinary input — visible, recoverable, and rare; chosen
     over a longer window that would make every eaten chord feel
     sluggish.
   - **frames → stdout**: `FrameOutput` payloads written verbatim;
     `FrameResized` tolerated (the phone owns the size, but a shrinking
     pane shouldn't crash the loop); `FrameTheme`/`FrameClear` ignored —
     the CLI never sends either, so the daemon-side emulator answers the
     program's theme queries with its default palette (a GUI-attached
     pane gets the real theme; the mismatch is cosmetic and local to the
     daemon's answer, while the phone terminal answers its own copy of
     the query through the pass-through); `FrameClosed` exits 0.
6. **Auto-reattach (hard requirement, not an enhancement).** The daemon's
   backpressure is drop-the-connection (`out` full → `finish()`,
   `session.go:924-934`); a Mirror client recovers by design on the next
   full frame, but a raw client's stream is dead bytes — so the CLI treats
   a clean EOF as *reattach*: reconnect with backoff (100 ms doubling,
   capped 3 s, unbounded attempts), whose first frame is again the full
   Mirror restart — the screen is rebuilt wholesale, never garbled.
   Exit instead when stdout stops being writable (SSH died) or SIGINT
   fires — same exit paths as detach. The GUI survives the same drops
   only by a human re-clicking the pane; the CLI automates that one move.

### Backpressure, spelled out

The reuse is deliberate: same `out` channel, same drop-kill. A slower,
dedicated raw queue would only delay the same cliff while adding a second
backpressure mechanism to reason about. The contract becomes: **the daemon
may kill a lagging raw attachment at any time; the CLI's reattach-on-EOF
makes that invisible** (at worst a sub-second blank while the first frame
redraws). Weak-phone-network plus huge-output (the realistic trigger:
`cat` of a big file over flaky cellular) costs a redraw, not a session, not
garbage.

## Testing

- **Daemon, unit** (`internal/daemon/`): Hello with `Raw:true` yields a
  first frame that begins like a Mirror restart (contains the clear+home
  sequence and scrollback content) and subsequent `FrameOutput`s that are
  the verbatim program bytes, not diffs; the Welcome echoes `Raw:true`
  back, and a non-raw Hello gets no echo; a raw attachment does not disturb
  a concurrent Mirror attachment's diff stream; `out`-overflow still closes
  the raw connection.
- **CLI, unit** (`internal/cli/`): `-t` resolution and bare-attach default
  selection (most-recently-active wins, tie-break stable); prefix
  interceptor as a pure byte-stream function — `0x02 0x64` → detach and
  nothing forwarded, `0x02 'x'` → nothing forwarded, timeout → nothing,
  ordinary bytes pass through untouched.
- **Integration**: real daemon, `kero new`, program output before attach
  shows up in the first frame (scrollback replayed); bytes typed after
  attach reach the pane; killing the consumer connection mid-stream makes
  the CLI reconnect and repaint.

## Risks

- **Drop-trigger timing is analysis, not measurement** (8192-frame buffer
  at 4 ms coalescing ≈ tens of seconds of full stall, or fewer huge
  frames). The reattach path makes the exact number non-critical; if field
  use shows frequent reconnects, the mitigation is a bigger raw queue — a
  constant, not a redesign.
- **Old-daemon degradation is best-effort**: Mirror diffs written verbatim
  render readably (they are self-locating ANSI) but without raw guarantees.
  Accepted: the fleet updates the daemon and CLI together, and a version
  note ships with the release.
- **Ctrl+B chord vs. agents' keybinds**: identical to tmux's behavior,
  which is the explicitly requested experience. Claude Code users under
  tmux already live with the eaten Ctrl+B.
- **`newID` has no uniqueness re-check** (observed, `daemon.go:360-368`):
  pre-existing, unchanged by this design; 33⁷ space (33-char alphabet),
  collisions negligible.
- **OSC query double-answer**: with raw pass-through, a program's theme/
  capability query may be answered both by the daemon-side emulator and,
  through the pipe, by the phone terminal. Most consumers take the first
  answer and stop; verify on real hardware during acceptance, and if it
  bites, the fix is daemon-side suppression of the emulator's reply for
  raw attachments — a contained change.

## Deferred

- Scrollback size trimming for the first frame (`readLines`,
  `terminal/mirror.go:302-334`, could cap replay to N lines) — only if
  phone reattach feels slow on 100k-line histories.
- A "lagging, about to drop" warning frame before the kill — needs a new
  frame kind; reattach already makes the kill invisible.
- `Welcome.Raw` absent on old daemons could print a one-line notice; chose
  silence (the degraded stream still works; noise on every old-daemon
  attach would be worse).
