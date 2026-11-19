package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"kero/internal/proto"

	"kero/locales"
)

// The download-by-path dialog's flow, apart from its view (Task 2): the
// path typed goes to the host as an fs.stat — the daemon itself resolves
// it, relative against the folder the tree shows, absolute, ~/ from the
// home — and what the host says it names downloads through downloadFrom,
// a file by the Save dialog, a folder by the picker. What it does not
// name stays in the dialog, told why in red.

// dlFlags are what the transfer seams' stand-ins below record: which
// dialog answered a download, and that the dial a download ends in ran —
// downloadFrom past its gate and its dialog. They live under the
// fixture's mu, as writes does, for the goroutines they ride.
type dlFlags struct {
	saved, picked, dialled bool
}

// dlFakes stands in for the transfer's dialogs and wire: the dialogs
// answer a temp folder and say which fired, and the dial fails at once
// and says it ran — the dialog's flow is the test's, not the
// transfer's.
func dlFakes(t *testing.T, f *filesHost) *dlFlags {
	t.Helper()
	d := &dlFlags{}
	dir := t.TempDir()
	f.a.xferSave = func(string) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		d.saved = true
		return filepath.Join(dir, "saved"), nil
	}
	f.a.xferPick = func(bool, bool) ([]string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		d.picked = true
		return []string{dir}, nil
	}
	f.a.xferDial = func(*host) (net.Conn, *bufio.Reader, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		d.dialled = true
		return nil, nil, errors.New("no dial: the dialog's flow is the test's, not the transfer's")
	}
	return d
}

// The typed path reaches downloadFrom as the host's own absolute one:
// relative against the base the dialog opened on, absolute as typed
// (cleaned), ~/ from the home; a folder picks the picker, a file the
// Save dialog — downloadFrom's own routing, unchanged.
func TestDlByPathResolvesPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  string // what the host resolves it to, and downloads
		dir   bool
	}{
		{"relative", "notes.md", "/srv/proj/notes.md", false},
		{"absolute", "/etc/app.conf", "/etc/app.conf", false},
		{"home", "~/backups", "/home/u/backups", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFilesHost(t)
			d := dlFakes(t, f)
			f.files["/srv/proj/notes.md"] = []byte("n")
			f.files["/etc/app.conf"] = []byte("c")
			f.dirs["/home/u/backups"] = true
			f.a.side.files.dl = dlByPath{open: true, host: f.h, base: "/srv/proj", gen: 1, query: tc.query}
			f.a.dlByPathSubmit()
			// The dial comes last of the three, so once it ran the
			// dialog before it is settled too.
			f.waitsFor(t, func() error {
				f.mu.Lock()
				defer f.mu.Unlock()
				if !d.dialled {
					return fmt.Errorf("%s never reached a transfer", tc.query)
				}
				if d.saved == tc.dir || d.picked != tc.dir {
					return fmt.Errorf("saved=%v picked=%v, want saved=%v picked=%v", d.saved, d.picked, !tc.dir, tc.dir)
				}
				return nil
			})
			f.ui.Lock()
			defer f.ui.Unlock()
			if f.a.side.files.dl.open {
				t.Fatal("the dialog stayed open on a path that downloads")
			}
		})
	}
}

// A path nothing is at is answered in the dialog, which stays open to
// take a better one: red text naming the miss, the Download button free
// again, and nothing on the wire beyond the stat.
func TestDlByPathNotFoundStaysOpen(t *testing.T) {
	f := newFilesHost(t)
	d := dlFakes(t, f)
	f.a.side.files.dl = dlByPath{open: true, host: f.h, base: "/srv/proj", gen: 1, query: "/no/such"}
	f.a.dlByPathSubmit()
	f.waitsFor(t, func() error {
		f.mu.Lock()
		dialled := d.dialled
		f.mu.Unlock()
		if dialled {
			return errors.New("a path nothing is at downloaded")
		}
		dl := &f.a.side.files.dl
		if dl.err != locales.FilesPathNotFound() {
			return fmt.Errorf("err %q, want the not-found wording", dl.err)
		}
		if !dl.open || dl.busy {
			return fmt.Errorf("open=%v busy=%v, want the dialog open and free", dl.open, dl.busy)
		}
		return nil
	})
}

// A host the dialog lost the connection to is told so in the dialog,
// synchronously: fs.stat needs the wire, so no goroutine runs toward an
// answer that cannot come.
func TestDlByPathNotConnected(t *testing.T) {
	f := newFilesHost(t)
	d := dlFakes(t, f)
	f.h.cl = nil
	f.a.side.files.dl = dlByPath{open: true, host: f.h, base: "/srv/proj", gen: 1, query: "notes.md"}
	f.a.dlByPathSubmit()
	f.ui.Lock()
	dl := &f.a.side.files.dl
	err, open, busy := dl.err, dl.open, dl.busy
	f.ui.Unlock()
	f.mu.Lock()
	dialled := d.dialled
	f.mu.Unlock()
	if want := locales.HostNotConnected("remote"); err != want {
		t.Fatalf("err %q, want %q", err, want)
	}
	if !open || busy {
		t.Fatalf("open=%v busy=%v, want the dialog open and free", open, busy)
	}
	if dialled {
		t.Fatal("a download started with no connection")
	}
}

