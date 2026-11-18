package main

// The command lines of the external transfers, word for word: the argv
// scp and rsync build for a host's remote.Addr, pinned exactly — a flag
// misplaced is a transfer that asks the wrong question or reaches the
// wrong side, and no process needs to run to tell. Below them, the
// spawn layer and the scp flows, on a scripted process: the argv the
// spawn saw, the state of the run mid-flight, the toast and the slot at
// its every end.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"kero/internal/proto"
	"kero/internal/remote"
	"kero/internal/settings"

	"kero/locales"
)

// xferSSHOpts is a copy of the bridge's own never-ask list, and this
// pins the copy against the source it must not drift from (sshArgs,
// internal/remote/transport.go) — plus the property rsync's -e word
// splitting stands on: no value carries a space.
func TestXferSSHOpts(t *testing.T) {
	want := []string{"BatchMode=yes", "ConnectTimeout=15",
		"ServerAliveInterval=10", "ServerAliveCountMax=3",
		"ExitOnForwardFailure=yes", "ClearAllForwardings=yes"}
	if !slices.Equal(xferSSHOpts, want) {
		t.Errorf("xferSSHOpts = %q, want the sshArgs snapshot %q", xferSSHOpts, want)
	}
	for _, o := range xferSSHOpts {
		if strings.ContainsFunc(o, func(r rune) bool { return r == ' ' || r == '\t' }) {
			t.Errorf("xferSSHOpts: %q carries a space; rsync word-splits its -e itself", o)
		}
	}
}

func TestXferSSHArgs(t *testing.T) {
	opts := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=10",
		"-o", "ServerAliveCountMax=3", "-o", "ExitOnForwardFailure=yes", "-o", "ClearAllForwardings=yes"}
	for _, c := range []struct {
		addr remote.Addr
		tail []string
	}{
		{remote.Addr{Scheme: "ssh", Host: "srv"}, nil},
		{remote.Addr{Scheme: "ssh", Host: "srv", Port: "2222"}, []string{"-p", "2222"}},
		{remote.Addr{Scheme: "ssh", User: "me", Host: "srv"}, []string{"-l", "me"}},
		{remote.Addr{Scheme: "ssh", User: "me", Host: "srv", Port: "2222"}, []string{"-p", "2222", "-l", "me"}},
	} {
		want := slices.Concat(opts, c.tail)
		if got := xferSSHArgs(c.addr); !slices.Equal(got, want) {
			t.Errorf("xferSSHArgs(%+v) = %q, want %q", c.addr, got, want)
		}
	}
}

// The "no copy" pin is the overwrite rule's own (decision 5): an ext
// upload lands on dir itself — the daemon's dedupName
// (internal/daemon/transfer.go) is the built-in stream's word, which no
// scp line passes through.
func TestScpUploadArgs(t *testing.T) {
	for _, c := range []struct {
		name  string
		addr  remote.Addr
		files []string
		dir   string
		want  []string
	}{
		{
			name:  "user and port",
			addr:  remote.Addr{Scheme: "ssh", User: "me", Host: "srv", Port: "2222"},
			files: []string{"/tmp/a.txt", "/tmp/b.txt"},
			dir:   "/srv/up",
			want: []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=10",
				"-o", "ServerAliveCountMax=3", "-o", "ExitOnForwardFailure=yes", "-o", "ClearAllForwardings=yes",
				"-P", "2222", "/tmp/a.txt", "/tmp/b.txt", "me@srv:/srv/up"},
		},
		{
			name:  "bare host, spaced dir",
			addr:  remote.Addr{Scheme: "ssh", Host: "srv"},
			files: []string{"/tmp/a"},
			dir:   "/srv/my files",
			want: []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=10",
				"-o", "ServerAliveCountMax=3", "-o", "ExitOnForwardFailure=yes", "-o", "ClearAllForwardings=yes",
				"/tmp/a", "srv:/srv/my files"},
		},
	} {
		got := scpUploadArgs(c.addr, c.files, c.dir)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: scpUploadArgs = %q, want %q", c.name, got, c.want)
		}
		for _, arg := range got {
			if strings.Contains(arg, " copy") {
				t.Errorf("%s: argv carries a copy name %q; an ext upload overwrites", c.name, arg)
			}
		}
	}
}

func TestScpDownloadArgs(t *testing.T) {
	for _, c := range []struct {
		name  string
		addr  remote.Addr
		path  string
		isDir bool
		dest  string
		want  []string
	}{
		{
			name:  "folder",
			addr:  remote.Addr{Scheme: "ssh", User: "me", Host: "srv", Port: "2222"},
			path:  "/srv/my proj",
			isDir: true,
			dest:  "/dest/my proj copy",
			want: []string{"-r",
				"-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=10",
				"-o", "ServerAliveCountMax=3", "-o", "ExitOnForwardFailure=yes", "-o", "ClearAllForwardings=yes",
				"-P", "2222", "me@srv:/srv/my proj", "/dest/my proj copy"},
		},
		{
			name: "file, bare host",
			addr: remote.Addr{Scheme: "ssh", Host: "srv"},
			path: "/srv/a.txt",
			dest: "/dest/a.txt",
			want: []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=10",
				"-o", "ServerAliveCountMax=3", "-o", "ExitOnForwardFailure=yes", "-o", "ClearAllForwardings=yes",
				"srv:/srv/a.txt", "/dest/a.txt"},
		},
	} {
		got := scpDownloadArgs(c.addr, c.path, c.isDir, c.dest)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: scpDownloadArgs = %q, want %q", c.name, got, c.want)
		}
	}
}

