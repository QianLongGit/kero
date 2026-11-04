package main

// The drag order against a real kerod, spawned as the app spawns it: a
// session.move's pin reaches the state stream the app shows, through
// the broadcasts a busy daemon sends around it.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"kero/internal/client"
	"kero/internal/proto"
)

// kerodBin builds the app's binary once for these tests: the daemon
// they spawn is the app's own __kerod, as the app's is. "" when the
// build cannot run here, and the tests skip.
var kerodBin = sync.OnceValue(func() string {
	bin := filepath.Join(os.TempDir(), fmt.Sprintf("kero-kerod-test-%d", os.Getpid()))
	if err := exec.Command("go", "build", "-o", bin, ".").Run(); err != nil {
		return ""
	}
	return bin
})

// spawnKerod starts a daemon of the built app in its own home and
// answers with a client connected to it.
func spawnKerod(t *testing.T) *client.Client {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KERO_HOME", home)
	t.Setenv("KERO_CONFIG", filepath.Join(home, "config"))
	t.Setenv("KERO_SOCKET", "")
	bin := kerodBin()
	if bin == "" {
		t.Skip("the app's binary does not build here")
	}
	cmd := exec.Command(bin, "__kerod")
	cmd.Env = append(os.Environ(), "KERO_HOME="+home, "KERO_CONFIG="+filepath.Join(home, "config"))
	cmd.Dir = home
	logf, _ := os.Create(filepath.Join(home, "kerod-test.log"))
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait(); logf.Close() })

	var cl *client.Client
	deadline := time.Now().Add(10 * time.Second)
	for {
		var err error
		cl, err = client.Connect(true)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("kerod never came: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(func() { cl.Close() })
	return cl
}

// stateStream takes the daemon's state broadcasts onto ch.
func stateStream(t *testing.T, cl *client.Client) chan proto.State {
	t.Helper()
	states := make(chan proto.State, 256)
	cl.OnEvent(func(m proto.Message) {
		if m.Event == "state" && m.State != nil {
			select {
			case states <- *m.State:
			default:
			}
		}
	})
	if err := cl.Call("subscribe", nil, nil); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return states
}

func statePin(st proto.State, id string) string {
	for _, s := range st.Sessions {
		if s.ID == id {
			return s.Pin
		}
	}
	return "<gone>"
}

// A session.move while the daemon keeps broadcasting — a program
// writing, as around a real drag — lands in the stream: some state on
// the way may still come without the pin, but one with it follows.
func TestSidebarMoveReachesTheStateStream(t *testing.T) {
	cl := spawnKerod(t)
	states := stateStream(t, cl)

	var s1, s2 proto.Session
	if err := cl.Call("create", proto.CreateParams{Name: "one", Command: []string{"sleep", "600"}}, &s1); err != nil {
		t.Fatalf("create s1: %v", err)
	}
	if err := cl.Call("create", proto.CreateParams{Name: "two", Command: []string{"sleep", "600"}}, &s2); err != nil {
		t.Fatalf("create s2: %v", err)
	}
	// A program at work drives the daemon's broadcasts, as a drag's
	// round trip sits among them.
	if err := cl.Call("send", proto.SendParams{ID: s1.ID, Data: "echo a; sleep 0.2; echo b; sleep 0.2; echo c; sleep 5\n"}, nil); err != nil {
		t.Logf("send skipped: %v", err)
	}
	for {
		select {
		case <-states:
			continue
		default:
		}
		break
	}

	moved := make(chan error, 1)
	go func() { moved <- cl.Call("session.move", proto.MoveSessionParams{Session: s1.ID, Pin: s2.ID}, nil) }()
	var saw []string
	for i := 0; i < 40; i++ {
		select {
		case st := <-states:
			saw = append(saw, statePin(st, s1.ID))
		case <-time.After(400 * time.Millisecond):
			i = 40
		}
	}
	if err := <-moved; err != nil {
		t.Fatalf("session.move: %v", err)
	}
	for _, p := range saw {
		if p == s2.ID {
			return // the pin landed in the stream the app shows
		}
	}
	t.Errorf("no state ever carried the pin: %v", saw)
}

// A quiet daemon's first state after a move carries the pin: nothing
// stands between the move and the broadcast.
func TestSidebarQuietMoveBroadcastsAtOnce(t *testing.T) {
	cl := spawnKerod(t)
	states := stateStream(t, cl)

	var s1, s2 proto.Session
	if err := cl.Call("create", proto.CreateParams{Name: "one", Command: []string{"sleep", "600"}}, &s1); err != nil {
		t.Fatalf("create s1: %v", err)
	}
	if err := cl.Call("create", proto.CreateParams{Name: "two", Command: []string{"sleep", "600"}}, &s2); err != nil {
		t.Fatalf("create s2: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // the creates' broadcasts land
	for {
		select {
		case <-states:
			continue
		default:
		}
		break
	}

	t0 := time.Now()
	if err := cl.Call("session.move", proto.MoveSessionParams{Session: s1.ID, Pin: s2.ID}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case st := <-states:
		t.Logf("first state at +%4.1fms", float64(time.Since(t0).Microseconds())/1000)
		if got := statePin(st, s1.ID); got != s2.ID {
			t.Errorf("the first state after the move carried pin=%q, want %q", got, s2.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no state after the move")
	}
}
