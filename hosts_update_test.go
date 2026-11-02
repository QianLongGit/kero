package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"kero/internal/settings"

	"kero/locales"
)

// asReleased makes the test's build a released one for behind(), which a
// development build never is: a version of its own, and not IsDev's. What
// it changed of the app it gives back.
func asReleased(t *testing.T, version string) {
	t.Helper()
	t.Setenv("MYGO_ENV", "production")
	mygo.App.SetVersion(version)
	t.Cleanup(func() { mygo.App.SetVersion("") })
}

// The Hosts page's switch turns the sidebar's update marks on, and saves.
func TestHostUpdateHintsSetting(t *testing.T) {
	a := testApp(t)
	a.ss.page = "hosts"
	tt := ui.NewTester(a.settingsView, 820, 1100)
	tt.Frame()
	r, ok := tt.Find(locales.SetUpdateHints())
	if !ok {
		t.Fatalf("no Update hints row; texts %q", tt.Texts())
	}
	if a.set.HostUpdateHints || settings.Load().HostUpdateHints {
		t.Fatal("update hints are on before the user asks")
	}
	tt.ClickAt(752, r.Y+r.H/2) // its switch, at the row's end
	tt.Frame()
	tt.Frame()
	if !a.set.HostUpdateHints || !settings.Load().HostUpdateHints {
		t.Fatalf("hints %t, saved %t", a.set.HostUpdateHints, settings.Load().HostUpdateHints)
	}
}

// A host behind the app tells so in the sidebar only while the user asked
// for update hints; one of a version the app can't even talk to says so
// whatever, as what it asks for is a fix, not an update.
func TestSidebarUpdateHintGate(t *testing.T) {
	asReleased(t, "2.3.26")
	a := testApp(t)
	a.mergeState(a.local(), a.st)
	h := withHost(t, a, "build")
	h.info.Updates = true
	if !h.behind() {
		t.Fatal("the test's host is not behind")
	}
	tt := ui.NewTester(a.view, 1100, 700)
	tt.Frame()
	if slices.Contains(tt.Texts(), locales.CommonUpdate()) {
		t.Error("an update chip shows while the setting is off")
	}
	a.set.HostUpdateHints = true
	tt.Frame()
	if !slices.Contains(tt.Texts(), locales.CommonUpdate()) {
		t.Error("no update chip while the setting is on")
	}
	shot(t, tt, "sidebar-update-hint")
	a.set.HostUpdateHints = false
	h.conn, h.errKind = connVersion, nil
	tt.Frame()
	if !slices.Contains(tt.Texts(), locales.CommonUpdate()) {
		t.Error("no update chip for a host of another protocol")
	}
}

// Update all hosts takes every host behind the app, or of a version it
// can't talk to, through one question, and installs on each what the one
// host's update does — a host that stands fine stays untouched.
func TestUpdateAllHosts(t *testing.T) {
	asReleased(t, "2.3.26")
	a := testApp(t)
	build := withHost(t, a, "build")
	fine := withHost(t, a, "gpu")
	build.conn, build.errKind = connVersion, nil
	a.ss.page = "hosts"
	tt := ui.NewTester(a.settingsView, 820, 1100)
	tt.Frame()
	if err := tt.Click(locales.CommonUpdate()); err != nil {
		t.Fatalf("no Update button: %v; texts %q", err, tt.Texts())
	}
	tt.Frame()
	if a.ss.confirm == nil || a.ss.confirm.title != locales.HostUpdateAllTitle(1) {
		t.Fatalf("no question: %+v", a.ss.confirm)
	}
	shot(t, tt, "settings-update-all")
	// What the install's goroutine tells the app queues for here, as a
	// window would marshal it onto its thread.
	updates := make(chan func(), 4)
	a.updateFn = func(fn func()) { updates <- fn }
	tt.Key(0, ui.KeyEnter)
	if build.busy != "install" {
		t.Fatalf("build is not installing: %q", build.busy)
	}
	if fine.busy != "" {
		t.Fatalf("gpu, which stands fine, installs too: %q", fine.busy)
	}
	// The install reaches a host that isn't there over SSH, which fails;
	// its word of it comes back on the app's thread.
	for build.busy != "" {
		select {
		case fn := <-updates:
			fn()
		case <-time.After(15 * time.Second):
			t.Fatalf("the install never ended: busy %q, toast %q", build.busy, a.pendingToast)
		}
	}
	if !strings.Contains(a.pendingToast, "build") {
		t.Errorf("no word of the failure: %q", a.pendingToast)
	}
}
