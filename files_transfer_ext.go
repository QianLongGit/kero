package main

// The Files view's external transfers — a mode that borrows ssh's own
// tools instead of the daemon's stream. The scp and rsync command lines
// are pure functions of the host's remote.Addr, above, pinned by unit
// tests without a process; on top of them, below, the spawn layer and
// the flows that run those lines as the app's own transfers — the one
// slot, the ✕, the failure toast and the relist of the built-in stream,
// reused unchanged.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"kero/internal/proto"
	"kero/internal/remote"

	"kero/locales"
)

// xferSSHOpts are the never-ask options the bridge's own ssh line
// carries (sshArgs, internal/remote/transport.go), copied here rather
// than shared: opening internal/remote for the transfer would shift
// kerod's build fingerprint. scp and rsync carry the same six, none
// dropped — a transfer must not ask and must notice a dead link exactly
// as the bridge does. Keep in step with sshArgs: if transport.go's list
// ever changes, this follows by hand.
var xferSSHOpts = []string{"BatchMode=yes", "ConnectTimeout=15",
	"ServerAliveInterval=10", "ServerAliveCountMax=3",
	"ExitOnForwardFailure=yes", "ClearAllForwardings=yes"}

// sshOptionArgs — xferSSHOpts as bare -o pairs, the base every line
// starts from. scp stops here: its own port is -P and its user rides
// the target, so it takes no -p/-l of ssh's.
func sshOptionArgs() []string {
	args := make([]string, 0, len(xferSSHOpts)*2)
	for _, o := range xferSSHOpts {
		args = append(args, "-o", o)
	}
	return args
}

// xferSSHArgs — sshOptionArgs, then ["-p",port] when a.Port is set and
// ["-l",user] when a.User is, mirroring sshArgs's own conditionals
// (internal/remote/transport.go): the line rsync's -e and the probe
// ride.
func xferSSHArgs(a remote.Addr) []string {
	args := sshOptionArgs()
	if a.Port != "" {
		args = append(args, "-p", a.Port)
	}
	if a.User != "" {
		args = append(args, "-l", a.User)
	}
	return args
}

// scpUploadArgs — scp [-o opts] [-P port] file… [user@]host:dir
// (scp passes -o through to its ssh; its port is -P, scp's -p keeps
// times; the user rides the target, scp has no -l user). The dir rides
// bare, spaces included: OpenSSH 9.0+ scp is SFTP underneath, where the
// argv reaches the far side as it left this one — no shell re-reads it,
// so a quote would only land in the path as a literal character, and
// 2.3.37's quoting broke every path, spaced or not, on the legacy (-O)
// backend just the same.
func scpUploadArgs(a remote.Addr, files []string, dir string) []string {
	args := sshOptionArgs()
	if a.Port != "" {
		args = append(args, "-P", a.Port)
	}
	args = append(args, files...)
	userHost := a.Host
	if a.User != "" {
		userHost = a.User + "@" + a.Host
	}
	return append(args, userHost+":"+dir)
}

// scpDownloadArgs — scp [-r when a folder] [-o opts] [-P port] [user@]host:path dest.
// The remote side rides bare as the upload's own (scpUploadArgs). No
// .git exclusion: scp has none; the built-in tar's skip
// (internal/daemon/transfer.go's skippedByDownload) is a difference of
// the mode, documented here, not worked around.
func scpDownloadArgs(a remote.Addr, path string, isDir bool, dest string) []string {
	var args []string
	if isDir {
		args = append(args, "-r")
	}
	args = append(args, sshOptionArgs()...)
	if a.Port != "" {
		args = append(args, "-P", a.Port)
	}
	userHost := a.Host
	if a.User != "" {
		userHost = a.User + "@" + a.Host
	}
	return append(args, userHost+":"+path, dest)
}

// rsyncUploadArgs — rsync -a -s --info=progress2 -e "ssh …" file… [user@]host:dir
// -a overwrites in place: no dedup, as scp (the built-in upload's
// dedupName is the daemon's own word, internal/daemon/transfer.go,
// which no scp/rsync line passes through). -s (--protect-args) carries
// the arguments past the remote shell's re-reading, spaces in the dir
// included (decision 11).
func rsyncUploadArgs(a remote.Addr, files []string, dir string) []string {
	args := []string{"-a", "-s", "--info=progress2",
		"-e", "ssh " + strings.Join(xferSSHArgs(a), " ")}
	args = append(args, files...)
	userHost := a.Host
	if a.User != "" {
		userHost = a.User + "@" + a.Host
	}
	return append(args, userHost+":"+dir)
}