// An answer the dialog that asked did not wait for is dropped: closed
// and reopened while the stat was under way, the old answer must not
// color the new dialog red nor start a download. The fixture's own mu
// holds the submit beside the dialog's replacement — the goroutine's
// fs.stat cannot enter handle (which takes it) until the state already
// says dropped — so the drop is what runs, not a race's luck.
func TestDlByPathStaleAnswerDropped(t *testing.T) {
	f := newFilesHost(t)
	d := dlFakes(t, f)
	f.files["/srv/proj/notes.md"] = []byte("n")
	f.a.side.files.dl = dlByPath{open: true, host: f.h, base: "/srv/proj", gen: 1, query: "notes.md"}
	f.mu.Lock()
	f.a.dlByPathSubmit() // its goroutine waits on mu inside handle
	f.a.side.files.dl.open = false
	f.a.side.files.dl.gen++ // the dialog closed, and another opened
	f.mu.Unlock()
	f.waitsFor(t, func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.stats["notes.md"] == 0 {
			return errors.New("the stat never reached the host")
		}
		return nil
	})
	// The dropped answer leaves no trace to wait on — give it a moment
	// to land (and, were the drop missing, to write), then ask what
	// stayed.
	time.Sleep(100 * time.Millisecond)
	f.ui.Lock()
	err := f.a.side.files.dl.err
	f.ui.Unlock()
	f.mu.Lock()
	dialled := d.dialled
	f.mu.Unlock()
	if dialled {
		t.Fatal("the stale answer started a download")
	}
	if err != "" {
		t.Fatalf("the stale answer showed %q", err)
	}
}

// A transfer already under way takes the flow at downloadFrom's own
// gate: the dialog closed on a path that exists, the gate speaks, and
// the transfer under way is untouched.
func TestDlByPathBusyXferToasts(t *testing.T) {
	f := newFilesHost(t)
	d := dlFakes(t, f)
	f.files["/srv/proj/notes.md"] = []byte("n")
	busy := &transferState{kind: "download", name: "busy"}
	f.a.side.files.xfer = busy
	f.a.side.files.dl = dlByPath{open: true, host: f.h, base: "/srv/proj", gen: 1, query: "notes.md"}
	f.a.dlByPathSubmit()
	f.waitsFor(t, func() error {
		f.mu.Lock()
		dialled := d.dialled
		f.mu.Unlock()
		if dialled {
			return errors.New("a second transfer started beside the busy one")
		}
		if f.a.pendingToast != locales.FilesTransferBusy() {
			return fmt.Errorf("toast %q, want the busy one", f.a.pendingToast)
		}
		if f.a.side.files.dl.open {
			return errors.New("the dialog stayed open")
		}
		return nil
	})
	f.ui.Lock()
	defer f.ui.Unlock()
	if f.a.side.files.xfer != busy {
		t.Fatal("the transfer under way was disturbed")
	}
}

// ---- The dialog and its two entrances ----

// dlByPathUI is the Files tab of a remote host with its tree standing and
// the transfer seams faked (dlFakes): the root row carries the dialog's
// entrances, and a good path finishes fast. The session's first pane is
// the fixture host's (the owner map), so the tab is the remote's.
func dlByPathUI(t *testing.T) (*filesHost, *ui.Tester, *dlFlags) {
	t.Helper()
	f := newFilesHost(t)
	d := dlFakes(t, f)
	s := &f.a.st.Sessions[0]
	s.Panes[0].Dir = "/srv/proj" // the pane works where the tree stands
	f.a.prefs.Side = sideFiles
	// The tree's folder listed, as the tab's first showing counted it
	// (gen): the listing sits as it is, asked of no one.
	f.a.side.files.dirs = map[string]*dirList{
		filesKey(f.h, "/srv/proj"): {entries: []proto.FSEntry{{Name: "README.md"}}, gen: 1},
	}
	tt := ui.NewTester(f.a.view, 1280, 720)
	tt.Frame()
	return f, tt, d
}

