package main

// The sidebar's drag order: a dropped session stands where the user put
// it — across project groups, through a state the daemon sent before the
// drag's answer, and over a second drag that would circle the pins.

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"kero/internal/proto"
	"kero/locales"
)

// dragApp is the app with two projects' sessions and a client that takes
// every call, telling the test what the sidebar's drags sent.
func dragApp(t *testing.T) (*App, *ui.Tester, chan proto.MoveSessionParams) {
	t.Helper()
	a := testApp(t)
	a.st.Sessions[0].Name = "alpha" // not "demo": the project takes that name
	a.st.Projects = append(a.st.Projects, proto.Project{ID: "p2", Name: "two"})
	a.st.Sessions = append(a.st.Sessions,
		proto.Session{ID: "s2", Project: "p1", Name: "beta", Created: 200},
		proto.Session{ID: "s3", Project: "p1", Name: "gamma", Created: 300},
		proto.Session{ID: "s4", Project: "p2", Name: "delta", Created: 400})
	moves := make(chan proto.MoveSessionParams, 16)
	a.local().conn = connReady
	// Attached, as a running app's sessions are: no dial in the
	// background, whose failure would write off the app's thread.
	a.terms["a"], a.terms["b"] = &termEntry{}, &termEntry{}
	a.local().cl, _ = projectClient(t, func(method string, raw json.RawMessage) (any, error) {
		if method == "session.move" {
			var p proto.MoveSessionParams
			if err := json.Unmarshal(raw, &p); err == nil {
				moves <- p
			}
		}
		return nil, nil
	})
	a.updateFn = func(fn func()) { fn() } // as a window marshals the updates
	tt := ui.NewTester(a.view, 1100, 640)
	tt.Frame()
	return a, tt, moves
}

// snapshotState copies the state the app shows as the daemon's broadcast
// would: fresh objects, decoded anew.
func snapshotState(a *App) proto.State {
	b, err := json.Marshal(a.st)
	if err != nil {
		panic(err)
	}
	var st proto.State
	if err := json.Unmarshal(b, &st); err != nil {
		panic(err)
	}
	return st
}

// takeState merges st as the daemon's state event does, through the path
// that replaces a host's sessions.
func takeState(a *App, st proto.State) { a.mergeState(a.local(), st) }

// dragRow moves the pointer from src's row onto dst's and lets go there,
// running inject between the drag's steps, as a broadcast may land
// mid-drag.
func dragRow(tt *ui.Tester, src, dst ui.Rect, inject func(step int)) {
	tt.Press(src.X+10, src.Y+src.H/2)
	for i := 1; i <= 10; i++ {
		f := float32(i) / 10
		x := src.X + 10
		y := src.Y + src.H/2 + (dst.Y+dst.H-4-src.Y-src.H/2)*f
		tt.Move(x, y)
		if inject != nil {
			inject(i)
		}
		tt.Advance(16 * time.Millisecond)
		tt.Frame()
	}
	tt.Release(dst.X+10, dst.Y+dst.H-4)
	tt.Frame()
	tt.Advance(50 * time.Millisecond)
	tt.Frame()
}

// moveSent takes the session.move the drag sent, if it came.
func moveSent(moves chan proto.MoveSessionParams) (proto.MoveSessionParams, bool) {
	select {
	case got := <-moves:
		return got, true
	case <-time.After(500 * time.Millisecond):
		return proto.MoveSessionParams{}, false
	}
}

func pinOf(a *App, id string) string {
	for i := range a.st.Sessions {
		if a.st.Sessions[i].ID == id {
			return a.st.Sessions[i].Pin
		}
	}
	return "<gone>"
}

func wantOrder(t *testing.T, a *App, want []string, what string) {
	t.Helper()
	if got := a.visibleOrder(); !slices.Equal(got, want) {
		t.Errorf("%s: sidebar order %v, want %v (pin s1=%q)", what, got, want, pinOf(a, "s1"))
	}
}

// A drag inside one group lifts the session after its anchor and it
// stays there: the order the drop left is the order the sidebar shows.
func TestSidebarDragStopsWhereDropped(t *testing.T) {
	a, tt, moves := dragApp(t)
	src, _ := tt.Find("alpha")
	dst, _ := tt.Find("gamma")
	dragRow(tt, src, dst, nil)
	if p, ok := moveSent(moves); !ok || p.Pin != "s3" {
		t.Fatalf("drag sent %+v (came %v), want a pin at s3", p, ok)
	}
	wantOrder(t, a, []string{"s2", "s3", "s1", "s4"}, "drag in one group")
}

