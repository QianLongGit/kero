package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/plugins/terminal"
	"github.com/egoist/mygo/ui"

	"kero/internal/actions"
	"kero/internal/proto"
	"kero/internal/settings"

	"kero/locales"
)

// testApp is the app with a session of two panes side by side, a | b,
// and no kerod.
func testApp(t *testing.T) *App {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KERO_HOME", home)
	// The configuration too: never the user's.
	t.Setenv("KERO_CONFIG", filepath.Join(home, "config"))
	a := newApp()
	var l *proto.Layout
	a.st = proto.State{
		Projects: []proto.Project{{ID: "p1", Name: "demo", Dir: "/tmp"}},
		Sessions: []proto.Session{{
			ID: "s1", Project: "p1", Name: "demo",
			Panes: []proto.Pane{
				{ID: "a", Shell: "fish", Dir: "/tmp", Status: proto.Idle, Progress: -1},
				{ID: "b", Shell: "zsh", Dir: "/tmp", Status: proto.Idle, Progress: -1},
			},
			Layout: l.Split("a", "b", "right", 0),
			Focus:  "a",
		}},
	}
	a.loaded, a.selected = true, "s1"
	a.local().loaded = true
	return a
}

// shot writes the tester's frame to $KERO_SHOTS, to look at.
func shot(t *testing.T, tt *ui.Tester, name string) {
	dir := os.Getenv("KERO_SHOTS")
	if dir == "" {
		return
	}
	f, err := os.Create(filepath.Join(dir, name+".png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	png.Encode(f, tt.Image())
}

func testDropApp(t *testing.T) *App {
	t.Helper()
	a := testApp(t)
	a.local().conn = connReady
	a.terms["a"], a.terms["b"] = &termEntry{}, &termEntry{}
	a.local().cl, _ = projectClient(t, func(method string, _ json.RawMessage) (any, error) {
		return nil, fmt.Errorf("test daemon rejected %s", method)
	})
	return a
}

func TestSidebarReceivesFolderDrops(t *testing.T) {
	a := testDropApp(t)
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	for _, at := range []ui.Point{{X: 10, Y: 30}, {X: 40, Y: 80}, {X: 40, Y: 120}, {X: 100, Y: 400}, {X: 20, Y: 620}} {
		if !tt.DropFiles(at.X, at.Y, t.TempDir()) {
			t.Errorf("sidebar drop at %v was not received", at)
		}
	}
	if tt.DropFiles(a.prefs.SidebarWidth+3, 400, t.TempDir()) {
		t.Error("sidebar received a drop outside its visible bounds")
	}
	shot(t, tt, "sidebar-folder-drop-light")
	tt.SetDark(true)
	tt.Advance(time.Second)
	tt.Frame()
	shot(t, tt, "sidebar-folder-drop-dark")
}

func TestSidebarRejectsUnavailableDrops(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*App)
	}{
		{"hidden", func(a *App) { a.prefs.SidebarHidden = true }},
		{"connecting", func(a *App) { a.local().conn = connConnecting }},
		{"disconnected", func(a *App) { a.local().conn = connFailed }},
		{"no client", func(a *App) { a.local().cl = nil }},
		{"palette", func(a *App) { a.pal.open = true }},
		{"folder picker", func(a *App) { a.picker = folderPicker{open: true, host: a.local()} }},
		{"only remote connected", func(a *App) {
			h := &host{name: "remote", conn: connReady, cl: a.local().cl, loaded: true}
			a.hosts = append(a.hosts, h)
			a.owner = map[string]*host{"s1": h, "p1": h, "a": h, "b": h}
			a.local().conn, a.local().cl = connFailed, nil
		}},
		{"confirmation", func(a *App) { a.confirm = &confirmation{open: true, title: "Confirm", action: "Close"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testDropApp(t)
			tt := ui.NewTester(a.view, 1100, 640)
			tt.Frame()
			tc.set(a)
			tt.Frame()
			if tt.DropFiles(30, 400, t.TempDir()) {
				t.Errorf("sidebar received a drop while %s", tc.name)
			}
		})
	}
}

func TestSidebarLocalDropWithRemoteSelected(t *testing.T) {
	a := testDropApp(t)
	h := &host{name: "remote", conn: connFailed, loaded: true}
	a.hosts = append(a.hosts, h)
	a.owner = map[string]*host{"s1": h, "p1": h, "a": h, "b": h}
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	if !tt.DropFiles(30, 400, t.TempDir()) {
		t.Error("local sidebar drop was blocked by the selected remote host")
	}
}

func TestSidebarKeepsTerminalDrops(t *testing.T) {
	a := testDropApp(t)
	term, err := terminal.New(terminal.Options{Conn: idleConn{make(chan struct{})}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { term.Close() })
	a.terms["a"] = &termEntry{term: term}
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	if !tt.DropFiles(400, 340, filepath.Join(t.TempDir(), "image.png")) {
		t.Error("terminal did not receive a file drop while the sidebar target was active")
	}
}

func TestDragPaneToSide(t *testing.T) {
	a := testDropApp(t)
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	grip, ok := tt.Find("zsh")
	if !ok {
		t.Fatalf("no title of pane b; texts %q", tt.Texts())
	}
	// Pick pane b up by its title, and carry it over a's left edge.
	x, y := grip.X+grip.W/2, grip.Y+grip.H/2
	ar, _ := tt.Find("fish") // pane a's title
	tx, ty := ar.X+10, float32(340)
	tt.Press(x, y)
	for i := 1; i <= 12; i++ {
		f := float32(i) / 12
		tt.Move(x+(tx-x)*f, y+(ty-y)*f)
		tt.Advance(16 * time.Millisecond)
		tt.Frame()
	}
	tt.Advance(400 * time.Millisecond)
	tt.Frame()
	if !tt.HasText("Split Left") {
		t.Errorf("no preview of a split on the left; texts %q", tt.Texts())
	}
	shot(t, tt, "drag-left")

	// Over a's middle: the panes would swap.
	tt.Move(ar.X+120, 340)
	tt.Advance(400 * time.Millisecond)
	tt.Frame()
	if !tt.HasText("Swap Panes") {
		t.Errorf("no preview of a swap; texts %q", tt.Texts())
	}
	shot(t, tt, "drag-swap")

	tt.Release(ar.X+120, 340)
	tt.Frame()
	tt.Advance(time.Second)
	tt.Frame()
	if got := a.st.Sessions[0].Layout.Panes(); !slices.Equal(got, []string{"b", "a"}) {
		t.Fatalf("after the drop, panes %v, want b then a", got)
	}
	if a.focusOf(&a.st.Sessions[0]) != "b" {
		t.Errorf("the pane moved has not the focus")
	}
	shot(t, tt, "dropped")
}

// The sidebar's animation moves the header's title one way only, in
// whole pixels: a title that steps back and forth shakes.
func TestSidebarAnimationMonotonic(t *testing.T) {
	bar := ui.TitleBar{Left: 78, Height: 60}
	for _, width := range []float32{200, 268, 333, 440} {
		prevX := float32(-1)
		prevEdge := float32(-1)
		for i := 0; i <= 20000; i++ {
			ch := chromeFor(float32(i)/20000, width, bar)
			x := ch.sideW + ch.mainPad + ch.headerPad // the title's left, in the window
			edge := ch.sideW + ch.mainPad             // the card's left edge
			for _, v := range []float32{ch.sideW, ch.mainPad, ch.headerPad} {
				if v != float32(int(v)) {
					t.Fatalf("width %v, progress %v: %v is not a whole pixel", width, ch.shown, v)
				}
			}
			if x < prevX {
				t.Fatalf("width %v, step %d: the title moved back, %v to %v", width, i, prevX, x)
			}
			if edge < prevEdge {
				t.Fatalf("width %v, step %d: the card's edge moved back, %v to %v", width, i, prevEdge, edge)
			}
			prevX, prevEdge = x, edge
		}
		if ch := chromeFor(1, width, bar); ch.headerPad != 16 || ch.mainPad != 0 || ch.sideW != width {
			t.Errorf("width %v shown: %+v", width, ch)
		}
		if ch := chromeFor(0, width, bar); ch.sideW != 0 || ch.mainPad != cardGap {
			t.Errorf("width %v hidden: %+v", width, ch)
		}
	}
}

// The sidebar's animation never holds still before it lands: a frame
// that does not move, then one that does, reads as a stutter.
func TestSidebarAnimationNoHolds(t *testing.T) {
	ease := sidebarMotion.Ease
	frames := int(sidebarMotion.Duration / (time.Second / 60))
	prev := float32(-1)
	for i := 0; i <= frames; i++ {
		w := chromeFor(ease(float32(i)/float32(frames)), 268, ui.TitleBar{}).sideW
		if w == prev && w != 268 {
			t.Errorf("frame %d: the sidebar held at %v before landing", i, w)
		}
		prev = w
	}
}

// With the sidebar hidden, only the header at the window's top left
// keeps clear of the traffic lights: a pane under it starts its title
// where any card does.
func TestHeaderPadBelowTopLeft(t *testing.T) {
	a := testApp(t)
	a.prefs.SidebarHidden = true
	var l *proto.Layout
	a.st.Sessions[0].Layout = l.Split("a", "b", "down", 0)
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	tt.Advance(time.Second)
	tt.Frame()
	top, ok1 := tt.Find("fish")
	below, ok2 := tt.Find("zsh")
	if !ok1 || !ok2 {
		t.Fatalf("no titles; texts %q", tt.Texts())
	}
	if below.Y <= top.Y {
		t.Fatalf("pane b is not under a: %v, %v", top, below)
	}
	if below.X >= top.X {
		t.Errorf("the title under the top left's starts at %v, as far in as the top left's %v", below.X, top.X)
	}
	shot(t, tt, "header-pad-below")
}

// The window moves from the panes' headers and the gaps around them, and
// not from a press on a pane, which focuses it.
func TestWindowDragRegions(t *testing.T) {
	a := testApp(t)
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	// Pane b's header, past its title: it moves the window, and leaves
	// the focus where it was.
	b, _ := tt.Find("zsh")
	tt.ClickAt(b.X+200, b.Y+b.H/2)
	tt.Frame()
	if f := a.focusOf(&a.st.Sessions[0]); f != "a" {
		t.Errorf("a press on a header's empty stretch moved the focus to %q", f)
	}
	// Pane b's body: it takes the press, and the focus.
	tt.ClickAt(b.X+200, 400)
	tt.Frame()
	if f := a.focusOf(&a.st.Sessions[0]); f != "b" {
		t.Errorf("a press on pane b left the focus on %q", f)
	}
}

// A selection copied as it ends says so; a copy the user asked for, or
// a program's, does not.
func TestCopyOnSelectToast(t *testing.T) {
	a := testApp(t)
	o := a.termOptions("a", nil)
	o.OnCopy("x", terminal.CopyUser)
	o.OnCopy("x", terminal.CopyProgram)
	if a.pendingToast != "" {
		t.Errorf("toast %q for a copy that shows itself", a.pendingToast)
	}
	o.OnCopy("x", terminal.CopySelect)
	if a.pendingToast != "Copied to clipboard" {
		t.Errorf("toast %q after copying a selection", a.pendingToast)
	}
}

// The sidebar's foot counts what wants a look, each count going to the
// next session of it, and says nothing while nothing does ($KERO_SHOTS).
func TestSidebarFooter(t *testing.T) {
	a := testApp(t)
	a.local().conn = connReady
	tt := ui.NewTester(a.view, 1100, 700)
	tt.Frame()
	for _, txt := range tt.Texts() {
		if strings.Contains(txt, "running") {
			t.Errorf("the foot still says %q", txt)
		}
	}
	base := a.st.Sessions[0]
	add := func(id, name string, st proto.Status, unread bool) {
		s := base
		s.ID, s.Name, s.Status, s.Unread = id, name, st, unread
		a.st.Sessions = append(a.st.Sessions, s)
	}
	add("s2", "deploy", proto.Waiting, true)
	add("s3", "refactor", proto.Working, false)
	add("s4", "tests", proto.Working, false)
	add("s5", "docs", proto.Idle, true)
	tt.Frame()
	tt.Advance(time.Second)
	tt.Frame()
	shot(t, tt, "sidebar-footer")
	for label, want := range map[string]string{"1 session needs you": "s2", "2 sessions working": "s3", "1 session done, unseen": "s5"} {
		a.selected = "s1"
		tt.Frame()
		if err := tt.Click(label); err != nil {
			t.Errorf("no %q in the foot: %v", label, err)
			continue
		}
		tt.Frame()
		if a.selected != want {
			t.Errorf("%q went to %s, want %s", label, a.selected, want)
		}
	}
}

// A session's tooltip shows below the pointer where it rested, as macOS
// shows tooltips, not centered over the row ($KERO_SHOTS).
func TestSessionTooltipAtPointer(t *testing.T) {
	// 30 keeps the pointer inside the row in every density: compact's
	// block inset puts the row's left edge 34 in.
	for _, dx := range []float32{30, 160} {
		a := testApp(t)
		a.st.Sessions[0].Process = "claude"
		a.st.Sessions[0].Dir = "/Users/someone/dev/kero"
		tt := ui.NewTester(a.view, 1100, 700)
		tt.Frame()
		r, ok := tt.Find("1") // the project header's count, above the row
		if !ok {
			t.Fatal("no project header")
		}
		m := a.sidebarDensity()
		x, y := 12+dx, r.Y+r.H/2+m.hdrH/2+3+m.rowH/2 // the session row's middle
		tt.Move(x, y)
		tt.Advance(2 * time.Second)
		tt.Frame()
		var tip ui.Rect
		found := false
		for _, txt := range tt.Texts() {
			if strings.HasPrefix(txt, "claude · ") {
				tip, found = tt.Find(txt)
			}
		}
		if !found {
			t.Fatalf("dx %g: no tooltip; texts %q", dx, tt.Texts())
		}
		if d := tip.X - x; d < 0 || d > 16 {
			t.Errorf("dx %g: tooltip text at x %g, the pointer at %g", dx, tip.X, x)
		}
		if d := tip.Y - y; d < 12 || d > 36 {
			t.Errorf("dx %g: tooltip text at y %g, the pointer at %g", dx, tip.Y, y)
		}
		shot(t, tt, fmt.Sprintf("session-tooltip-%g", dx))
	}
}

// A kerod of another version of Kero gets a chip to restart it, which asks
// first while a program runs, as the restart ends it.
func TestRestartStaleDaemon(t *testing.T) {
	a := testApp(t)
	a.local().conn, a.local().daemonApp = connReady, "1.9.0"
	a.st.Sessions[0].Process = "claude"
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	if err := tt.Click("Restart Service"); err != nil {
		t.Fatalf("no chip to restart kerod: %v; texts %q", err, tt.Texts())
	}
	tt.Frame()
	if a.confirm == nil || !strings.Contains(a.confirm.detail, "Kero 1.9.0") {
		t.Fatalf("no question before the restart: %+v", a.confirm)
	}

	a.confirm, a.local().daemonApp = nil, mygo.App.Version()
	tt.Frame()
	if tt.HasText("Restart Service") {
		t.Error("a chip to restart a kerod of this version")
	}
}

// idleConn is a Conn that shows only what a test feeds the terminal.
type idleConn struct{ closed chan struct{} }

func (c idleConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, os.ErrClosed
}
func (c idleConn) Write(b []byte) (int, error) { return len(b), nil }
func (c idleConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

// Find searches the pane the user is in, its scrollback too: the bar
// counts the matches as one types, Enter and the buttons go through them,
// and Escape closes it, the highlights going; Find Next opens it again
// with the last query.
func TestFindBar(t *testing.T) {
	if testing.Short() {
		t.Skip("downloads libghostty-vt")
	}
	if err := terminal.Load(); err != nil {
		t.Fatal(err)
	}
	a := testApp(t)
	term, err := terminal.New(terminal.Options{Conn: idleConn{make(chan struct{})}})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	var b strings.Builder
	for i := range 200 {
		if i%50 == 7 {
			b.WriteString("error: build failed\r\n")
		} else {
			b.WriteString("ok " + itoa(i) + "\r\n")
		}
	}
	term.Feed([]byte(b.String()))
	a.terms["a"] = &termEntry{term: term}
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	a.run("find.open", nil)
	tt.Frame()
	tt.Type("ERROR")
	for range 50 {
		tt.Frame()
		if s := term.SearchStatus(); !s.Searching && s.Selected > 0 {
			break
		}
	}
	tt.Frame()
	if !tt.HasText("1 of 4") {
		t.Fatalf("no count; texts %q", tt.Texts())
	}
	shot(t, tt, "find")
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	tt.Key(ui.Shift, ui.KeyEnter)
	tt.Frame()
	if !tt.HasText("2 of 4") {
		t.Errorf("after Enter twice and Shift+Enter; texts %q", tt.Texts())
	}
	if err := tt.Click(a.tip(locales.ActionFindNext(), "find.next")); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText("3 of 4") {
		t.Errorf("after the button for the next; texts %q", tt.Texts())
	}
	shot(t, tt, "find-scrolled")
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	tt.Frame()
	if a.find.pane != "" || tt.HasText("3 of 4") || term.SearchStatus().Text != "" {
		t.Errorf("Escape left the bar open or the search on: %+v; texts %q", term.SearchStatus(), tt.Texts())
	}
	a.run("find.next", nil)
	tt.Frame()
	tt.Frame()
	if a.find.pane != "a" || a.find.query != "ERROR" || term.SearchStatus().Text != "ERROR" {
		t.Errorf("Find Next did not search again: %+v, %+v", a.find.query, term.SearchStatus())
	}
}

// Command over the path of an image a pane printed, relative to its
// directory, shows the image beside it, until Command is let go of,
// which lets go of the image too.
func TestImagePeek(t *testing.T) {
	if testing.Short() {
		t.Skip("downloads libghostty-vt")
	}
	if err := terminal.Load(); err != nil {
		t.Fatal(err)
	}
	a := testApp(t)
	dir := t.TempDir()
	a.st.Sessions[0].Panes[0].Dir = dir
	f, err := os.Create(filepath.Join(dir, "shot.png"))
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 30, 20))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{230, 120, 40, 255}), image.Point{}, draw.Src)
	png.Encode(f, img)
	f.Close()
	term, err := terminal.New(terminal.Options{Conn: idleConn{make(chan struct{})}, OpenLink: func(link string) bool { return a.openLink("a", link) }})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	term.Feed([]byte("saved shot.png:3 and notes.txt\r\n"))
	a.terms["a"] = &termEntry{term: term}
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	b, ok := tt.Find("Terminal")
	if !ok {
		t.Fatalf("no terminal; texts %q", tt.Texts())
	}
	// Along the first row to the path, Command pressed with the pointer
	// still.
