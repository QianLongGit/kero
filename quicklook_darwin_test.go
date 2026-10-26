//go:build darwin

package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ebitengine/purego"
)

var (
	// qlCalls carries what the tests ask of the main thread, and qlMainErr
	// says why that thread cannot host the panel, nil while it can.
	qlCalls   chan func()
	qlMainErr error
)

// AppKit makes windows — the Quick Look panel is one — only on the
// process's main thread, which a test's own goroutine is not. So the
// tests run as they always do, in testing's goroutine, while this main
// thread, held for the purpose, runs what they ask of it: the one place
// in the process Quick Look may be called from, as the app's main thread
// is the other.
func TestMain(m *testing.M) {
	runtime.LockOSThread()
	if lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW); err != nil {
		qlMainErr = err
	} else {
		var mainThread func() int32
		purego.RegisterLibFunc(&mainThread, lib, "pthread_main_np")
		if mainThread() == 0 {
			qlMainErr = errors.New("the goroutine this began on is not the process's main thread")
		}
	}
	calls := make(chan func())
	qlCalls = calls
	code := make(chan int, 1)
	go func() {
		code <- m.Run()
		close(calls)
	}()
	for fn := range calls {
		fn()
	}
	os.Exit(<-code)
}

// qlOnMain runs fn on the main thread TestMain holds, and hands its
// error back.
func qlOnMain(fn func() error) error {
	errs := make(chan error, 1)
	qlCalls <- func() { errs <- fn() }
	return <-errs
}

// Quick Look previews a file in Apple's panel, bound once and shared by
// every preview after: a second one, of another file, swaps the panel's
// item for it. What the panel looks like is told by hand; this holds the
// binding layer to staying alive through both calls.
func TestQuickLook(t *testing.T) {
	if qlMainErr != nil {
		t.Skipf("no main thread to host the panel: %v", qlMainErr)
	}
	for i, name := range []string{"note.txt", "other.txt"} {
		f := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(f, []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := qlOnMain(func() error { return quickLook(f) }); err != nil {
			t.Fatalf("preview %d of %s: %v", i+1, name, err)
		}
	}
}
