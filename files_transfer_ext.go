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
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// quoteRemotePath shell-quotes the path of a scp host:path side: the
// remote shell re-reads it — a space breaks it — so it goes out wrapped
// in double quotes with \, " and $ escaped inside. Local paths ride argv
// untouched (a picker's path never starts with "-", and scp takes no --);
// rsync formats its own remote side and is not quoted here.
func quoteRemotePath(p string) string {
	if p == "" {
		return ""
	}
	q := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`)
	return `"` + q.Replace(p) + `"`
}

// scpUploadArgs — scp [-o opts] [-P port] file… [user@]host:"dir"
// (scp passes -o through to its ssh; its port is -P, scp's -p keeps
// times; the user rides the target, scp has no -l user; the dir is
// quoteRemotePath'd).
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
	return append(args, userHost+":"+quoteRemotePath(dir))
}

// scpDownloadArgs — scp [-r when a folder] [-o opts] [-P port] [user@]host:"path" dest.
// No .git exclusion: scp has none; the built-in tar's skip
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
	return append(args, userHost+":"+quoteRemotePath(path), dest)
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

// -- rsync's own: the probe, the cache, the arc --

// probeRsync asks the host over ssh whether rsync is there — once per
// host per run: the answer cannot change between two transfers, and its
// ssh hop is a second connection every transfer would otherwise pay
// (decision 4). Only a yes is kept: a no may be rsync installed since.
// Read on the transfer's goroutine only: the one slot serializes the
// map's users.
func (a *App) probeRsync(h *host) bool {
	if a.rsyncOK[h.name] {
		return true
	}
	p, err := a.xferSpawn(remote.SSHCommand(), rsyncProbeArgs(h.addr))
	if err != nil {
		return false
	}
	// The version banner is small, far under a pipe's own room, so Wait
	// returns without a reader having drained it.
	if err := p.Wait(); err != nil {
		return false
	}
	a.rsyncOK[h.name] = true
	return true
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
	// word, not this one's.
	rsync := a.set.FilesTransfer == "rsync"
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
		// switch of method would hide the problem (decision 10).
		tool := "scp"
		if rsync {
			tool = "rsync"
		}
		if _, err := exec.LookPath(tool); err != nil {
			a.xferFailed(x, err)
			return
		}
		if rsync && !a.probeRsync(h) {
			// No rsync on the host: the same standing, in its own word —
			// no fallback, the built-in stream untouched (decision 10).
			a.update(func() {
				a.toast(locales.FilesRsyncAbsent(h.name))
				a.xferRelease(x)
			})
			return
		}
		args := scpUploadArgs(h.addr, files, dir)
		var prog func(string) // nil on scp: nothing it writes is machine-readable
		if rsync {
			args, prog = rsyncUploadArgs(h.addr, files, dir), a.rsyncArc(x, false)
		}
		a.update(func() { x.total = total })
		if err := a.xferRun(h, x, tool, args, prog); err != nil {
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
	// As the upload's: the mode read and held before the goroutine.
	rsync := a.set.FilesTransfer == "rsync"
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
		// (decision 10).
		tool := "scp"
		if rsync {
			tool = "rsync"
		}
		if _, err := exec.LookPath(tool); err != nil {
			a.xferFailed(x, err)
			return
		}
		if rsync && !a.probeRsync(h) {
			a.update(func() {
				a.toast(locales.FilesRsyncAbsent(h.name))
				a.xferRelease(x)
			})
			return
		}
		var prog func(string) // nil on scp, as the upload's
		if rsync {
			prog = a.rsyncArc(x, true)
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
			if err := a.xferRun(h, x, tool, args, prog); err != nil {
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
				return a.xferRun(h, x, tool, args, prog)
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