find:
	for y := b.Y; y < b.Y+40; y += 4 {
		for x := b.X; x < b.X+b.W/2; x += 3 {
			tt.Move(x, y)
			tt.HoldModifiers(ui.Cmd)
			if link, _, _ := term.HoveredLink(); link != "" {
				break find
			}
		}
	}
	tt.Frame()
	if link, path, _ := term.HoveredLink(); link != "shot.png:3" || !path {
		t.Fatalf("hovered %q, %v; text %q", link, path, term.Text())
	}
	if want := filepath.Join(dir, "shot.png"); a.peek.path != want {
		t.Fatalf("peeks at %q, want %q", a.peek.path, want)
	}
	// The decoded image lands, as the window's update would bring it, and
	// shows in the flyout.
	data, _ := os.ReadFile(a.peek.path)
	a.peek.bm, _ = ui.DecodeBitmap(data)
	tt.Frame()
	ft := ui.NewTester(a.peekView, peekMinW, 20+peekPad+peekCaption)
	ft.Frame()
	if !ft.HasText("shot.png") || !ft.HasText("30 × 20") {
		t.Fatalf("no preview; texts %q", ft.Texts())
	}
	shot(t, ft, "peek")
	tt.HoldModifiers(0)
	tt.Frame()
	if a.peek.bm != nil || a.peek.path != "" {
		t.Errorf("the preview stays once Command is let go of: %+v", a.peek)
	}
}