// rsyncUploadArgs: -a overwrites in place and -s carries the dir's
// spaces past the remote shell's re-reading (decision 11), so the dir
// rides unquoted; the -e value is one string rsync word-splits itself —
// the port rides it as -p, ssh's own, where scp's own is -P.
func TestRsyncUploadArgs(t *testing.T) {
	for _, c := range []struct {
		name  string
		addr  remote.Addr
		files []string
		dir   string
		want  []string
	}{
		{
			name:  "user, port, spaced dir",
			addr:  remote.Addr{Scheme: "ssh", User: "me", Host: "srv", Port: "2222"},
			files: []string{"/tmp/a.txt", "/tmp/b.txt"},
			dir:   "/srv/my files",
			want: []string{"-a", "-s", "--info=progress2",
				"-e", "ssh -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=10" +
					" -o ServerAliveCountMax=3 -o ExitOnForwardFailure=yes -o ClearAllForwardings=yes -p 2222 -l me",
				"/tmp/a.txt", "/tmp/b.txt", "me@srv:/srv/my files"},
		},
		{
			name:  "bare host",
			addr:  remote.Addr{Scheme: "ssh", Host: "srv"},
			files: []string{"/tmp/a"},
			dir:   "/srv/up",
			want: []string{"-a", "-s", "--info=progress2",
				"-e", "ssh -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=10" +
					" -o ServerAliveCountMax=3 -o ExitOnForwardFailure=yes -o ClearAllForwardings=yes",
				"/tmp/a", "srv:/srv/up"},
		},
	} {
		got := rsyncUploadArgs(c.addr, c.files, c.dir)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: rsyncUploadArgs = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRsyncDownloadArgs(t *testing.T) {
	for _, c := range []struct {
		name  string
		addr  remote.Addr
		path  string
		isDir bool
		dest  string
		want  []string
	}{
		{
			name:  "folder excludes .git",
			addr:  remote.Addr{Scheme: "ssh", User: "me", Host: "srv", Port: "2222"},
			path:  "/srv/my proj",
			isDir: true,
			dest:  "/dest",
			want: []string{"-a", "-s", "--exclude=.git", "--info=progress2",
				"-e", "ssh -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=10" +
					" -o ServerAliveCountMax=3 -o ExitOnForwardFailure=yes -o ClearAllForwardings=yes -p 2222 -l me",
				"me@srv:/srv/my proj", "/dest"},
		},
		{
			name: "file",
			addr: remote.Addr{Scheme: "ssh", Host: "srv"},
			path: "/srv/a.txt",
			dest: "/dest/a.txt",
			want: []string{"-a", "-s", "--info=progress2",
				"-e", "ssh -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=10" +
					" -o ServerAliveCountMax=3 -o ExitOnForwardFailure=yes -o ClearAllForwardings=yes",
				"srv:/srv/a.txt", "/dest/a.txt"},
		},
	} {
		got := rsyncDownloadArgs(c.addr, c.path, c.isDir, c.dest)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: rsyncDownloadArgs = %q, want %q", c.name, got, c.want)
		}
		if c.isDir && !slices.Contains(got, "--exclude=.git") {
			t.Errorf("%s: a folder download misses --exclude=.git", c.name)
		}
		if !c.isDir && slices.Contains(got, "--exclude=.git") {
			t.Errorf("%s: a file download carries --exclude=.git", c.name)
		}
	}
}

// The probe rides the bridge's own never-ask ssh line — BatchMode keeps
// it a question no one has to answer — and carries no path, so it takes
// no -s (decision 11).
func TestRsyncProbeArgs(t *testing.T) {
	got := rsyncProbeArgs(remote.Addr{Scheme: "ssh", User: "me", Host: "srv", Port: "2222"})
	want := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=10",
		"-o", "ServerAliveCountMax=3", "-o", "ExitOnForwardFailure=yes", "-o", "ClearAllForwardings=yes",
		"-p", "2222", "-l", "me", "--", "srv", "rsync --version"}
	if !slices.Equal(got, want) {
		t.Errorf("rsyncProbeArgs = %q, want %q", got, want)
	}
	if slices.Contains(got, "-s") {
		t.Errorf("probe carries -s; no path rides it")
	}
}

