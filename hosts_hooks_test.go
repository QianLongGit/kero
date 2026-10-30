package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"kero/internal/proto"

	"kero/locales"
)

// The hooks row words each state a host's kerod tells of its agent hooks
// with the skills' words, the row's own while none has come.
func TestHostHooksDetail(t *testing.T) {
	for state, want := range map[string]string{
		"current":  locales.SetSkillInstalled(),
		"outdated": locales.SetSkillOutdated(),
		"missing":  locales.SetSkillMissing(),
		"":         locales.SetHostHooksDetail(),
	} {
		if got := hooksDetail(state); got != want {
			t.Errorf("hooksDetail(%q) = %q, want %q", state, got, want)
		}
	}
}

// The row offers a remove while hooks are on the host, an install — an
// update, when an older Kero wrote the ones there — while they aren't
// current, and nothing while it doesn't know.
func TestHostHooksOffers(t *testing.T) {
	for _, c := range []struct {
		state            string
		remove, install  bool
	}{
		{"current", true, false},
		{"outdated", true, true},
		{"missing", false, true},
		{"", false, false},
	} {
		remove, install := hooksOffers(c.state)
		if remove != c.remove || install != c.install {
			t.Errorf("hooksOffers(%q) = %t, %t; want %t, %t", c.state, remove, install, c.remove, c.install)
		}
	}
}

// The hooks actions reach the host's kerod as the hooks RPCs, with the
// agent named; a host the app can't reach says so, with no call.
func TestHostHooksAction(t *testing.T) {
	a := testApp(t)
	h := withHost(t, a, "build")

	a.hostHooksAction(h, true)
	if a.pendingToast != locales.HostNotConnected(h.label()) {
		t.Fatalf("toast %q, want the host not reached", a.pendingToast)
	}

	calls := make(chan string, 4)
	h.cl, _ = projectClient(t, func(method string, raw json.RawMessage) (any, error) {
		var p proto.HooksParams
		if err := json.Unmarshal(raw, &p); err != nil || p.Agent != "claude" {
			t.Errorf("%s: agent %+v, %v", method, p, err)
		}
		calls <- method
		return proto.HooksResult{State: "current"}, nil
	})
	a.hostHooksAction(h, true)
	if got := <-calls; got != "hooks.setup" {
		t.Errorf("an install called %s", got)
	}
	a.hostHooksAction(h, false)
	if got := <-calls; got != "hooks.remove" {
		t.Errorf("a remove called %s", got)
	}
}

// The Hosts page shows each host's agent hooks with their state, and the
// row's buttons reach the host's kerod — or say the host isn't reached.
func TestHostsHooksRow(t *testing.T) {
	a := testApp(t)
	h := withHost(t, a, "build")
	a.ss.page = "hosts"
	a.ss.hooks = map[string]*hostHooksLook{"build": {state: "missing"}}
	tt := ui.NewTester(a.settingsView, 820, 640)
	tt.Frame()
	if !tt.HasText(locales.SetHostHooks()) || !tt.HasText(locales.SetSkillMissing()) {
		t.Fatalf("no hooks row: %q", tt.Texts())
	}
	if err := tt.Click(locales.CommonInstall()); err != nil {
		t.Fatalf("no install: %v; texts %q", err, tt.Texts())
	}
	if a.pendingToast != locales.HostNotConnected(h.label()) {
		t.Fatalf("toast %q, want the host not reached", a.pendingToast)
	}
	// Current hooks: a remove alone.
	a.pendingToast = ""
	a.ss.hooks["build"].state = "current"
	tt.Frame()
	if err := tt.Click(locales.CommonRemove()); err != nil {
		t.Fatalf("no remove: %v; texts %q", err, tt.Texts())
	}
	if a.pendingToast != locales.HostNotConnected(h.label()) {
		t.Fatalf("toast %q after a remove", a.pendingToast)
	}
	// Outdated: an update beside the remove.
	a.pendingToast = ""
	a.ss.hooks["build"].state = "outdated"
	tt.Frame()
	if err := tt.Click(locales.CommonUpdate()); err != nil {
		t.Fatalf("no update: %v; texts %q", err, tt.Texts())
	}
	if a.pendingToast != locales.HostNotConnected(h.label()) {
		t.Fatalf("toast %q after an update", a.pendingToast)
	}
	shot(t, tt, "settings-host-hooks")
}

// A host's tag carries its menu in the sidebar, as its header does in
// trouble: the same hooks actions, held while the host isn't reached.
func TestHostMenuHooks(t *testing.T) {
	a := testApp(t)
	a.mergeState(a.local(), a.st)
	withHost(t, a, "build")
	tt := ui.NewTester(a.view, 1100, 700)
	tt.Frame()
	if err := tt.RightClick("build"); err != nil {
		t.Fatal(err)
	}
	menu := tt.Menu()
	for _, want := range []string{locales.HostMenuInstallHooks(), locales.HostMenuRemoveHooks()} {
		if !slices.Contains(menu, want) {
			t.Fatalf("the tag's menu has no %q: %q", want, menu)
		}
	}
	// The host isn't reached: the actions are held.
	for _, item := range []string{locales.HostMenuInstallHooks(), locales.HostMenuRemoveHooks()} {
		if err := tt.ChooseMenuItem(item); err == nil || !strings.Contains(err.Error(), "disabled") {
			t.Errorf("chose %q while the host isn't reached: %v", item, err)
		}
	}
}