// A split top to bottom, with one side by side inside it, gives every
// split element a key of its own: MyGo panics on two elements with one key
// under one parent, as when a column split made a Row before its Column.
func TestNestedSplitKeys(t *testing.T) {
	a := testApp(t)
	s := &a.st.Sessions[0]
	s.Panes = append(s.Panes, proto.Pane{ID: "c", Shell: "bash", Dir: "/tmp", Status: proto.Idle, Progress: -1})
	s.Layout = &proto.Layout{Ratio: 0.5, A: &proto.Layout{Pane: "a"},
		B: &proto.Layout{Row: true, Ratio: 0.5, A: &proto.Layout{Pane: "b"}, B: &proto.Layout{Pane: "c"}}}
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	tt.Frame()
	for _, title := range []string{"fish", "zsh", "bash"} {
		if !tt.HasText(title) {
			t.Errorf("no pane %s; texts %q", title, tt.Texts())
		}
	}
}

// A browser pane shows a toolbar over its page, as its header, with an
// address bar that a blank page asks to type in and Open Location gives
// the keyboard; it attaches no terminal.
func TestBrowserPane(t *testing.T) {
	a := testApp(t)
	s := &a.st.Sessions[0]
	s.Panes[1] = proto.Pane{ID: "b", Dir: "/tmp", URL: proto.BlankURL, Status: proto.Idle, Progress: -1}
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	tt.Frame()
	for _, text := range []string{locales.BrowserBlank()} {
		if !tt.HasText(text) {
			t.Errorf("no %q; texts %q", text, tt.Texts())
		}
	}
	if a.terms["b"] != nil {
		t.Error("a browser pane attached a terminal")
	}
	e := a.webs["b"]
	if e == nil {
		t.Fatal("no browser for pane b")
	}
	// A blank page's address bar takes the keyboard: what is typed there
	// goes to the page, as a URL.
	tt.Type("localhost:5173")
	tt.Frame()
	if e.address != "localhost:5173" {
		t.Errorf("typed %q into the address bar", e.address)
	}
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	if e.url != "http://localhost:5173" {
		t.Errorf("went to %q", e.url)
	}

	// Its toolbar is its header: no title over it, page's or other.
	s = &a.st.Sessions[0]
	s.Panes[1].Title = "Vite App"
	tt.Frame()
	if tt.HasText("Vite App") || tt.HasText("browser") || !tt.HasText(locales.BrowserAddress()) {
		t.Errorf("a browser pane shows a title bar; texts %q", tt.Texts())
	}
	// Open Location gives the browser's address bar the keyboard, with
	// what it holds to type over.
	a.setFocus(s, "b")
	a.run("browser.address", nil)
	tt.Frame()
	tt.Type("kero.sh")
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	if e.url != "https://kero.sh" {
		t.Errorf("Open Location went to %q", e.url)
	}
	shot(t, tt, "browser")
}