// rsyncVersionOK reads the first line of a rsync --version banner and
// says whether it is GNU rsync 3.1+ — the floor the line stands on:
// -s arrived with 3.0 and --info=progress2 with 3.1, and macOS ships an
// openrsync (and once a GNU 2.6.9) that answers --version happily and
// takes neither flag. GNU's own banner is "rsync  version X.Y…" — two
// spaces, the word version — which openrsync's "openrsync X.Y…" never
// matches; a single-space "rsync version" is no GNU banner either.
func TestRsyncVersionOK(t *testing.T) {
	for _, c := range []struct {
		line string
		want bool
	}{
		{"rsync  version 3.1  protocol version 30", true},
		{"rsync  version 3.2.7  protocol version 31", true},
		{"rsync  version 3.4.1  protocol version 31\n", true},
		{"rsync  version 4.0.0  protocol version 32", true},
		{"rsync  version 2.6.9  protocol version 29\n", false}, // GNU, pre-3.1
		{"rsync  version 3.0.6  protocol version 30", false},   // -s yes, --info no
		{"rsync  version ", false},                             // the banner's prefix and nothing after — hostile stdout, not a crash
		{"rsync  version  \n", false},                          // whitespace alone where a version would stand
		{"openrsync 3.1 (protocol version 29)", false},         // macOS's own
		{"rsync version 3.4.1", false},                         // not GNU's banner
		{"", false},
	} {
		if got := rsyncVersionOK(c.line); got != c.want {
			t.Errorf("rsyncVersionOK(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

// The brew homes are darwin's alone: a GUI process's PATH — launchd's
// bare /usr/bin:/bin:/usr/sbin:/sbin — never holds them, so the find
// must reach there itself; elsewhere the tools live where LookPath
// sees them or nowhere.
func TestXferBrewHomes(t *testing.T) {
	for _, c := range []struct {
		goos, tool string
		want       []string
	}{
		{"darwin", "rsync", []string{"/opt/homebrew/bin/rsync", "/usr/local/bin/rsync"}},
		{"darwin", "scp", []string{"/opt/homebrew/bin/scp", "/usr/local/bin/scp"}},
		{"linux", "rsync", nil},
		{"windows", "scp", nil},
	} {
		if got := xferBrewHomes(c.goos, c.tool); !slices.Equal(got, c.want) {
			t.Errorf("xferBrewHomes(%q, %q) = %q, want %q", c.goos, c.tool, got, c.want)
		}
	}
}

// findScp takes LookPath's word unasked: an executable file is
// LookPath's own definition, Windows included — where FileInfo.Mode
// carries no execute bit, and a bit test there would reject every
// candidate, LookPath's own answer first of all. Only the brew homes,
// paths no one vouched for, stand asked to be executables — and they
// exist on darwin alone, where the bit means what it says.
func TestFindScpTakesLookPathsWord(t *testing.T) {
	want, err := exec.LookPath("scp")
	if err != nil {
		t.Skip("no scp on this machine to take the word of")
	}
	p, ok := findScp()
	if !ok || p != want {
		t.Errorf("findScp() = %q, %v; want LookPath's own %q", p, ok, want)
	}
}

// -- the spawn layer and the scp flows, on a scripted process --

// fakeProc is the process a test's xferSpawn answers: the stdout and
// stderr it holds, the error Wait answers once release lets it — nil
// for at once — and the channel Kill closes, so a run can be watched
// mid-flight and the ✕ observed landing on the process.
type fakeProc struct {
	out, errOut string
	waitErr     error
	release     chan struct{}
	killed      chan struct{}
	killOnce    sync.Once
}

func newFakeProc() *fakeProc {
	return &fakeProc{killed: make(chan struct{})}
}

func (p *fakeProc) Stdout() io.Reader { return strings.NewReader(p.out) }
func (p *fakeProc) Stderr() io.Reader { return strings.NewReader(p.errOut) }

func (p *fakeProc) Kill() error {
	p.killOnce.Do(func() { close(p.killed) })
	return nil
}

func (p *fakeProc) Wait() error {
	if p.release != nil {
		<-p.release
	}
	return p.waitErr
}

// spawnSpy stands in for the app's process spawn: how many times the
// app reached for one, and the argv it reached with.
type spawnSpy struct {
	mu    sync.Mutex
	calls int
	name  string
	args  []string
	proc  *fakeProc
}

func (s *spawnSpy) install(a *App) {
	a.xferSpawn = func(name string, args []string) (xferProc, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls++
		s.name, s.args = name, append([]string(nil), args...)
		return s.proc, nil
	}
}

func (s *spawnSpy) ran() (int, string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.name, s.args
}

// scpExt turns the files fixture into the ext flows' own: the setting
// on scp and the host's addr the ssh the predicate asks for — the
// fixture's host carries an empty addr, and without this the flows
// would silently fall back and the test fail of something else.
func scpExt(f *filesHost) *filesHost {
	f.a.set.FilesTransfer = "scp"
	f.h.addr = remote.Addr{Scheme: "ssh", Host: "srv"}
	return f
}

// The predicate holds only when every word of it does: the user chose
// scp or rsync, the host speaks ssh, and it is not Windows — anything
// else, silently, is the built-in stream's (decision 3).
func TestXferExtApplies(t *testing.T) {
	f := newFilesHost(t)
	for _, c := range []struct {
		mode, scheme, os string
		want             bool
	}{
		{"scp", "ssh", "linux", true},
		{"rsync", "ssh", "", true},
		{"builtin", "ssh", "linux", false},
		{"scp", "wsl", "linux", false},
		{"scp", "http", "linux", false},
		{"rsync", "", "linux", false},
		{"scp", "ssh", "windows", false},
	} {
		f.a.set.FilesTransfer = c.mode
		f.h.addr = remote.Addr{Scheme: c.scheme, Host: "srv"}
		f.h.info.OS = c.os
		if got := f.a.xferExtApplies(f.h); got != c.want {
			t.Errorf("%s × scheme %q × os %q: xferExtApplies = %v, want %v", c.mode, c.scheme, c.os, got, c.want)
		}
	}
}

// A host the tools cannot speak to — not ssh, or Windows — never
// spawns: the built-in stream runs instead, and no toast tells of the
// switch, for none is needed (decision 3).
func TestXferExtFallsBackSilently(t *testing.T) {
	for _, c := range []struct{ name, scheme, os string }{
		{"a wsl host", "wsl", "linux"},
		{"a windows host", "ssh", "windows"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFilesHost(t)
			f.a.set.FilesTransfer = "scp"
			f.h.addr = remote.Addr{Scheme: c.scheme, Host: "srv"}
			f.h.info.OS = c.os
			spawn := &spawnSpy{proc: newFakeProc()}
			spawn.install(f.a)
			dialed := make(chan struct{}, 1)
			f.a.xferDial = func(*host) (net.Conn, *bufio.Reader, error) {
				dialed <- struct{}{}
				return nil, nil, errors.New("the built-in stream's own dial")
			}
			dest := t.TempDir()
			f.a.xferSave = func(string) (string, error) { return filepath.Join(dest, "a.txt"), nil }

			f.a.downloadFrom(f.h, "/r/a.txt", false)
			select {
			case <-dialed:
			case <-time.After(8 * time.Second):
				t.Fatal("the built-in stream never dialed; the flow strayed")
			}
			time.Sleep(50 * time.Millisecond)
			if calls, _, _ := spawn.ran(); calls != 0 {
				t.Fatalf("the flow spawned %d time(s); this host is the built-in stream's alone", calls)
			}
		})
	}
}

// An scp upload lands on the dir itself — the daemon's dedup is the
// built-in stream's word, which no scp line passes through (decision 5)
// — one line for every file at once, its arc sized by the files' stat
// sum while the run still waits (scp never says a byte of its own,
// decision 6), and the folder lists again when it lands.
func TestScpExtUploadOverwrites(t *testing.T) {
	f := scpExt(newFilesHost(t))
	fs := &f.a.side.files
	fs.dirs = map[string]*dirList{filesKey(f.h, "/srv"): {entries: []proto.FSEntry{{Name: "old.txt"}}}}
	src := t.TempDir()
	up := []string{filepath.Join(src, "a.txt"), filepath.Join(src, "b.txt")}
	for i, p := range up {
		if err := os.WriteFile(p, []byte(strings.Repeat("x", (i+1)*4)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var total int64
	for _, p := range up {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		total += st.Size()
	}
	proc := newFakeProc()
	proc.release = make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-proc.release:
		default:
			close(proc.release)
		}
	})
	spawn := &spawnSpy{proc: proc}
	spawn.install(f.a)
	f.a.xferPick = func(dir, multiple bool) ([]string, error) {
		if dir || !multiple {
			t.Errorf("the picker asked dir=%v multiple=%v", dir, multiple)
		}
		return up, nil
	}

	f.a.uploadTo(f.h, "/srv")
	// Mid-run, the run held open: the arc's whole is already the files'
	// stat sum, set before the line went out.
	f.waitsFor(t, func() error {
		x := f.a.side.files.xfer
		if x == nil {
			return errors.New("no transfer under way")
		}
		if x.total != total {
			return fmt.Errorf("x.total = %d, want the stat sum %d", x.total, total)
		}
		return nil
	})
	close(proc.release)
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the transfer never ended")
		}
		if f.a.pendingToast != locales.FilesUploadDone(2, "remote") {
			return errors.New("toast " + f.a.pendingToast)
		}
		if l := fs.dirs[filesKey(f.h, "/srv")]; l == nil || l.gen == fs.gen {
			return errors.New("the folder did not list again")
		}
		return nil
	})
	calls, name, args := spawn.ran()
	if calls != 1 {
		t.Fatalf("spawned %d time(s), want the one line for every file", calls)
	}
	// The find's own answer is what runs the line, an absolute word, and
	// it lands in the setting — scp asks no --version, existence is all
	// it needs, so the find costs no spawn of its own.
	if name != f.a.set.FilesScpPath {
		t.Errorf("spawned %q, want the find's own %q", name, f.a.set.FilesScpPath)
	}
	if f.a.set.FilesScpPath == "" {
		t.Error("the find's answer never reached the setting")
	}
	if want := scpUploadArgs(f.h.addr, up, "/srv"); !slices.Equal(args, want) {
		t.Errorf("argv = %q, want %q", args, want)
	}
	if s := fmt.Sprint(args); strings.Contains(s, " copy") {
		t.Errorf("argv carries a copy name %q; an ext upload overwrites", s)
	}
}