// Dropped onto a row of no project, the session moves into that loose
// group with its pin — the pin an empty project names stands there, not
// back in the group the session left.
func TestSidebarDropIntoNoProjectMovesTheSession(t *testing.T) {
	a, tt, moves := dragApp(t)
	a.st.Sessions = append(a.st.Sessions, proto.Session{ID: "s5", Name: "lone", Created: 500})
	tt.Frame()
	src, ok := tt.Find("alpha")
	if !ok {
		t.Fatal("no alpha row")
	}
	dst, ok := tt.Find("lone")
	if !ok {
		t.Fatalf("no lone row; texts %q", tt.Texts())
	}
	dragRow(tt, src, dst, nil)
	p, ok := moveSent(moves)
	if !ok {
		t.Fatal("the drop sent no session.move")
	}
	if p.Pin != "s5" {
		t.Errorf("move pinned at %q, want s5", p.Pin)
	}
	if p.Project == nil || *p.Project != "" {
		t.Errorf("move project %v, want a pointer at \"\" — the group of none", p.Project)
	}
	if got := pinOf(a, "s1"); got != "s5" {
		t.Errorf("pin(s1)=%q, want s5", got)
	}
	if s := a.session("s1"); s == nil || s.Project != "" {
		t.Errorf("session project %q, want none", func() string {
			if s := a.session("s1"); s != nil {
				return s.Project
			}
			return "<gone>"
		}())
	}
	wantOrder(t, a, []string{"s2", "s3", "s4", "s5", "s1"}, "drop into no project")
}

// Dropped onto the header of the sessions without a project, the session
// heads that loose group — pin "head" and an empty project, not a head
// pin left in the group it came from.
func TestSidebarDropOntoNoProjectHeaderHeadsIt(t *testing.T) {
	a, tt, moves := dragApp(t)
	a.st.Sessions = append(a.st.Sessions, proto.Session{ID: "s5", Name: "lone", Created: 500})
	tt.Frame()
	src, ok := tt.Find("alpha")
	if !ok {
		t.Fatal("no alpha row")
	}
	dst, ok := tt.Find(locales.SidebarOtherProject())
	if !ok {
		t.Fatalf("no header for the sessions without a project; texts %q", tt.Texts())
	}
	dragRow(tt, src, dst, nil)
	p, ok := moveSent(moves)
	if !ok {
		t.Fatal("the drop sent no session.move")
	}
	if p.Pin != "head" {
		t.Errorf("move pinned at %q, want head", p.Pin)
	}
	if p.Project == nil || *p.Project != "" {
		t.Errorf("move project %v, want a pointer at \"\" — the group of none", p.Project)
	}
	if got := pinOf(a, "s1"); got != "head" {
		t.Errorf("pin(s1)=%q, want head", got)
	}
	if s := a.session("s1"); s == nil || s.Project != "" {
		t.Errorf("session project %q, want none", func() string {
			if s := a.session("s1"); s != nil {
				return s.Project
			}
			return "<gone>"
		}())
	}
	wantOrder(t, a, []string{"s2", "s3", "s4", "s1", "s5"}, "drop onto the no-project header")
}

// Dragging two sessions under each other in turn would circle their
// pins, and a circle voids every pin on it: the older pin gives way, and
// both drags stand — the second drag lands, it does not bounce both
// sessions back.
func TestSidebarPinSwapBreaksTheCycle(t *testing.T) {
	a, tt, moves := dragApp(t)
	// alpha under gamma: pin(s1)=s3, order [s2 s3 s1 s4].
	src, _ := tt.Find("alpha")
	dst, _ := tt.Find("gamma")
	dragRow(tt, src, dst, nil)
	if p, ok := moveSent(moves); !ok || p.Pin != "s3" {
		t.Fatalf("first drag sent %+v (came %v), want a pin at s3", p, ok)
	}
	f1 := ui.NewTester(a.view, 1100, 640)
	f1.Frame()
	// gamma under alpha — a pin that would circle back: pin(s1) gives way.
	src2, ok := f1.Find("gamma")
	if !ok {
		t.Fatal("no gamma row after the first drag")
	}
	dst2, ok := f1.Find("alpha")
	if !ok {
		t.Fatal("no alpha row after the first drag")
	}
	dragRow(f1, src2, dst2, nil)
	if p, ok := moveSent(moves); !ok || p.Pin != "s1" {
		t.Fatalf("second drag sent %+v (came %v), want a pin at s1", p, ok)
	}
	f2 := ui.NewTester(a.view, 1100, 640)
	f2.Frame()
	if got := a.visibleOrder(); slices.Equal(got, []string{"s1", "s2", "s3", "s4"}) {
		t.Fatalf("both drags bounced back to the original order %v: the pin circle voided them", got)
	}
	// s3 under s1, s1 back in the auto order.
	wantOrder(t, a, []string{"s1", "s3", "s2", "s4"}, "pin swap")
}