// An address typed in the palette opens in a browser, first of what it
// finds; words find the browser's actions, but open nothing.
func TestPaletteOpensAddress(t *testing.T) {
	a := testApp(t)
	tt := ui.NewTester(a.view, 1100, 640)
	a.openPalette()
	tt.Frame()
	tt.Type("localhost:5173")
	tt.Frame()
	// The palette's rows follow its label.
	texts := tt.Texts()
	rows := texts[slices.Index(texts, locales.PaletteLabel())+1:]
	if len(rows) < 2 || rows[0] != locales.PaletteOpenInBrowser() || rows[1] != "http://localhost:5173" {
		t.Fatalf("the address is not the first item; rows %q", rows)
	}
	a.pal.query = ""
	tt.Frame()
	tt.Type("web")
	tt.Frame()
	if tt.HasText(locales.PaletteOpenInBrowser()) || !tt.HasText(locales.ActionNewBrowser()) {
		t.Errorf("words: texts %q", tt.Texts())
	}
}

// With focus-follows-mouse, the pointer going into a pane focuses it, but
// a pane that comes under a pointer standing still does not.
func TestFocusFollowsMouse(t *testing.T) {
	a := testApp(t)
	a.set.FocusFollowsMouse = true
	s := &a.st.Sessions[0]
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	ar, _ := tt.Find("fish") // pane a's title
	br, _ := tt.Find("zsh")  // pane b's
	move := func(x float32) {
		tt.Move(x, 340)
		tt.Advance(16 * time.Millisecond)
		tt.Frame()
	}
	move(ar.X + 40)
	if got := a.focusOf(s); got != "a" {
		t.Fatalf("over a, focus %q", got)
	}
	move(br.X + 40)
	if got := a.focusOf(s); got != "b" {
		t.Fatalf("into b, focus %q, want b", got)
	}

	// The keyboard takes the focus back to a: b, under the pointer, keeps
	// it from there, as the pointer moves on in it.
	a.setFocus(s, "a")
	tt.Frame()
	move(br.X + 60)
	if got := a.focusOf(s); got != "a" {
		t.Fatalf("moving in b after the keyboard focused a, focus %q", got)
	}

	// The panes swap places under the pointer standing still, and back.
	var l *proto.Layout
	s.Layout = l.Split("b", "a", "right", 0)
	tt.Advance(time.Second)
	tt.Frame()
	s.Layout = l.Split("a", "b", "right", 0)
	tt.Advance(time.Second)
	tt.Frame()
	if got := a.focusOf(s); got != "a" {
		t.Fatalf("b came under the pointer standing still, and took the focus")
	}

	// Off, the pointer leaves the focus be.
	a.set.FocusFollowsMouse = false
	move(ar.X + 40)
	move(br.X + 40)
	if got := a.focusOf(s); got != "a" {
		t.Fatalf("with the setting off, focus %q", got)
	}
}

// writes is a Conn that keeps what the terminal sends to the program.
type writes struct {
	idleConn
	got chan []byte
}

func (w writes) Write(b []byte) (int, error) {
	w.got <- append([]byte(nil), b...)
	return len(b), nil
}