// A scp file download that fails — the process's own words on its
// stderr, carried by the toast — leaves nothing behind: no file of the
// name, no temp of it, and the slot gives the claim back.
func TestScpExtDownloadFileAtomic(t *testing.T) {
	f := scpExt(newFilesHost(t))
	dest := t.TempDir()
	f.a.xferSave = func(string) (string, error) { return filepath.Join(dest, "a.txt"), nil }
	proc := newFakeProc()
	proc.errOut = "No such file or directory"
	proc.waitErr = errors.New("exit status 1")
	spawn := &spawnSpy{proc: proc}
	spawn.install(f.a)

	f.a.downloadFrom(f.h, "/r/a.txt", false)
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the slot never cleared")
		}
		if toast := f.a.pendingToast; !strings.Contains(toast, "No such file or directory") {
			return errors.New("toast " + toast)
		}
		return nil
	})
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("leftovers %v", entries)
	}
	if calls, _, _ := spawn.ran(); calls != 1 {
		t.Fatalf("spawned %d time(s), want one", calls)
	}
}

// A folder download lands beside one of its own name as a paste does —
// "proj copy" — the built-in's own rule, and the done toast says the
// folder that landed.
func TestScpExtDownloadDirDedupsName(t *testing.T) {
	f := scpExt(newFilesHost(t))
	dest := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dest, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.a.xferPick = func(dir, multiple bool) ([]string, error) {
		if !dir || multiple {
			t.Errorf("the picker asked dir=%v multiple=%v", dir, multiple)
		}
		return []string{dest}, nil
	}
	spawn := &spawnSpy{proc: newFakeProc()}
	spawn.install(f.a)

	f.a.downloadFrom(f.h, "/r/proj", true)
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the transfer never ended")
		}
		if f.a.pendingToast != locales.FilesDownloadDone("proj copy") {
			return errors.New("toast " + f.a.pendingToast)
		}
		return nil
	})
	_, _, args := spawn.ran()
	if want := scpDownloadArgs(f.h.addr, "/r/proj", true, filepath.Join(dest, "proj copy")); !slices.Equal(args, want) {
		t.Errorf("argv = %q, want %q", args, want)
	}
}

// scp cannot size what it fetches: a download's total stays 0 and the
// arc spins (decision 6). An upload's whole — the files' stat sum — is
// pinned by TestScpExtUploadOverwrites, not here.
func TestScpExtDownloadSpins(t *testing.T) {
	f := scpExt(newFilesHost(t))
	dest := t.TempDir()
	f.a.xferSave = func(string) (string, error) { return filepath.Join(dest, "a.bin"), nil }
	proc := newFakeProc()
	proc.release = make(chan struct{})
	t.Cleanup(func() { close(proc.release) })
	spawn := &spawnSpy{proc: proc}
	spawn.install(f.a)

	f.a.downloadFrom(f.h, "/r/a.bin", false)
	f.waitsFor(t, func() error {
		x := f.a.side.files.xfer
		if x == nil {
			return errors.New("no transfer under way")
		}
		if x.cancel == nil {
			return errors.New("the run never armed")
		}
		if x.total != 0 {
			return fmt.Errorf("x.total = %d, want 0: the arc spins", x.total)
		}
		return nil
	})
}

// The ✕ of a transfer of its own kills the process, and the end reads
// cancelled — not failed — though Wait then answers the kill's own
// signal error, and nothing of the download lands.
func TestXferExtCancelKillsProc(t *testing.T) {
	f := scpExt(newFilesHost(t))
	dest := t.TempDir()
	f.a.xferSave = func(string) (string, error) { return filepath.Join(dest, "a.txt"), nil }
	proc := newFakeProc()
	proc.release = make(chan struct{})
	spawn := &spawnSpy{proc: proc}
	spawn.install(f.a)

	f.a.downloadFrom(f.h, "/r/a.txt", false)
	var cancel func()
	f.waitsFor(t, func() error {
		if x := f.a.side.files.xfer; x != nil && x.cancel != nil {
			cancel = x.cancel
			return nil
		}
		return errors.New("no transfer under way to cancel")
	})
	f.ui.Lock()
	cancel()
	f.ui.Unlock()
	select {
	case <-proc.killed:
	case <-time.After(8 * time.Second):
		t.Fatal("the ✕ never killed the process")
	}
	proc.waitErr = errors.New("signal: killed")
	close(proc.release)
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the slot never cleared")
		}
		if f.a.pendingToast != locales.FilesTransferCancelled() {
			return errors.New("toast " + f.a.pendingToast)
		}
		return nil
	})
	if entries, err := os.ReadDir(dest); err != nil || len(entries) != 0 {
		t.Fatalf("the cancelled download left %v behind (err %v)", entries, err)
	}
}