// The root row's new button opens the dialog; a wrong path is answered in
// red with the dialog open to take a better one, which downloads.
func TestDlByPathDialogErrThenOk(t *testing.T) {
	f, tt, d := dlByPathUI(t)
	f.files["/etc/app.conf"] = []byte("c")
	if err := tt.Click(locales.FilesDownloadByPath()); err != nil {
		t.Fatal(err) // the fifth button, by its label
	}
	tt.Frame()
	if !tt.HasText(locales.FilesDownloadByPathDetail()) {
		t.Fatalf("no dialog; texts %q", tt.Texts())
	}
	tt.Type("/no/such")
	tt.Key(0, ui.KeyEnter)
	f.waitsFor(t, func() error {
		if f.a.side.files.dl.err == "" {
			return errors.New("the miss was not answered")
		}
		return nil
	})
	tt.Frame()
	if !tt.HasText(locales.FilesPathNotFound()) {
		t.Fatalf("no red answer; texts %q", tt.Texts())
	}
	f.ui.Lock()
	if open := f.a.side.files.dl.open; !open {
		f.ui.Unlock()
		t.Fatal("the dialog closed on a path nothing is at")
	}
	// tt.Type appends, it does not replace (palette_test.go types only
	// after emptying the field): clear the input, frame, then type the
	// second try.
	f.a.side.files.dl.query = ""
	f.ui.Unlock()
	tt.Frame()
	tt.Type("/etc/app.conf") // Changed fires on the new text — the second try
	tt.Key(0, ui.KeyEnter)
	f.waitsFor(t, func() error {
		f.mu.Lock()
		dialled := d.dialled
		f.mu.Unlock()
		if !dialled {
			return errors.New("the second try never reached a transfer")
		}
		return nil
	})
	f.ui.Lock()
	defer f.ui.Unlock()
	if f.a.side.files.dl.open {
		t.Fatal("the dialog stayed open on a path that downloads")
	}
}

// Esc and Cancel close the dialog, and the entrance opens it again.
func TestDlByPathDialogOpenClose(t *testing.T) {
	f, tt, _ := dlByPathUI(t)
	if err := tt.Click(locales.FilesDownloadByPath()); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	f.ui.Lock()
	open := f.a.side.files.dl.open
	f.ui.Unlock()
	if open {
		t.Fatal("Escape left the dialog open")
	}
	if err := tt.Click(locales.FilesDownloadByPath()); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText(locales.FilesDownloadByPathDetail()) {
		t.Fatalf("no dialog on the reopen; texts %q", tt.Texts())
	}
	if err := tt.Click(locales.CommonCancel()); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	f.ui.Lock()
	open = f.a.side.files.dl.open
	f.ui.Unlock()
	if open {
		t.Fatal("Cancel left the dialog open")
	}
}

// The root label's menu carries the download beside the upload, and its
// item opens the dialog. The label's text is the pane's own shown path
// too, so a right click by label could hit the pane's header instead:
// point at the label through the row, beside its first button.
// ChooseMenuItem, not Click, to open the dialog: the fifth button carries
// the same label, so a plain click could hit the button and pass green
// with the menu never tested.
func TestDlByPathRootMenu(t *testing.T) {
	_, tt, _ := dlByPathUI(t)
	btn, ok := tt.Find(locales.FilesNewFile())
	if !ok {
		t.Fatalf("no new-file button; texts %q", tt.Texts())
	}
	tt.RightClickAt(btn.X-15, btn.Y+btn.H/2)
	got := tt.Menu()
	for _, want := range []string{locales.FilesUploadHere(), locales.FilesDownloadByPath()} {
		if !slices.Contains(got, want) {
			t.Fatalf("the root's menu is %q, want %q in it", got, want)
		}
	}
	if err := tt.ChooseMenuItem(locales.FilesDownloadByPath()); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText(locales.FilesDownloadByPathDetail()) {
		t.Fatalf("no dialog; texts %q", tt.Texts())
	}
}

// An entrance on a host not connected says so and opens nothing: fs.stat
// needs the wire. The toast is what the window shows — the app's own
// pending word is spent by the frame that shows it.
func TestDlByPathEntranceNotConnected(t *testing.T) {
	f, tt, _ := dlByPathUI(t)
	f.ui.Lock()
	f.h.cl = nil
	f.ui.Unlock()
	tt.Frame()
	if err := tt.Click(locales.FilesDownloadByPath()); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText(locales.HostNotConnected("remote")) {
		t.Fatalf("no toast of it; texts %q", tt.Texts())
	}
	f.ui.Lock()
	open := f.a.side.files.dl.open
	f.ui.Unlock()
	if open {
		t.Fatal("the dialog opened with no connection")
	}
}

// This machine's Files tree carries neither entrance: its files are here
// already.
func TestDlByPathLocalHasNoEntrance(t *testing.T) {
	a := testApp(t)
	s := &a.st.Sessions[0]
	s.Panes[0].Dir = t.TempDir()
	a.prefs.Side = sideFiles
	tt := ui.NewTester(a.view, 1280, 720)
	tt.Frame()
	if err := tt.Click(locales.FilesDownloadByPath()); err == nil {
		t.Fatal("this machine's tree carries the download entrance")
	}
}