// rsyncDownloadArgs — rsync -a -s [--exclude=.git when a folder]
// --info=progress2 -e "ssh …" [user@]host:path dest; --exclude=.git
// aligns a folder download with the built-in tar's skip.
func rsyncDownloadArgs(a remote.Addr, path string, isDir bool, dest string) []string {
	args := []string{"-a", "-s"}
	if isDir {
		args = append(args, "--exclude=.git")
	}
	args = append(args, "--info=progress2", "-e", "ssh "+strings.Join(xferSSHArgs(a), " "))
	userHost := a.Host
	if a.User != "" {
		userHost = a.User + "@" + a.Host
	}
	return append(args, userHost+":"+path, dest)
}

// rsyncProbeArgs — for remote.SSHCommand(): the ssh line that runs
// "rsync --version" on the host, BatchMode keeping it a question no one
// has to answer. No -s: no path rides it.
func rsyncProbeArgs(a remote.Addr) []string {
	return append(xferSSHArgs(a), "--", a.Host, "rsync --version")
}

// rsyncVersionOK reads the first line of a rsync --version banner and
// says whether it is GNU rsync 3.1 or newer — the floor the transfer
// line stands on: -s arrived with 3.0, --info=progress2 with 3.1, and
// macOS ships an openrsync that answers --version happily yet takes
// neither flag. GNU's own banner is "rsync  version X.Y…" — two spaces
// before "version" — which openrsync's "openrsync X.Y…" never matches;
// anything else is read as no, for a version guessed wrong is a
// transfer that fails mid-air with flags the far side rejected.
func rsyncVersionOK(firstLine string) bool {
	const gnuBanner = "rsync  version " // GNU's own spelling: two spaces
	rest, ok := strings.CutPrefix(firstLine, gnuBanner)
	if !ok {
		return false
	}
	// The line is a far side's own stdout — the prefix alone, nothing
	// after it, must read as no, not crash the transfer's goroutine.
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return false
	}
	parts := strings.Split(fields[0], ".")
	if len(parts) < 2 { // "3" alone says too little
		return false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return major > 3 || (major == 3 && minor >= 1)
}

// xferBrewHomes lists the Homebrew homes a transfer tool lives in on
// darwin, which the find probes past LookPath: a GUI process's PATH is
// launchd's bare /usr/bin:/bin:/usr/sbin:/sbin, and a brew-installed
// tool — the GNU rsync macOS itself never ships, an OpenSSH past the
// system's — sits unseen in one of these. Elsewhere there is no such
// home; LookPath's word is final.
func xferBrewHomes(goos, tool string) []string {
	if goos != "darwin" {
		return nil
	}
	return []string{"/opt/homebrew/bin/" + tool, "/usr/local/bin/" + tool}
}

// xferCandidates lists where a transfer tool may live on this machine:
// LookPath's own word first, then darwin's brew homes (xferBrewHomes's
// own why), duplicates dropped — a PATH that holds a brew home would
// ask one path twice.
func xferCandidates(tool string) []string {
	var cands []string
	if p, err := exec.LookPath(tool); err == nil {
		cands = append(cands, p)
	}
	for _, p := range xferBrewHomes(runtime.GOOS, tool) {
		if !slices.Contains(cands, p) {
			cands = append(cands, p)
		}
	}
	return cands
}