// scp not on this machine ends the transfer where it stands — the
// missing-tool word, never a fallback, never silence (decision 10):
// the user chose the tool, and a silent switch of method would hide
// the problem. PATH alone is emptied, the one failure source; the
// spawn seam stays.
func TestXferExtMissingBinaryFails(t *testing.T) {
	f := scpExt(newFilesHost(t))
	t.Setenv("PATH", "")
	up := filepath.Join(t.TempDir(), "up.txt")
	if err := os.WriteFile(up, []byte("worth"), 0o600); err != nil {
		t.Fatal(err)
	}
	spawn := &spawnSpy{proc: newFakeProc()}
	spawn.install(f.a)
	f.a.xferPick = func(bool, bool) ([]string, error) { return []string{up}, nil }

	f.a.uploadTo(f.h, "/srv")
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the slot never cleared")
		}
		if toast := f.a.pendingToast; toast != locales.FilesTransferFailed(locales.FilesToolMissing("scp")) {
			return errors.New("toast " + toast)
		}
		return nil
	})
	if calls, _, _ := spawn.ran(); calls != 0 {
		t.Fatalf("the flow spawned %d time(s); there is nothing to run", calls)
	}
}

// A picker walked away from gives the claim back — no spawn, no failure
// toast — as the built-in flow's own walked-away does.
func TestScpExtPickWalkedAway(t *testing.T) {
	f := scpExt(newFilesHost(t))
	spawn := &spawnSpy{proc: newFakeProc()}
	spawn.install(f.a)
	f.a.xferPick = func(bool, bool) ([]string, error) { return nil, nil }

	f.a.uploadTo(f.h, "/srv")
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the slot never cleared")
		}
		if toast := f.a.pendingToast; toast != locales.FilesUploadToast("remote") {
			return errors.New("toast " + toast + ", want the flow's own opening one")
		}
		return nil
	})
	if calls, _, _ := spawn.ran(); calls != 0 {
		t.Fatalf("the walked-away picker still spawned %d time(s)", calls)
	}
}

// rsync's own frames, as they ride stdout: the bytes done, comma-grouped
// by rsync's own hand, and the percent it figures — the (xfr#…) tail a
// folder's last frame carries and a 0% first frame both parse; every
// other line rsync writes does not, and a frame that does not parse is
// skipped, never an error (decision 6).
func TestParseRsyncProgress(t *testing.T) {
	for _, c := range []struct {
		frame string
		done  int64
		pct   float64
		ok    bool
	}{
		{"          1,234,567  45%  10.00MB/s    0:00:01", 1234567, 45, true},
		{"              4,096   0%    0.00kB/s    0:00:00", 4096, 0, true},
		{"         12,345,678 100%  12.34MB/s    0:00:02 (xfr#1, to-chk=0/3)", 12345678, 100, true},
		{"", 0, 0, false},
		{`rsync: link_stat "/a" failed: No such file`, 0, 0, false},
		{"        N bytes sent", 0, 0, false},
	} {
		done, pct, ok := parseRsyncProgress(c.frame)
		if done != c.done || pct != c.pct || ok != c.ok {
			t.Errorf("parseRsyncProgress(%q) = %d, %v, %v; want %d, %v, %v",
				c.frame, done, pct, ok, c.done, c.pct, c.ok)
		}
	}
}

// callSpy stands in for the app's process spawn where every call is
// countable apart: it records the name and the argv of each, in order,
// and answers each with the process its own shape asks for — the local
// find, the probe and the transfer are three processes, and their
// spawns must not blur.
type callSpy struct {
	mu    sync.Mutex
	calls [][]string
	names []string
	proc  func(args []string) *fakeProc // what each spawn answers; nil a plain one
	errOn func(args []string) bool      // spawns this says of answer no process at all
}

func (s *callSpy) install(a *App) {
	a.xferSpawn = func(name string, args []string) (xferProc, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.errOn != nil && s.errOn(args) {
			return nil, errors.New("spawn refused")
		}
		s.calls = append(s.calls, append([]string(nil), args...))
		s.names = append(s.names, name)
		var p *fakeProc
		if s.proc != nil {
			p = s.proc(args)
		} else {
			p = newFakeProc()
		}
		return p, nil
	}
}

func (s *callSpy) snapshot() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.calls...)
}

func (s *callSpy) snapshotNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.names...)
}

func (s *callSpy) count(match func([]string) bool) int {
	n := 0
	for _, c := range s.snapshot() {
		if match(c) {
			n++
		}
	}
	return n
}

// isProbeArgs says an argv is the probe's: the ssh line that ends in
// "rsync --version" — no transfer argv carries that word at its tail.
func isProbeArgs(args []string) bool {
	return len(args) > 0 && args[len(args)-1] == "rsync --version"
}

// rsyncExt turns the files fixture into rsync's own — as scpExt's, the
// mode on the host's addr the predicate asks for, the fixture's host
// carrying an empty one.
func rsyncExt(f *filesHost) *filesHost {
	f.a.set.FilesTransfer = "rsync"
	f.h.addr = remote.Addr{Scheme: "ssh", Host: "srv"}
	return f
}

// isLocalVersionArgs says an argv is the local rsync find's own: a
// bare --version asked of one candidate path — no other spawn of the
// flows is a single argument long.
func isLocalVersionArgs(args []string) bool {
	return len(args) == 1 && args[0] == "--version"
}