// A stale state can bring back the pin a second drag's chain broke:
// the hold puts its own drag back and breaks the chain again, so the
// circle does not void both pins and drop the rows to the auto order
// while the answer is out.
func TestSidebarPinCycleBrokenThroughAStaleState(t *testing.T) {
	a, tt, moves := dragApp(t)
	// alpha under gamma, carried by the daemon's state.
	src, _ := tt.Find("alpha")
	dst, _ := tt.Find("gamma")
	dragRow(tt, src, dst, nil)
	if p, ok := moveSent(moves); !ok || p.Pin != "s3" {
		t.Fatalf("first drag sent %+v (came %v), want a pin at s3", p, ok)
	}
	takeState(a, snapshotState(a)) // the state that carries the pin
	f1 := ui.NewTester(a.view, 1100, 640)
	f1.Frame()

	// gamma under alpha: pin(s3)=s1, pin(s1) broken. A state from before
	// that answer carries pin(s1)=s3 back — a circle, which the hold
	// breaks again rather than let void both drags.
	stale := snapshotState(a)
	src2, ok := f1.Find("gamma")
	if !ok {
		t.Fatal("no gamma row after the first drag")
	}
	dst2, ok := f1.Find("alpha")
	if !ok {
		t.Fatal("no alpha row after the first drag")
	}
	dragRow(f1, src2, dst2, nil)
	if p, ok := moveSent(moves); !ok || p.Pin != "s1" {
		t.Fatalf("second drag sent %+v (came %v), want a pin at s1", p, ok)
	}
	takeState(a, stale)
	f2 := ui.NewTester(a.view, 1100, 640)
	f2.Frame()
	wantOrder(t, a, []string{"s1", "s3", "s2", "s4"}, "a stale state over a broken chain")
}

// A state the daemon sent before the drag's answer — a broadcast in
// flight while the move's RPC runs — cannot stand the row back where it
// was: the pin the app set at once holds over it, until the state that
// carries the pin confirms it; after that a state without the pin is
// the daemon's word, and the app takes it.
func TestSidebarDragHoldsThePinThroughStates(t *testing.T) {
	a, tt, moves := dragApp(t)
	src, _ := tt.Find("alpha")
	dst, _ := tt.Find("gamma")
	stale := snapshotState(a) // before the drop: no pins
	dragRow(tt, src, dst, nil)
	if p, ok := moveSent(moves); !ok || p.Pin != "s3" {
		t.Fatalf("drag sent %+v (came %v), want a pin at s3", p, ok)
	}
	fresh := snapshotState(a) // after the drop: the pin the daemon keeps

	takeState(a, stale) // the broadcast that left before the answer
	f := ui.NewTester(a.view, 1100, 640)
	f.Frame()
	if got := pinOf(a, "s1"); got != "s3" {
		t.Fatalf("a stale state took the pin back: pin(s1)=%q", got)
	}
	wantOrder(t, a, []string{"s2", "s3", "s1", "s4"}, "through a stale state")

	takeState(a, fresh) // the state the move made
	f.Frame()
	if got := pinOf(a, "s1"); got != "s3" {
		t.Fatalf("the fresh state lost the pin: pin(s1)=%q", got)
	}

	takeState(a, stale) // a state without the pin, after it confirmed
	f.Frame()
	if got := pinOf(a, "s1"); got != "" {
		t.Errorf("the hold outlived its confirmation: pin(s1)=%q, want none — the daemon's word", got)
	}
	wantOrder(t, a, []string{"s1", "s2", "s3", "s4"}, "after the pin confirmed")
}

// marshalUpdates stands in for a window's main thread, as a window
// marshals the app's updates: a client's answer runs its change on one
// goroutine, and the test's looks run there too, after them — the race
// detector sees the order the app itself keeps. It returns the
// look-on-the-app function.
func marshalUpdates(t *testing.T, a *App) func(look func()) {
	t.Helper()
	upd := make(chan func())
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for fn := range upd {
			fn()
		}
	}()
	t.Cleanup(func() { close(upd); <-gone })
	a.updateFn = func(fn func()) { upd <- fn }
	return func(look func()) {
		ran := make(chan struct{})
		upd <- func() { look(); close(ran) }
		<-ran
	}
}