// findScp answers scp's own path. scp has no --version to ask — and
// needs none: every scp speaks the line we run, its flags the old
// universal set — so existence is the whole question. LookPath's word
// is taken unasked, an executable file being its own definition,
// Windows included — where FileInfo.Mode carries no execute bit, and a
// bit test there would reject every candidate, LookPath's answer first
// of all; only the brew homes, paths no one vouched for, stand asked —
// and they exist on darwin alone, where the bit means what it says.
func findScp() (string, bool) {
	if p, err := exec.LookPath("scp"); err == nil {
		return p, true
	}
	for _, p := range xferBrewHomes(runtime.GOOS, "scp") {
		if st, err := os.Stat(p); err == nil && st.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// findRsync answers rsync's own path: the first candidate whose
// --version banner is GNU 3.1+ (rsyncVersionOK's own floor), asked of
// the candidate itself — rsyncLocalVersion — for the line's flags must
// run on this side before any hop.
func (a *App) findRsync() (string, bool) {
	for _, p := range xferCandidates("rsync") {
		if a.rsyncLocalVersion(p) {
			return p, true
		}
	}
	return "", false
}

// rsyncFrame matches one frame of rsync --info=progress2 as it rides
// stdout: the bytes done, comma-grouped by rsync's own hand, then the
// percent rsync itself figures. A frame it does not match is not an
// error — rsync writes other lines, stderr never carries frames, and
// the next frame is a keystroke away (decision 6).
var rsyncFrame = regexp.MustCompile(`^\s*([\d,]+)\s+(\d+)%`)

// parseRsyncProgress reads one frame of rsync --info=progress2 as it
// rides stdout: the bytes done, whole, and the percent rsync itself
// figures — 0 before it has one. The frame is rsync's own layout,
// ^\s*([\d,]+)\s+(\d+)%; a frame that does not parse is not an error —
// rsync writes other lines, stderr never carries frames, and the next
// frame is a keystroke away.
func parseRsyncProgress(frame string) (done int64, pct float64, ok bool) {
	m := rsyncFrame.FindStringSubmatch(frame)
	if m == nil {
		return 0, 0, false
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	p, err := strconv.ParseFloat(m[2], 64)
	if err != nil {
		return 0, 0, false
	}
	return n, p, true
}

// -- the spawn layer and the predicate --

// xferProc is a transfer's own process, as the app spawns it: the lines
// its stdout writes, why it failed on its stderr, and the two ends of
// it — Kill the ✕ takes, Wait the exit the flow takes.
type xferProc interface {
	Stdout() io.Reader
	Stderr() io.Reader
	Kill() error
	Wait() error
}

// osProc is an os/exec command as an xferProc: the pipes StdoutPipe and
// StderrPipe gave, both read to their end before Wait closes them.
type osProc struct {
	cmd    *exec.Cmd
	out    io.Reader
	errOut io.Reader
}

// defaultXferSpawn runs the command, pipes both streams, starts it.
func defaultXferSpawn(name string, args []string) (xferProc, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errOut, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &osProc{cmd: cmd, out: out, errOut: errOut}, nil
}

func (p *osProc) Stdout() io.Reader { return p.out }
func (p *osProc) Stderr() io.Reader { return p.errOut }
func (p *osProc) Kill() error       { return p.cmd.Process.Kill() }
func (p *osProc) Wait() error       { return p.cmd.Wait() }

// xferExtApplies says a transfer goes on a line of its own: the user chose
// scp or rsync, the host is ssh — scp and rsync speak no Windows path — and
// it is not Windows. Everything else, silently, is the built-in stream's.
func (a *App) xferExtApplies(h *host) bool {
	return (a.set.FilesTransfer == "scp" || a.set.FilesTransfer == "rsync") &&
		h.addr.Scheme == "ssh" && !h.windows()
}

// splitFrames cuts a stream where its frames end — \r or \n both, for
// rsync's progress rides \r without a tty; the tail with neither end
// still a frame.
func splitFrames(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// xferRun runs one command as the transfer: it spawns, arms the ✕ on Kill,
// reads stdout a frame at a time to progress (nil for none), and waits.
// The ✕'s mark on x reads cancelled at the end; any other wait failure
// carries stderr's last words.
func (a *App) xferRun(h *host, x *transferState, name string, args []string, progress func(frame string)) error {
	p, err := a.xferSpawn(name, args)
	if err != nil {
		return err
	}
	// The ✕ takes the process, the mark set first — xferCancel's own
	// shape (files_transfer.go) — for the signal error Wait then answers
	// is only the how of a cancelled end, not its word.
	a.update(func() {
		x.cancel = func() {
			x.err = errXferCancelled
			p.Kill()
		}
	})
	// Stderr drains beside the run, its last 4KB held: a failure's own
	// words sit at its end, and the answer must not hang on a process
	// that holds the pipe open. Read to the end before Wait, which
	// closes the pipes.
	tailc := make(chan string, 1)
	go func() {
		const tailMax = 4 << 10
		var tail []byte
		// The stream once, read to its end: Stderr hands the process's
		// own pipe, not a fresh view of it each call.
		r := p.Stderr()
		for {
			chunk := make([]byte, tailMax)
			n, rerr := r.Read(chunk)
			tail = append(tail, chunk[:n]...)
			if len(tail) > tailMax {
				copy(tail, tail[len(tail)-tailMax:])
				tail = tail[:tailMax]
			}
			if rerr != nil {
				tailc <- string(tail)
				return
			}
		}
	}()
	if progress != nil {
		sc := bufio.NewScanner(p.Stdout())
		sc.Split(splitFrames)
		for sc.Scan() {
			if fr := sc.Text(); fr != "" {
				progress(fr)
			}
		}
	}
	werr := p.Wait()
	tail := <-tailc
	// The verdict is read on the UI thread, where the ✕ leaves its mark:
	// reading x.err here would race the very click that writes it, a
	// transfer's fields being the UI thread's alone (transferState).
	errc := make(chan error, 1)
	a.update(func() {
		switch {
		case x.err != nil:
			errc <- errXferCancelled
		case werr != nil:
			errc <- fmt.Errorf("%v: %s", werr, tail)
		default:
			errc <- nil
		}
	})
	return <-errc
}

// -- rsync's own: the find, the probe, the cache, the arc --

// rsyncProbeResult — the probe's three answers, each with its own word
// in the UI: rsync 3.1+ on the host, no rsync there at all, or one too
// old to carry the line's flags.
type rsyncProbeResult int

const (
	rsyncRemoteOK     rsyncProbeResult = iota // GNU 3.1+ answered
	rsyncRemoteAbsent                         // no rsync there — the ssh line itself failed
	rsyncRemoteOld                            // answered, but below GNU 3.1
)

// rsyncLocalVersion asks one candidate's own banner — spawn it, read
// the first stdout line, then Wait, the order the pipes' own contract
// asks: the reader drains before Wait closes what it reads.
func (a *App) rsyncLocalVersion(path string) bool {
	p, err := a.xferSpawn(path, []string{"--version"})
	if err != nil {
		return false
	}
	line, _ := bufio.NewReader(p.Stdout()).ReadString('\n')
	return p.Wait() == nil && rsyncVersionOK(line)
}

// settleTool settles the transfer's tool on this machine — rsync by
// rsync, scp else. The setting's path is the user's own word, taken
// as it stands: never probed, never second-guessed — its failures are
// the user's to see (the flow wraps them in a word that says where to
// fix). Empty, the find answers for the mode (findScp, findRsync), and
// its answer is remembered in the setting — the user sees what runs
// their transfers, may change it, and the next transfer takes it
// without the find again.
func (a *App) settleTool(rsync bool, settingPath string) (string, bool) {
	if settingPath != "" {
		return settingPath, true
	}
	find := findScp
	if rsync {
		find = a.findRsync
	}
	p, ok := find()
	if ok {
		a.rememberXferTool(rsync, p)
	}
	return p, ok
}

// rememberXferTool writes a found path into the setting it belongs to,
// on the UI thread, only when the field is still empty: the setting is
// the find's own memory, saved as the settings window's own edits are
// (applySettings), so the user sees it, may change it — and a path
// written while the find ran, the user's own word, keeps its place.
func (a *App) rememberXferTool(rsync bool, path string) {
	a.update(func() {
		if rsync && a.set.FilesRsyncPath == "" {
			a.set.FilesRsyncPath = path
			a.applySettings(a.set)
		}
		if !rsync && a.set.FilesScpPath == "" {
			a.set.FilesScpPath = path
			a.applySettings(a.set)
		}
	})
}

// probeRsync asks the host over ssh what its rsync is — once per host
// per run: the answer cannot change between two transfers, and its
// ssh hop is a second connection every transfer would otherwise pay
// (decision 4). Only an OK is kept: an absent or old one may be rsync
// installed or updated since. Read on the transfer's goroutine only:
// the one slot serializes the map's users.
func (a *App) probeRsync(h *host) rsyncProbeResult {
	if a.rsyncOK[h.name] {
		return rsyncRemoteOK
	}
	p, err := a.xferSpawn(remote.SSHCommand(), rsyncProbeArgs(h.addr))
	if err != nil {
		return rsyncRemoteAbsent
	}
	// The banner's first line read before Wait, as the local find's
	// own — the pipe drains before Wait closes it — and the banner is
	// small, far under a pipe's own room, so the one line is all it
	// takes.
	line, _ := bufio.NewReader(p.Stdout()).ReadString('\n')
	if err := p.Wait(); err != nil {
		return rsyncRemoteAbsent
	}
	if !rsyncVersionOK(line) {
		return rsyncRemoteOld
	}
	a.rsyncOK[h.name] = true
	return rsyncRemoteOK
}

// rsyncArc reads rsync's own frames for the arc: its bytes fill done,
// and — a download alone, whose whole no stat could size — its percent
// backs the whole out. An upload's whole is the stat sum the flow set
// before the run; a percent never rewrites it (decision 6). Both only
// move forward: rsync's first frames restate their sums, and the arc
// must not run backwards. The updates hold to a tenth of a second —
// rsync refreshes near ten a second, the arc needs a tenth of that —
// with the final frame passing always, for an arc that never filled its
// end looks stalled there.
func (a *App) rsyncArc(x *transferState, download bool) func(frame string) {
	var last time.Time
	return func(frame string) {
		done, pct, ok := parseRsyncProgress(frame)
		if !ok || pct <= 0 {
			return
		}
		now := time.Now()
		if pct < 100 && now.Sub(last) < 100*time.Millisecond {
			return
		}
		last = now
		a.update(func() {
			x.done = max(x.done, done)
			if download {
				x.total = max(x.total, int64(float64(done)*100/pct))
			}
		})
	}
}

// uploadToExt runs the Upload Here… flow on a line of the host's own
// (see the file header): the skeleton — the claim, the picker, the
// toasts — is the built-in uploadTo's, line for line; the wire alone
// differs, one command of the chosen mode carrying every file at once,
// and the landing on dir itself: scp and rsync both overwrite
// same-named, no dedup of the daemon's (decision 5).
func (a *App) uploadToExt(h *host, dir string) {
	// The mode read here, on the UI thread the menu ran on, and held by
	// the goroutine: a setting switched mid-flow is the next transfer's
	// word, not this one's. The tool's path rides with it — the user's
	// own word, or the find's remembered answer.
	rsync := a.set.FilesTransfer == "rsync"
	toolPath := a.set.FilesScpPath
	if rsync {
		toolPath = a.set.FilesRsyncPath
	}
	x := &transferState{kind: xferUpload, name: proto.DirName(dir), host: h.name}
	a.side.files.xfer = x
	a.toast(locales.FilesUploadToast(h.name))
	go func() {
		files, err := a.xferPick(false, true)
		switch {
		case err != nil:
			a.xferFailed(x, err)
			return
		case len(files) == 0:
			a.update(func() { a.xferRelease(x) })
			return
		}
		// The arc's whole is the files' stat sum, set before the run:
		// scp writes nothing machine-readable, so done holds still and
		// the whole alone fills, while rsync's frames fill the done side
		// without ever rewriting this whole (decision 6).
		var total int64
		for _, f := range files {
			st, err := os.Stat(f)
			if err != nil {
				a.xferFailed(x, err)
				return
			}
			total += st.Size()
		}
		// The tool of the mode, missing on this machine: this transfer's
		// end, not a fallback — the user chose the tool, and a silent
		// switch of method would hide the problem (decision 10). The
		// setting's path is the user's own word; empty, the find answers
		// and remembers (settleTool's own why).
		tool, ok := a.settleTool(rsync, toolPath)
		if !ok {
			if rsync {
				// A rsync too old for the line's flags — macOS's
				// openrsync — is as absent as none at all, in its own
				// word.
				a.update(func() {
					a.toast(locales.FilesRsyncLocalOld())
					a.xferRelease(x)
				})
			} else {
				a.xferFailed(x, errors.New(locales.FilesToolMissing("scp")))
			}
			return
		}
		if rsync {
			switch a.probeRsync(h) {
			case rsyncRemoteAbsent:
				// No rsync on the host: the same standing, in its own
				// word — no fallback, the built-in stream untouched
				// (decision 10).
				a.update(func() {
					a.toast(locales.FilesRsyncAbsent(h.name))
					a.xferRelease(x)
				})
				return
			case rsyncRemoteOld:
				// There, but below the floor the flags stand on: its
				// own word too, for the same standing.
				a.update(func() {
					a.toast(locales.FilesRsyncRemoteOld(h.name))
					a.xferRelease(x)
				})
				return
			}
		}
		args := scpUploadArgs(h.addr, files, dir)
		var prog func(string) // nil on scp: nothing it writes is machine-readable
		if rsync {
			args, prog = rsyncUploadArgs(h.addr, files, dir), a.rsyncArc(x, false)
		}
		a.update(func() { x.total = total })
		if err := a.xferRun(h, x, tool, args, prog); err != nil {
			// A hand-set path that would not run says so in a word that
			// says where to fix it; the find's own answer has earned no
			// such word.
			if toolPath != "" && err != errXferCancelled {
				err = errors.New(locales.FilesToolPathBad(toolPath, err.Error()))
			}
			a.xferFailed(x, err)
			return
		}
		a.update(func() {
			a.xferRelease(x)
			a.toast(locales.FilesUploadDone(len(files), h.name))
			// The folder that took them lists again, as the built-in's
			// own landing does.
			a.relistDir(h, dir)
		})
	}()
}

// downloadFromExt runs the Download… flow on a line of the host's own:
// as the upload's, the skeleton is the built-in downloadFrom's — the
// Save dialog, the folder picker, the temp-and-rename of a file, the
// paste-name landing of a folder — with the wire one command of the
// chosen mode. scp cannot size what it fetches: the total stays 0 and
// the arc spins; rsync's own percent backs the whole out (decision 6).
func (a *App) downloadFromExt(h *host, path string, isDir bool) {
	// As the upload's: the mode read and held before the goroutine, the
	// tool's path riding with it.
	rsync := a.set.FilesTransfer == "rsync"
	toolPath := a.set.FilesScpPath
	if rsync {
		toolPath = a.set.FilesRsyncPath
	}
	name := proto.DirName(path)
	x := &transferState{kind: xferDownload, name: name, host: h.name}
	a.side.files.xfer = x
	a.toast(locales.FilesDownloadToast(name))
	go func() {
		dest, file := "", name
		if isDir {
			dirs, err := a.xferPick(true, false)
			switch {
			case err != nil:
				a.xferFailed(x, err)
				return
			case len(dirs) == 0:
				a.update(func() { a.xferRelease(x) })
				return
			}
			dest = dirs[0]
		} else {
			p, err := a.xferSave(name)
			switch {
			case err != nil:
				a.xferFailed(x, err)
				return
			case p == "":
				a.update(func() { a.xferRelease(x) })
				return
			}
			dest, file = filepath.Dir(p), filepath.Base(p)
		}
		// As the upload's: the tool's absence is this transfer's end
		// (decision 10), the setting's path the user's own word, and
		// the find's answer remembered (settleTool's own why).
		tool, ok := a.settleTool(rsync, toolPath)
		if !ok {
			if rsync {
				a.update(func() {
					a.toast(locales.FilesRsyncLocalOld())
					a.xferRelease(x)
				})
			} else {
				a.xferFailed(x, errors.New(locales.FilesToolMissing("scp")))
			}
			return
		}
		if rsync {
			switch a.probeRsync(h) {
			case rsyncRemoteAbsent:
				a.update(func() {
					a.toast(locales.FilesRsyncAbsent(h.name))
					a.xferRelease(x)
				})
				return
			case rsyncRemoteOld:
				a.update(func() {
					a.toast(locales.FilesRsyncRemoteOld(h.name))
					a.xferRelease(x)
				})
				return
			}
		}
		var prog func(string) // nil on scp, as the upload's
		if rsync {
			prog = a.rsyncArc(x, true)
		}
		// As the upload's wrap: a hand-set path that would not run says
		// so in a word that says where to fix it — both ends of the
		// flow's one wire share it.
		run := func(args []string, prog func(string)) error {
			err := a.xferRun(h, x, tool, args, prog)
			if err != nil && toolPath != "" && err != errXferCancelled {
				err = errors.New(locales.FilesToolPathBad(toolPath, err.Error()))
			}
			return err
		}
		var landed string
		if isDir {
			// A folder lands beside one of its own name as a paste does,
			// "name copy" — the built-in's own rule.
			root := dirDestName(dest, name)
			args := scpDownloadArgs(h.addr, path, true, root)
			if rsync {
				args = rsyncDownloadArgs(h.addr, path, true, root)
			}
			if err := run(args, prog); err != nil {
				a.xferFailed(x, err)
				return
			}
			landed = root
		} else {
			var err error
			landed, err = downloadViaTemp(dest, file, func(tmp string) error {
				args := scpDownloadArgs(h.addr, path, false, tmp)
				if rsync {
					args = rsyncDownloadArgs(h.addr, path, false, tmp)
				}
				return run(args, prog)
			})
			if err != nil {
				a.xferFailed(x, err)
				return
			}
		}
		a.update(func() {
			a.xferRelease(x)
			a.toast(locales.FilesDownloadDone(proto.DirName(landed)))
		})
	}()
}