// gnuRsyncProc — the process a --version question wants back: GNU
// 3.4.1's own banner, the local find's and the probe's yes.
func gnuRsyncProc() *fakeProc {
	p := newFakeProc()
	p.out = "rsync  version 3.4.1  protocol version 31\n"
	return p
}

// A host without rsync ends its transfer where it stands — the
// dedicated toast, never the generic failed one, never a fallback — and
// nothing of the transfer itself spawns beside the probe (decision 4
// and 10).
func TestRsyncProbeAbsentAborts(t *testing.T) {
	f := rsyncExt(newFilesHost(t))
	up := filepath.Join(t.TempDir(), "up.txt")
	if err := os.WriteFile(up, []byte("worth"), 0o600); err != nil {
		t.Fatal(err)
	}
	spy := &callSpy{proc: func(args []string) *fakeProc {
		if isLocalVersionArgs(args) {
			return gnuRsyncProc()
		}
		p := newFakeProc()
		if isProbeArgs(args) {
			// bash: rsync: command not found — the host's own word.
			p.waitErr = errors.New("exit status 127")
		}
		return p
	}}
	spy.install(f.a)
	f.a.xferPick = func(bool, bool) ([]string, error) { return []string{up}, nil }

	f.a.uploadTo(f.h, "/srv")
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the slot never cleared")
		}
		if f.a.pendingToast != locales.FilesRsyncAbsent("remote") {
			return errors.New("toast " + f.a.pendingToast)
		}
		return nil
	})
	for _, args := range spy.snapshot() {
		if !isProbeArgs(args) && !isLocalVersionArgs(args) {
			t.Fatalf("a transfer ran beside the failed probe: %q", args)
		}
	}
	if n := spy.count(isProbeArgs); n != 1 {
		t.Fatalf("the probe ran %d time(s), want one", n)
	}
}

// The local find runs before anything leaves this machine: a rsync
// that answers --version with anything but GNU 3.1+ — macOS's own
// openrsync, present and answering — ends the transfer where it stands,
// its own toast, never a fallback (decision 10), and nothing else
// spawns: no probe hop, no transfer line that would only fail mid-air
// on flags the tool never learned.
func TestRsyncLocalFindAborts(t *testing.T) {
	f := rsyncExt(newFilesHost(t))
	up := filepath.Join(t.TempDir(), "up.txt")
	if err := os.WriteFile(up, []byte("worth"), 0o600); err != nil {
		t.Fatal(err)
	}
	spy := &callSpy{proc: func(args []string) *fakeProc {
		p := newFakeProc()
		if isLocalVersionArgs(args) {
			// macOS's own: present, answering, flagless — the shape
			// 2.3.37 read as good enough.
			p.out = "openrsync 2.6.9 (protocol version 29)\n"
		}
		return p
	}}
	spy.install(f.a)
	f.a.xferPick = func(bool, bool) ([]string, error) { return []string{up}, nil }

	f.a.uploadTo(f.h, "/srv")
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the slot never cleared")
		}
		if f.a.pendingToast != locales.FilesRsyncLocalOld() {
			return errors.New("toast " + f.a.pendingToast)
		}
		return nil
	})
	for _, args := range spy.snapshot() {
		if !isLocalVersionArgs(args) {
			t.Fatalf("a spawn left the find's own question: %q", args)
		}
	}
}

// The find's answer is the tool itself: the path whose banner passed
// is the name the transfer runs under — an absolute word, so a brew
// rsync past PATH is the one that moves the files, not whatever a
// bare "rsync" would resolve to all over again — and it is written
// into the setting, for the user to see and change: the setting is
// the find's own memory.
func TestRsyncLocalFindOKRunsFoundPath(t *testing.T) {
	f := rsyncExt(newFilesHost(t))
	up := filepath.Join(t.TempDir(), "up.txt")
	if err := os.WriteFile(up, []byte("worth"), 0o600); err != nil {
		t.Fatal(err)
	}
	spy := &callSpy{proc: func(args []string) *fakeProc {
		if isLocalVersionArgs(args) || isProbeArgs(args) {
			return gnuRsyncProc()
		}
		return newFakeProc()
	}}
	spy.install(f.a)
	f.a.xferPick = func(bool, bool) ([]string, error) { return []string{up}, nil }

	f.a.uploadTo(f.h, "/srv")
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the transfer never ended")
		}
		if f.a.pendingToast != locales.FilesUploadDone(1, "remote") {
			return errors.New("toast " + f.a.pendingToast)
		}
		return nil
	})
	calls, names := spy.snapshot(), spy.snapshotNames()
	var found string
	for i, args := range calls {
		if isLocalVersionArgs(args) {
			found = names[i]
		}
	}
	if found == "" {
		t.Fatal("the local find never ran")
	}
	ran := false
	for i, args := range calls {
		if !isLocalVersionArgs(args) && !isProbeArgs(args) {
			ran = true
			if names[i] != found {
				t.Errorf("the transfer ran as %q, want the find's own %q", names[i], found)
			}
		}
	}
	if !ran {
		t.Fatal("the transfer never ran")
	}
	if f.a.set.FilesRsyncPath != found {
		t.Errorf("the setting holds %q, want the find's own %q — the find's answer is the user's to see", f.a.set.FilesRsyncPath, found)
	}
	// And it is the file's word too, not the app's memory alone: Load
	// reads back what applySettings saved, so the next run of the app
	// takes the path without the find again.
	if saved := settings.Load().FilesRsyncPath; saved != found {
		t.Errorf("settings.Load() holds %q, want the find's own %q — the memory must be the file's", saved, found)
	}
}