// ⌘⌫, ⌘← and ⌘→ send Control+U, Control+A and Control+E, which clear the
// line and go to its start and end, and only on a Mac, where ⌘ is not the
// terminal's Control.
func TestCommandKeysEditLine(t *testing.T) {
	if testing.Short() {
		t.Skip("downloads libghostty-vt")
	}
	if err := terminal.Load(); err != nil {
		t.Fatal(err)
	}
	a := testApp(t)
	conn := writes{idleConn{make(chan struct{})}, make(chan []byte, 8)}
	term, err := terminal.New(terminal.Options{Conn: conn})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	a.terms["a"] = &termEntry{term: term}
	o := a.termOptions("a", nil)
	for _, k := range []struct {
		key  ui.Key
		want string
	}{{ui.KeyBackspace, "\x15"}, {ui.KeyLeft, "\x01"}, {ui.KeyRight, "\x05"}} {
		if took := o.OnKey(ui.Super, k.key); took != (runtime.GOOS == "darwin") {
			t.Fatalf("⌘+%v taken: %v", k.key, took)
		}
		if runtime.GOOS != "darwin" {
			continue
		}
		select {
		case b := <-conn.got:
			if string(b) != k.want {
				t.Errorf("⌘+%v sent %q, want %q", k.key, b, k.want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("⌘+%v sent nothing", k.key)
		}
	}
	for _, k := range []struct {
		mods ui.Modifiers
		key  ui.Key
	}{
		{0, ui.KeyBackspace}, {ui.Super | ui.Shift, ui.KeyBackspace}, {ui.Super, ui.KeyDelete},
		{0, ui.KeyLeft}, {ui.Super | ui.Shift, ui.KeyLeft}, {ui.Super | ui.Alt, ui.KeyRight},
	} {
		if o.OnKey(k.mods, k.key) {
			t.Errorf("%v+%v taken", k.mods, k.key)
		}
	}
}

// A long name in a narrow sidebar ends in an ellipsis: the time beside
// it, and the ⌘ shortcut in its place, show whole.
func TestSidebarRowKeepsTime(t *testing.T) {
	widths := func(name string) (ago, key float32) {
		a := testApp(t)
		a.prefs.SidebarWidth = minSidebar
		tt := ui.NewTester(a.view, 1100, 640)
		a.st.Sessions[0].Name = name
		a.st.Sessions[0].Since = tt.Now().Add(-3 * time.Hour).UnixMilli()
		tt.Frame()
		r, ok := tt.Find(locales.AgoHours(3))
		if !ok {
			t.Fatalf("%q: no time shown", name)
		}
		tt.HoldModifiers(ui.Cmd)
		tt.Frame()
		k, ok := tt.Find(actions.Glyphs("CmdOrCtrl+1"))
		if !ok {
			t.Fatalf("%q: no shortcut shown", name)
		}
		return r.W, k.W
	}
	t0, k0 := widths("demo")
	t1, k1 := widths("Investigate why the session list truncates long generated titles")
	if t1 < t0 || k1 < k0 {
		t.Errorf("with a long name, the time is %g wide (want %g) and the shortcut %g (want %g)", t1, t0, k1, k0)
	}
}

// A detailed row shows, under the session's name, its status message,
// else its branch and changes, at the whole row's width; a compact one
// keeps to one line ($KERO_SHOTS).
func TestSidebarRows(t *testing.T) {
	a := testApp(t)
	a.local().conn = connReady
	a.st.Sessions[0].Dir = "/Users/someone/dev/demo"
	base := a.st.Sessions[0]
	add := func(id, name string, st proto.Status, msg string, g *proto.GitInfo) {
		s := base
		s.ID, s.Name, s.Status, s.Message, s.Git = id, name, st, msg, g
		a.st.Sessions = append(a.st.Sessions, s)
	}
	add("s2", "migrate", proto.Working, "Migrating the\ndatabase", nil)
	add("s3", "login", proto.Idle, "", &proto.GitInfo{Root: "/tmp/demo", Branch: "fix-login-redirect-after-oauth", Default: "main", Ahead: 2, Files: 3, Added: 40, Removed: 2})
	a.st.Sessions[1].Progress = 40
	tt := ui.NewTester(a.view, 1100, 700)
	tt.Frame()
	shot(t, tt, "sidebar-rows-detailed")
	texts := strings.Join(tt.Texts(), "\n")
	for _, want := range []string{"Migrating the database", "fix-login-redirect-after-oauth ↑2 · 3 files +40 −2", "/Users/someone/dev/demo"} {
		if !strings.Contains(texts, want) {
			t.Errorf("detailed rows have no %q; texts %q", want, tt.Texts())
		}
	}
	// The branch on the second line obeys the sidebar's branch setting,
	// as the one beside a compact row's name does; the line falls back
	// to the folder, which is its own last choice.
	a.set.SidebarBranch = false
	tt.Frame()
	texts = strings.Join(tt.Texts(), "\n")
	if strings.Contains(texts, "fix-login-redirect-after-oauth") || strings.Contains(texts, "↑2") {
		t.Errorf("a detailed row shows the branch with the setting off; texts %q", tt.Texts())
	}
	if !strings.Contains(texts, "Migrating the database") {
		t.Errorf("a detailed row lost its message with the branch off; texts %q", tt.Texts())
	}
	a.set.SidebarBranch = true
	tt.Frame()
	a.set.SidebarRows = "compact"
	tt.Frame()
	shot(t, tt, "sidebar-rows-compact")
	texts = strings.Join(tt.Texts(), "\n")
	if strings.Contains(texts, "Migrating the database") {
		t.Errorf("a compact row shows the message")
	}
	if !strings.Contains(texts, "fix-login-redirect-after-oauth") {
		t.Errorf("a compact row lost its branch; texts %q", tt.Texts())
	}
}

// Dragging a project's name moves it among its host's projects, before
// or after the project it is dropped on.
func TestProjectOrder(t *testing.T) {
	a := testApp(t)
	a.st.Projects = append(a.st.Projects,
		proto.Project{ID: "p2", Name: "two"},
		proto.Project{ID: "p3", Name: "three"})
	a.prefs.ProjectOrder = []string{"p3", "p1", "p2"}
	ids := func() []string {
		var out []string
		for _, g := range a.hostGroups(a.local()) {
			out = append(out, g.project.ID)
		}
		return out
	}
	if got := ids(); !slices.Equal(got, []string{"p3", "p1", "p2"}) {
		t.Fatalf("ordered %v", got)
	}
	a.moveProject(projectDrag{host: a.local(), id: "p1"}, "p3", false)
	if got := ids(); !slices.Equal(got, []string{"p1", "p3", "p2"}) {
		t.Errorf("before p3: %v", got)
	}
	a.moveProject(projectDrag{host: a.local(), id: "p2"}, "p1", false)
	if got := ids(); !slices.Equal(got, []string{"p2", "p1", "p3"}) {
		t.Errorf("before p1: %v", got)
	}
}

// A project's name dragged onto another project's lower half puts it
// after that project.
// A project's header dragged onto another host's moves its host's whole
// section there — the sidebar's only handle on the order of the hosts'
// sections — and the settings keep it.
func TestHostDrag(t *testing.T) {
	a := testApp(t)
	one := &host{name: "one", conn: connReady, loaded: true}
	two := &host{name: "two", conn: connReady, loaded: true}
	a.hosts = append(a.hosts, one, two)
	a.set.Hosts = append(a.set.Hosts, settings.Host{Name: "one", Addr: "one.local"}, settings.Host{Name: "two", Addr: "two.local"})
	a.st.Projects = append(a.st.Projects,
		proto.Project{ID: "q1", Name: "pone"},
		proto.Project{ID: "q2", Name: "ptwo"})
	a.st.Sessions = append(a.st.Sessions,
		proto.Session{ID: "os1", Project: "q1", Name: "one sess", Created: 100},
		proto.Session{ID: "ts1", Project: "q2", Name: "two sess", Created: 100})
	a.owner = map[string]*host{"q1": one, "q2": two, "os1": one, "ts1": two}
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	src, ok := tt.Find("pone")
	if !ok {
		t.Fatalf("no pone project; texts %q", tt.Texts())
	}
	dst, ok := tt.Find("ptwo")
	if !ok {
		t.Fatalf("no ptwo project; texts %q", tt.Texts())
	}
	dragged(tt, src, dst)
	var order []string
	for _, h := range a.hosts[1:] {
		order = append(order, h.name)
	}
	if !slices.Equal(order, []string{"two", "one"}) {
		t.Errorf("hosts %v, want two then one", order)
	}
	if len(a.set.Hosts) != 2 || a.set.Hosts[0].Name != "two" || a.set.Hosts[1].Name != "one" {
		t.Errorf("the settings kept %v", a.set.Hosts)
	}
	tt.Frame()
	late, _ := tt.Find("pone")
	prior, _ := tt.Find("ptwo")
	if prior.Y > late.Y {
		t.Error("the sections did not trade places")
	}
}

func TestProjectDrag(t *testing.T) {
	a := testApp(t)
	a.st.Projects = append(a.st.Projects, proto.Project{ID: "p2", Name: "two"})
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	src, ok := tt.Find("demo")
	if !ok {
		t.Fatalf("no demo project; texts %q", tt.Texts())
	}
	dst, ok := tt.Find("two")
	if !ok {
		t.Fatalf("no two project; texts %q", tt.Texts())
	}
	tt.Press(src.X+10, src.Y+src.H/2)
	for i := 1; i <= 10; i++ {
		f := float32(i) / 10
		x := src.X + 10
		y := src.Y + src.H/2 + (dst.Y+dst.H-4-src.Y-src.H/2)*f
		tt.Move(x, y)
		tt.Advance(16 * time.Millisecond)
		tt.Frame()
	}
	tt.Release(dst.X+10, dst.Y+dst.H-4)
	tt.Frame()
	tt.Advance(50 * time.Millisecond)
	tt.Frame()
	if !slices.Equal(a.prefs.ProjectOrder, []string{"p2", "p1"}) {
		t.Errorf("order %v, want p2 then p1", a.prefs.ProjectOrder)
	}
}

// A remote host's projects drag as this machine's do, in their own
// order: the drag of a remote header to another of its host's leaves
// the order and the screen follows.
func TestProjectDragRemote(t *testing.T) {
	a := testApp(t)
	h := &host{name: "remote", conn: connReady, loaded: true}
	a.hosts = append(a.hosts, h)
	a.st.Projects = append(a.st.Projects,
		proto.Project{ID: "r1", Name: "rone"},
		proto.Project{ID: "r2", Name: "rtwo"})
	a.st.Sessions = append(a.st.Sessions,
		proto.Session{ID: "rs1", Project: "r1", Name: "rone sess", Created: 100},
		proto.Session{ID: "rs2", Project: "r2", Name: "rtwo sess", Created: 200})
	a.owner = map[string]*host{"r1": h, "r2": h, "rs1": h, "rs2": h}
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	// The host's kerod pushes its layout as it changes, as the app sits
	// in the sidebar: what each push leaves must not undo the drags.
	a.mergeState(h, proto.State{
		Projects: []proto.Project{{ID: "r1", Name: "rone"}, {ID: "r2", Name: "rtwo"}},
		Sessions: []proto.Session{
			{ID: "rs1", Project: "r1", Name: "rone sess", Created: 100},
			{ID: "rs2", Project: "r2", Name: "rtwo sess", Created: 200}}})
	tt.Frame()
	src, ok := tt.Find("rone")
	if !ok {
		t.Fatalf("no rone project; texts %q", tt.Texts())
	}
	dst, ok := tt.Find("rtwo")
	if !ok {
		t.Fatalf("no rtwo project; texts %q", tt.Texts())
	}
	dragged(tt, src, dst)
	if !slices.Equal(a.prefs.ProjectOrder, []string{"r2", "r1"}) {
		t.Errorf("order %v, want r2 then r1", a.prefs.ProjectOrder)
	}
	tt.Frame()
	two, _ := tt.Find("rtwo")
	one, _ := tt.Find("rone")
	if one.Y < two.Y {
		t.Errorf("the remote projects kept their order: rone at %v, rtwo at %v", one.Y, two.Y)
	}
}

// The Order choice sorts a project's sessions by created or updated time.
func TestSidebarOrder(t *testing.T) {
	a := testApp(t)
	a.st.Sessions[0].Created, a.st.Sessions[0].Activity = 100, 200
	a.st.Sessions = append(a.st.Sessions,
		proto.Session{ID: "s2", Project: "p1", Created: 200, Activity: 100},
		proto.Session{ID: "s3", Project: "p1", Created: 300, Activity: 400})
	ids := func() []string {
		var out []string
		for _, g := range a.hostGroups(a.local()) {
			for _, s := range g.sessions {
				out = append(out, s.ID)
			}
		}
		return out
	}
	a.prefs.SidebarOrder = "created"
	if got := ids(); !slices.Equal(got, []string{"s1", "s2", "s3"}) {
		t.Errorf("created: %v", got)
	}
	a.prefs.SidebarOrder = "updated"
	if got := ids(); !slices.Equal(got, []string{"s3", "s1", "s2"}) {
		t.Errorf("updated: %v", got)
	}
}

// The sidebar's rows follow its density setting: compact, the default,
// and standard, the look before the setting.
func TestSidebarDensity(t *testing.T) {
	a := testApp(t)
	a.set.SidebarRows = "compact" // the density alone moves the rows here
	a.st.Projects[0].Name = "proj"
	a.st.Sessions[0].Name = "sess"
	a.st.Sessions = append(a.st.Sessions, proto.Session{ID: "s2", Project: "p1", Name: "beta", Created: 200})
	if got := a.sidebarDensity(); got != (densityMetrics{rowH: 26, hdrH: 24, fontPx: 12.5, block: true}) {
		t.Errorf("compact metrics %+v", got)
	}
	a.set.SidebarDensity = "standard"
	if got := a.sidebarDensity(); got != (densityMetrics{rowH: 34, hdrH: 28, fontPx: 13.5, block: true}) {
		t.Errorf("standard metrics %+v", got)
	}
	// The rows take their metrics: two sessions' names sit a row's height
	// and margin apart, standard's as it was.
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	sess, _ := tt.Find("sess")
	beta, _ := tt.Find("beta")
	if d := beta.Y - sess.Y; d != 34+1 {
		t.Errorf("standard rows %g apart, want 35", d)
	}
	a.set.SidebarDensity = "compact"
	tt.Frame()
	sess, _ = tt.Find("sess")
	beta, _ = tt.Find("beta")
	if d := beta.Y - sess.Y; d != 26+1 {
		t.Errorf("compact rows %g apart, want 27", d)
	}
}

// Compact, a project's children stand as one block, a bar of the
// project's color down the block's left edge spanning its rows, their
// names inset past the bar — less than standard's badge's room, which
// keeps today's layout, no bar.
func TestCompactBlockIndent(t *testing.T) {
	a := testApp(t)
	a.set.SidebarRows = "compact" // the density's block alone is under test
	a.st.Projects[0].Name = "proj"
	a.st.Sessions[0].Name = "sess"
	a.st.Sessions = append(a.st.Sessions, proto.Session{ID: "s2", Project: "p1", Name: "beta", Created: 200})
	badge := a.projectBadgeColor(a.st.Projects[0])
	col := color.RGBA{R: badge.R, G: badge.G, B: badge.B, A: badge.A}
	tt := ui.NewTester(a.view, 1100, 640)

	// Standard stands in the block too, its bar of the project's color.
	a.set.SidebarDensity = "standard"
	tt.Frame()
	std, ok := tt.Find("sess")
	if !ok {
		t.Fatalf("no session name; texts %q", tt.Texts())
	}
	// Standard stands its children in the block like compact does, the
	// bar of the project's color at its edge: the densities differ in
	// their metrics, not in the block.
	barX := std.X - blockGap - blockBar/2
	if got := tt.Image().RGBAAt(int(barX), int(std.Y+std.H/2)); got == (color.RGBA{}) {
		t.Error("standard shows no bar at the block's edge")
	}

	// Compact: the block's own inset takes the badge's place, and a bar
	// of the project's color runs down its left edge, spanning the rows.
	a.set.SidebarDensity = "compact"
	tt.Frame()
	sess, _ := tt.Find("sess")
	beta, ok := tt.Find("beta")
	if !ok {
		t.Fatalf("no second session; texts %q", tt.Texts())
	}
	if sess.X != std.X {
		t.Errorf("compact children at %g, standard's %g: the block insets both alike", sess.X, std.X)
	}
	barX = sess.X - blockGap - blockBar/2
	at := func(y float32) color.RGBA { return tt.Image().RGBAAt(int(barX), int(y)) }
	// The chosen row's highlight begins at the bar, and its shadow grounds
	// on it: the bar shows the project's color, a shade off at most.
	near := func(y float32) bool {
		got := at(y)
		return abs32(float32(got.R)-float32(col.R)) <= 4 &&
			abs32(float32(got.G)-float32(col.G)) <= 4 &&
			abs32(float32(got.B)-float32(col.B)) <= 4
	}
	for _, y := range []float32{sess.Y + sess.H/2, beta.Y + beta.H/2, (sess.Y + sess.H + beta.Y) / 2} {
		if !near(y) {
			t.Errorf("no bar of the project's color at (%.0f, %.0f): %v", barX, y, at(y))
		}
	}
	// The bar spans the block's rows, and no more.
	past := beta.Y + beta.H/2 + a.sidebarDensity().rowH/2 + 2
	if got := at(past); got == col {
		t.Errorf("the bar runs past the block, to %g", past)
	}
}

// The chosen row's highlight holds the block's gap at its left edge, so
// its text stands clear of it in either theme: the bar tells the block,
// the highlight the row, and neither hugs the text.
func TestCompactBlockChosenHighlight(t *testing.T) {
	a := testApp(t)
	a.st.Projects[0].Name = "proj"
	a.st.Sessions[0].Name = "sess"
	tt := ui.NewTester(a.view, 1100, 640)
	room := func() float32 { // the highlight's left edge, from the text, in its row
		sess, ok := tt.Find("sess")
		if !ok {
			t.Fatalf("no session name; texts %q", tt.Texts())
		}
		img := tt.Image()
		y := int(sess.Y + sess.H/2)
		ref := img.RGBAAt(int(sess.X)-1, y) // inside the highlight, beside the text
		x := int(sess.X) - 2
		for x > 0 && img.RGBAAt(x, y) == ref {
			x--
		}
		return sess.X - float32(x+1)
	}
	tt.Frame()
	tt.Advance(time.Second) // the highlight's color eases in
	tt.Frame()
	if d := room(); d < blockGap-2 {
		t.Errorf("light: the chosen row's highlight begins %g from its text, want %g", d, float32(blockGap))
	}
	tt.SetDark(true)
	tt.Advance(time.Second) // colors ease from the light frame's
	tt.Frame()
	if d := room(); d < blockGap-2 {
		t.Errorf("dark: the chosen row's highlight begins %g from its text, want %g", d, float32(blockGap))
	}
}

// A project's square takes its first letter, and keeps its color by id,
// with a color of its own among the projects.
func TestProjectBadge(t *testing.T) {
	for name, want := range map[string]string{"feature-4.3": "F", "dora-go": "D", "呆蛙精灵": "呆"} {
		if got := projectInitial(name); got != want {
			t.Errorf("%q: %q, want %q", name, got, want)
		}
	}
	a := testApp(t)
	a.st.Projects = append(a.st.Projects,
		proto.Project{ID: "p2", Name: "two"},
		proto.Project{ID: "p3", Name: "three"})
	p1 := proto.Project{ID: "p1", Name: "one"}
	if a.projectBadgeColor(p1) != a.projectBadgeColor(proto.Project{ID: "p1", Name: "renamed"}) {
		t.Error("the badge's color changed with the name")
	}
	seen := map[ui.Color]bool{}
	for _, p := range []proto.Project{p1, {ID: "p2"}, {ID: "p3"}} {
		c := a.projectBadgeColor(p)
		if seen[c] {
			t.Errorf("projects share the color %v", c)
		}
		seen[c] = true
	}
	if a.projectBadgeColor(proto.Project{}) == a.projectBadgeColor(p1) {
		t.Error("the other project has the same color as a project")
	}
}

// Collapsing the project of the session shown hides its sessions too; the
// badge and the chevron still tell which project it is.
func TestCollapseSelectedProject(t *testing.T) {
	a := testApp(t)
	a.st.Projects[0].Name = "proj"
	a.st.Sessions[0].Name = "sess"
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	p, ok := tt.Find("proj")
	if !ok {
		t.Fatalf("no project name; texts %q", tt.Texts())
	}
	// The dot sits over the badge's lower right corner.
	badgeX := p.X - projectBadgeGap - projectBadge
	badgeY := p.Y + p.H/2 - projectBadge/2
	dot := func() color.RGBA {
		return tt.Image().RGBAAt(int(badgeX+projectBadge-1), int(badgeY+projectBadge-1))
	}
	red := func(c color.RGBA) bool { return c.R > 180 && c.G < 140 && c.B < 140 }
	if red(dot()) {
		t.Error("the open project's badge shows the dot")
	}
	tt.ClickAt(p.X+20, p.Y+p.H/2)
	tt.Frame()
	if !a.prefs.Collapsed["p1"] {
		t.Fatal("the project did not collapse")
	}
	if tt.HasText("sess") {
		t.Error("the selected session still shows under its collapsed project")
	}
	if !red(dot()) {
		t.Error("the collapsed project's badge has no red dot for the shown session")
	}
	tt.ClickAt(p.X+20, p.Y+p.H/2)
	tt.Frame()
	if a.prefs.Collapsed["p1"] || !tt.HasText("sess") {
		t.Error("the project did not open again")
	}
	if red(dot()) {
		t.Error("the opened project's badge still shows the dot")
	}
}

// dragSessions is the app of the session-drag tests: three sessions of
// p1 (demo s1, beta s2, gamma s3, by created) and one of p2 (delta s4),
// with a daemon that records each session.move.
func dragSessions(t *testing.T) (*App, *ui.Tester, chan proto.MoveSessionParams) {
	t.Helper()
	a := testApp(t)
	a.st.Projects = append(a.st.Projects, proto.Project{ID: "p2", Name: "two"})
	a.st.Sessions = append(a.st.Sessions,
		proto.Session{ID: "s2", Project: "p1", Name: "beta", Created: 200},
		proto.Session{ID: "s3", Project: "p1", Name: "gamma", Created: 300},
		proto.Session{ID: "s4", Project: "p2", Name: "delta", Created: 400})
	moves := make(chan proto.MoveSessionParams, 16)
	a.local().conn = connReady
	a.local().cl, _ = projectClient(t, func(method string, raw json.RawMessage) (any, error) {
		if method == "session.move" {
			var p proto.MoveSessionParams
			if err := json.Unmarshal(raw, &p); err == nil {
				moves <- p
			}
		}
		return nil, nil
	})
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	return a, tt, moves
}

// dragged moves the row at src onto dst, as the project-drag test does.
func dragged(tt *ui.Tester, src, dst ui.Rect) {
	tt.Press(src.X+10, src.Y+src.H/2)
	for i := 1; i <= 10; i++ {
		f := float32(i) / 10
		x := src.X + 10
		y := src.Y + src.H/2 + (dst.Y+dst.H-4-src.Y-src.H/2)*f
		tt.Move(x, y)
		tt.Advance(16 * time.Millisecond)
		tt.Frame()
	}
	tt.Release(dst.X+10, dst.Y+dst.H-4)
	tt.Frame()
	tt.Advance(50 * time.Millisecond)
	tt.Frame()
}

// moved is the one session.move the test expects, or none at all.
func moved(t *testing.T, moves chan proto.MoveSessionParams, want *proto.MoveSessionParams) {
	t.Helper()
	select {
	case got := <-moves:
		if want == nil {
			t.Fatalf("session.move %+v, want none", got)
		}
		if got.Session != want.Session || got.Pin != want.Pin {
			t.Fatalf("session.move %+v, want session %q pin %q", got, want.Session, want.Pin)
		}
		switch {
		case got.Project == nil && want.Project != nil:
			t.Fatalf("session.move without a project, want %q", *want.Project)
		case got.Project != nil && want.Project == nil:
			t.Fatalf("session.move to project %q, want none", *got.Project)
		case got.Project != nil && *got.Project != *want.Project:
			t.Fatalf("session.move to project %q, want %q", *got.Project, *want.Project)
		}
	case <-time.After(2 * time.Second):
		if want != nil {
			t.Fatal("no session.move call")
		}
	}
}

// A session dragged onto another row of its host pins after that row,
// onto another project's header goes to its head, and across hosts pins
// nothing: a session cannot leave the kerod that owns its terminal.
func TestSessionDrag(t *testing.T) {
	t.Run("row to row", func(t *testing.T) {
		_, tt, moves := dragSessions(t)
		src, ok := tt.Find("gamma")
		if !ok {
			t.Fatalf("no gamma row; texts %q", tt.Texts())
		}
		dst, ok := tt.Find("beta")
		if !ok {
			t.Fatalf("no beta row; texts %q", tt.Texts())
		}
		dragged(tt, src, dst)
		moved(t, moves, &proto.MoveSessionParams{Session: "s3", Pin: "s2"})
	})
	t.Run("onto another project's header", func(t *testing.T) {
		_, tt, moves := dragSessions(t)
		src, ok := tt.Find("gamma")
		if !ok {
			t.Fatalf("no gamma row; texts %q", tt.Texts())
		}
		dst, ok := tt.Find("two")
		if !ok {
			t.Fatalf("no two project; texts %q", tt.Texts())
		}
		dragged(tt, src, dst)
		two := "p2"
		moved(t, moves, &proto.MoveSessionParams{Session: "s3", Pin: "head", Project: &two})
	})
	t.Run("across hosts", func(t *testing.T) {
		a, tt, moves := dragSessions(t)
		h := &host{name: "remote", conn: connReady, loaded: true}
		a.hosts = append(a.hosts, h)
		a.st.Sessions = append(a.st.Sessions, proto.Session{ID: "r1", Name: "remote one", Created: 500})
		a.owner = map[string]*host{"r1": h}
		tt.Frame()
		src, ok := tt.Find("demo")
		if !ok {
			t.Fatalf("no demo row; texts %q", tt.Texts())
		}
		dst, ok := tt.Find("remote one")
		if !ok {
			t.Fatalf("no remote row; texts %q", tt.Texts())
		}
		dragged(tt, src, dst)
		time.Sleep(200 * time.Millisecond)
		moved(t, moves, nil)
	})
}

// Reset to Auto Order takes a session's pin off, back to where the
// order the sidebar keeps would put it.
func TestSessionReset(t *testing.T) {
	_, tt, moves := dragSessions(t)
	if err := tt.RightClick("gamma"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem(locales.SidebarResetOrder()); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	moved(t, moves, &proto.MoveSessionParams{Session: "s3"})
}

// ⌘1–9 (Ctrl+1–9 elsewhere) reach the rows as the sidebar shows them:
// the pins' order, and, a project folded away with the session shown in
// it, none of its rows — the shortcuts follow what is on the screen.
func TestSidebarShortcutOrder(t *testing.T) {
	a := testApp(t)
	a.st.Sessions[0].Name = "alpha" // not "demo": the project takes that name
	a.st.Sessions = append(a.st.Sessions,
		proto.Session{ID: "s2", Project: "p1", Name: "beta", Created: 200},
		proto.Session{ID: "s3", Project: "p1", Name: "gamma", Pin: "s1", Created: 300})
	a.st.Projects = append(a.st.Projects, proto.Project{ID: "p2", Name: "two"})
	a.st.Sessions = append(a.st.Sessions,
		proto.Session{ID: "s4", Project: "p2", Name: "delta", Created: 400},
		proto.Session{ID: "s5", Project: "p2", Name: "epsilon", Created: 500})
	a.prefs.Collapsed = map[string]bool{"p2": true}
	a.selected = "s5"                  // in the folded project, hidden with it
	want := []string{"s1", "s3", "s2"} // gamma pinned after demo
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	if got := a.visibleOrder(); !slices.Equal(got, want) {
		t.Fatalf("visibleOrder %v, want %v", got, want)
	}
	for _, name := range []string{"delta", "epsilon"} {
		if tt.HasText(name) {
			t.Errorf("%s shows in the folded project", name)
		}
	}
	tt.HoldModifiers(ui.Cmd)
	tt.Frame()
	names := map[string]string{"s1": "alpha", "s2": "beta", "s3": "gamma"}
	for i, id := range want {
		row, ok := tt.Find(names[id])
		if !ok {
			t.Fatalf("no %s row; texts %q", names[id], tt.Texts())
		}
		key, ok := tt.Find(actions.Glyphs(fmt.Sprintf("CmdOrCtrl+%d", i+1)))
		if !ok {
			t.Fatalf("row %d (%s) shows no ⌘%d", i+1, names[id], i+1)
		}
		// The number stands in the row it names: their centers share a row.
		if d := key.Y + key.H/2 - (row.Y + row.H/2); d < -rowH/2 || d > rowH/2 {
			t.Errorf("⌘%d centers %g off the %s row", i+1, d, names[id])
		}
	}
	if _, ok := tt.Find(actions.Glyphs("CmdOrCtrl+4")); ok {
		t.Error("a fourth ⌘ number shows, past the rows on the screen")
	}
}