// A move the daemon refuses — a host of an older make, one that knows
// no empty project — rolls what the app showed at once back to what
// stood before the drag, says why, and holds nothing.
func TestSidebarRefusedMoveRollsBack(t *testing.T) {
	a := testApp(t)
	a.st.Sessions[0].Name = "alpha"
	a.st.Sessions = append(a.st.Sessions, proto.Session{ID: "s2", Project: "p1", Name: "beta", Created: 200})
	a.local().cl, _ = projectClient(t, func(method string, _ json.RawMessage) (any, error) {
		if method == "session.move" {
			return nil, errors.New("no project \"\"")
		}
		return nil, nil
	})
	onApp := marshalUpdates(t, a)

	a.moveSession("s1", "s2", nil)
	// The refusal's answer comes on the client's goroutine; the rollback
	// with it, and the test's looks after.
	var pin, toast string
	var held int
	deadline := time.Now().Add(2 * time.Second)
	for {
		onApp(func() { pin, toast, held = pinOf(a, "s1"), a.pendingToast, len(a.pendingMoves) })
		if toast != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if pin != "" {
		t.Errorf("a refused move left pin=%q, want none", pin)
	}
	if toast == "" {
		t.Error("a refused move said nothing")
	}
	if held != 0 {
		t.Errorf("a refused move still holds %d", held)
	}
}

// A drag's hold is bounded: past its time, a state that carries no word
// of the move takes the sessions as the daemon has them.
func TestSidebarMoveHoldTimesOut(t *testing.T) {
	a := testApp(t)
	a.st.Sessions[0].Name = "alpha"
	a.st.Sessions = append(a.st.Sessions, proto.Session{ID: "s2", Project: "p1", Name: "beta", Created: 200})
	a.local().cl, _ = projectClient(t, func(string, json.RawMessage) (any, error) {
		return nil, nil
	})
	a.updateFn = func(fn func()) { fn() }

	stale := snapshotState(a) // before the move: no pin
	a.moveSession("s1", "s2", nil)
	k := moveKey{a.local(), "s1"}
	if m, ok := a.pendingMoves[k]; ok {
		m.at = time.Now().Add(-moveHold - time.Second)
		a.pendingMoves[k] = m
	} else {
		t.Fatal("the drag held nothing")
	}
	takeState(a, stale)
	f := ui.NewTester(a.view, 1100, 640)
	f.Frame()
	if got := pinOf(a, "s1"); got != "" {
		t.Errorf("an expired hold kept pin=%q", got)
	}
	if _, ok := a.pendingMoves[k]; ok {
		t.Error("an expired hold stayed")
	}
}

// A refusal that lands after another drag took the session — the first
// answer out for its round trip, the user already at the row again —
// rolls back nothing: the later drag's show and hold stand, and the
// refusal says why the first one failed.
func TestSidebarRefusedMoveSparesALaterDrag(t *testing.T) {
	a := testApp(t)
	a.st.Sessions[0].Name = "alpha"
	a.st.Sessions = append(a.st.Sessions,
		proto.Session{ID: "s2", Project: "p1", Name: "beta", Created: 200},
		proto.Session{ID: "s3", Project: "p1", Name: "gamma", Created: 300})
	release := make(chan struct{})
	a.local().cl, _ = projectClient(t, func(method string, raw json.RawMessage) (any, error) {
		if method == "session.move" {
			var p proto.MoveSessionParams
			if err := json.Unmarshal(raw, &p); err == nil && p.Pin == "s2" {
				// The first drag's refusal holds until the second drag
				// stands, as a slow answer met a quick hand does.
				<-release
				return nil, errors.New("no project \"\"")
			}
		}
		return nil, nil
	})
	onApp := marshalUpdates(t, a)

	a.moveSession("s1", "s2", nil) // to beta — refused, but the answer holds
	a.moveSession("s1", "s3", nil) // to gamma, before the answer comes
	close(release)
	var pin, toast string
	var held int
	deadline := time.Now().Add(2 * time.Second)
	for {
		onApp(func() { pin, toast, held = pinOf(a, "s1"), a.pendingToast, len(a.pendingMoves) })
		if toast != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if pin != "s3" {
		t.Errorf("the refusal rolled the later drag back: pin=%q, want s3", pin)
	}
	if held != 1 {
		t.Errorf("the refusal dropped the later drag's hold: %d held, want 1", held)
	}
	if toast == "" {
		t.Error("the refusal said nothing")
	}
}