// A path the user set is their own word: the find never questions it —
// no --version asked of it — and the transfer runs it exactly, while
// the probe still asks the host, for the host is not the user's word.
func TestXferSettledPathSkipsFind(t *testing.T) {
	f := rsyncExt(newFilesHost(t))
	f.a.set.FilesRsyncPath = "/my/rsync"
	up := filepath.Join(t.TempDir(), "up.txt")
	if err := os.WriteFile(up, []byte("worth"), 0o600); err != nil {
		t.Fatal(err)
	}
	spy := &callSpy{proc: func(args []string) *fakeProc {
		if isProbeArgs(args) {
			return gnuRsyncProc()
		}
		return newFakeProc()
	}}
	spy.install(f.a)
	f.a.xferPick = func(bool, bool) ([]string, error) { return []string{up}, nil }

	f.a.uploadTo(f.h, "/srv")
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the transfer never ended")
		}
		if f.a.pendingToast != locales.FilesUploadDone(1, "remote") {
			return errors.New("toast " + f.a.pendingToast)
		}
		return nil
	})
	for _, args := range spy.snapshot() {
		if isLocalVersionArgs(args) {
			t.Fatalf("the find questioned the user's own path: %q", args)
		}
	}
	names := spy.snapshotNames()
	if len(names) == 0 || !slices.Contains(names, "/my/rsync") {
		t.Errorf("the transfer never ran the user's path; spawns %q", names)
	}
}

// A path the user set that will not run says so in a word that says
// where to fix it — the toast carries the path itself — never a bare
// spawn error the user cannot place.
func TestXferSettledPathBadErr(t *testing.T) {
	f := rsyncExt(newFilesHost(t))
	f.a.set.FilesRsyncPath = "/no/such/rsync"
	up := filepath.Join(t.TempDir(), "up.txt")
	if err := os.WriteFile(up, []byte("worth"), 0o600); err != nil {
		t.Fatal(err)
	}
	spy := &callSpy{proc: func(args []string) *fakeProc {
		if isProbeArgs(args) {
			return gnuRsyncProc()
		}
		return newFakeProc()
	}}
	spy.errOn = func(args []string) bool {
		// The user's path: no such file, as a stale hand-set path is.
		return !isProbeArgs(args)
	}
	spy.install(f.a)
	f.a.xferPick = func(bool, bool) ([]string, error) { return []string{up}, nil }

	f.a.uploadTo(f.h, "/srv")
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the slot never cleared")
		}
		if toast := f.a.pendingToast; !strings.Contains(toast, "/no/such/rsync") {
			return errors.New("toast " + toast)
		}
		return nil
	})
}

// A host whose rsync answers but too old — macOS's own openrsync, or a
// GNU 2.6.9 — ends its transfer in its own word too: the probe reads
// the banner, not just the exit, and the toast says what to update
// where, still no fallback (decision 10).
func TestRsyncProbeRemoteOldAborts(t *testing.T) {
	f := rsyncExt(newFilesHost(t))
	up := filepath.Join(t.TempDir(), "up.txt")
	if err := os.WriteFile(up, []byte("worth"), 0o600); err != nil {
		t.Fatal(err)
	}
	spy := &callSpy{proc: func(args []string) *fakeProc {
		if isLocalVersionArgs(args) {
			return gnuRsyncProc()
		}
		p := newFakeProc()
		if isProbeArgs(args) {
			// Present and exiting 0 — all the old probe asked — but a
			// banner the flags cannot stand on.
			p.out = "rsync  version 2.6.9  protocol version 29\n"
		}
		return p
	}}
	spy.install(f.a)
	f.a.xferPick = func(bool, bool) ([]string, error) { return []string{up}, nil }

	f.a.uploadTo(f.h, "/srv")
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the slot never cleared")
		}
		if f.a.pendingToast != locales.FilesRsyncRemoteOld("remote") {
			return errors.New("toast " + f.a.pendingToast)
		}
		return nil
	})
	for _, args := range spy.snapshot() {
		if !isLocalVersionArgs(args) && !isProbeArgs(args) {
			t.Fatalf("a transfer ran beside the too-old probe: %q", args)
		}
	}
}

// The probe's answer holds for the host's next transfer too: one ssh
// hop per host per run — the cache's own cost, and its worth
// (decision 4).
func TestRsyncProbeCachesPerHost(t *testing.T) {
	f := rsyncExt(newFilesHost(t))
	up := filepath.Join(t.TempDir(), "up.txt")
	if err := os.WriteFile(up, []byte("worth"), 0o600); err != nil {
		t.Fatal(err)
	}
	spy := &callSpy{proc: func(args []string) *fakeProc {
		if isLocalVersionArgs(args) || isProbeArgs(args) {
			return gnuRsyncProc()
		}
		return newFakeProc()
	}}
	spy.install(f.a)
	f.a.xferPick = func(bool, bool) ([]string, error) { return []string{up}, nil }

	for i := 0; i < 2; i++ {
		f.a.uploadTo(f.h, "/srv")
		f.waitsFor(t, func() error {
			if f.a.side.files.xfer != nil {
				return errors.New("the transfer never ended")
			}
			if f.a.pendingToast != locales.FilesUploadDone(1, "remote") {
				return errors.New("toast " + f.a.pendingToast)
			}
			return nil
		})
	}
	if n := spy.count(isLocalVersionArgs); n != 1 {
		t.Fatalf("the local find ran %d time(s) over two transfers, want the one", n)
	}
	if n := spy.count(isProbeArgs); n != 1 {
		t.Fatalf("the probe ran %d time(s) over two transfers, want the one", n)
	}
	if got := len(spy.snapshot()); got != 4 {
		t.Fatalf("%d spawn(s) in all, want the find, the probe and two transfers", got)
	}
}

// rsync's frames drive the arc as they ride stdout — stderr carries only
// failure text, and a reader wired there would pass no real transfer at
// all (decision 6). The bytes fill done, and a download's whole — no
// stat could size it — is what the percent backs out, the final frame
// through the throttle, for an arc that never filled its end looks
// stalled there.
func TestRsyncProgressDrivesArc(t *testing.T) {
	f := rsyncExt(newFilesHost(t))
	dest := t.TempDir()
	f.a.xferSave = func(string) (string, error) { return filepath.Join(dest, "a.bin"), nil }
	proc := newFakeProc()
	proc.out = "          1,234,567  45%\r     2,469,134 100%  12.34MB/s    0:00:02 (xfr#1, to-chk=0/3)\n"
	proc.release = make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-proc.release:
		default:
			close(proc.release)
		}
	})
	spy := &callSpy{proc: func(args []string) *fakeProc {
		if isLocalVersionArgs(args) || isProbeArgs(args) {
			return gnuRsyncProc()
		}
		return proc
	}}
	spy.install(f.a)

	f.a.downloadFrom(f.h, "/r/a.bin", false)
	// Mid-run, the run held open: both frames have ridden, and the arc
	// holds their pair — 45% of a whole backs out 1234567*100/45, and
	// the 100% frame's own smaller guess does not pull it back.
	f.waitsFor(t, func() error {
		x := f.a.side.files.xfer
		if x == nil {
			return errors.New("no transfer under way")
		}
		if x.done != 2469134 {
			return fmt.Errorf("x.done = %d, want the final frame's 2469134", x.done)
		}
		if x.total != 2743482 {
			return fmt.Errorf("x.total = %d, want the backed-out 2743482", x.total)
		}
		return nil
	})
	close(proc.release)
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the transfer never ended")
		}
		if f.a.pendingToast != locales.FilesDownloadDone("a.bin") {
			return errors.New("toast " + f.a.pendingToast)
		}
		return nil
	})
}

// An upload's whole is the stat sum the flow set before the run, and a
// percent never rewrites it — the backing-out is a download's alone,
// whose whole no stat could size (decision 6). The frame's bytes still
// fill the done side of the arc.
func TestRsyncUploadTotalStaysStat(t *testing.T) {
	f := rsyncExt(newFilesHost(t))
	src := t.TempDir()
	up := []string{filepath.Join(src, "a.txt"), filepath.Join(src, "b.txt")}
	for i, p := range up {
		if err := os.WriteFile(p, []byte(strings.Repeat("x", (i+1)*4)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	proc := newFakeProc()
	proc.out = "          1,234,567  45%\n"
	proc.release = make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-proc.release:
		default:
			close(proc.release)
		}
	})
	spy := &callSpy{proc: func(args []string) *fakeProc {
		if isLocalVersionArgs(args) || isProbeArgs(args) {
			return gnuRsyncProc()
		}
		return proc
	}}
	spy.install(f.a)
	f.a.xferPick = func(bool, bool) ([]string, error) { return up, nil }

	f.a.uploadTo(f.h, "/srv")
	// Mid-run, the run held open: the frame's bytes fill done — 1234567
	// against the files' 12 — and the whole stays the two files' stat
	// sum, 12, whatever percent the frame claims.
	f.waitsFor(t, func() error {
		x := f.a.side.files.xfer
		if x == nil {
			return errors.New("no transfer under way")
		}
		if x.done != 1234567 {
			return fmt.Errorf("x.done = %d, want the frame's 1234567", x.done)
		}
		if x.total != 12 {
			return fmt.Errorf("x.total = %d, want the stat sum 12", x.total)
		}
		return nil
	})
	close(proc.release)
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the transfer never ended")
		}
		if f.a.pendingToast != locales.FilesUploadDone(2, "remote") {
			return errors.New("toast " + f.a.pendingToast)
		}
		return nil
	})
}

// A folder a download takes aligns with the built-in tar's own skip —
// --exclude=.git on the rsync line — and a file's line carries none, a
// file having nothing to exclude (decision 8).
func TestRsyncDownloadDirExcludesGit(t *testing.T) {
	f := rsyncExt(newFilesHost(t))
	dest := t.TempDir()
	f.a.xferPick = func(dir, multiple bool) ([]string, error) {
		if !dir || multiple {
			t.Errorf("the picker asked dir=%v multiple=%v", dir, multiple)
		}
		return []string{dest}, nil
	}
	spy := &callSpy{proc: func(args []string) *fakeProc {
		if isLocalVersionArgs(args) || isProbeArgs(args) {
			return gnuRsyncProc()
		}
		return newFakeProc()
	}}
	spy.install(f.a)

	f.a.downloadFrom(f.h, "/r/proj", true)
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the transfer never ended")
		}
		return nil
	})
	calls := spy.snapshot()
	if len(calls) != 3 { // the local find, the probe, then the transfer
		t.Fatalf("%d spawn(s) in all: %q", len(calls), calls)
	}
	if !isLocalVersionArgs(calls[0]) {
		t.Errorf("the first spawn is not the local find: %q", calls[0])
	}
	if !isProbeArgs(calls[1]) {
		t.Errorf("the second spawn is not the probe: %q", calls[1])
	}
	root := filepath.Join(dest, "proj")
	if want := rsyncDownloadArgs(f.h.addr, "/r/proj", true, root); !slices.Equal(calls[2], want) {
		t.Errorf("dir argv = %q, want %q", calls[2], want)
	}

	// A file's line of the same flow: no --exclude — nothing to exclude.
	f.a.xferSave = func(string) (string, error) { return filepath.Join(dest, "a.txt"), nil }
	files := &callSpy{}
	files.install(f.a)
	f.a.downloadFrom(f.h, "/r/a.txt", false)
	f.waitsFor(t, func() error {
		if f.a.side.files.xfer != nil {
			return errors.New("the transfer never ended")
		}
		return nil
	})
	for _, args := range files.snapshot() {
		if !isProbeArgs(args) && slices.Contains(args, "--exclude=.git") {
			t.Errorf("a file download carries --exclude=.git: %q", args)
		}
	}
}

// xferRun reads a process's stdout a frame at a time — \r and \n both,
// for rsync's progress rides \r without a tty — and hands each frame
// over; the tail with neither end still a frame. scp passes nil, for
// it writes nothing machine-readable.
func TestXferRunSplitsFrames(t *testing.T) {
	f := newFilesHost(t)
	proc := newFakeProc()
	proc.out = "          1,234  45%\r     2,468  90%\ntail"
	spawn := &spawnSpy{proc: proc}
	spawn.install(f.a)
	var frames []string
	x := &transferState{kind: xferUpload, name: "t"}
	err := f.a.xferRun(f.h, x, "rsync", []string{"-a"}, func(fr string) { frames = append(frames, fr) })
	if err != nil {
		t.Fatalf("xferRun: %v", err)
	}
	want := []string{"          1,234  45%", "     2,468  90%", "tail"}
	if !slices.Equal(frames, want) {
		t.Errorf("frames = %q, want %q", frames, want)
	}
}
