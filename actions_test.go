package main

import (
	"testing"

	"kero/internal/actions"

	"kero/locales"
)

// The View menu holds every action of it, the ones with shortcuts above
// all: an item missing from the menu has no shortcut anywhere, as the
// menus are what carry the keys.
func TestMenuFilesToggle(t *testing.T) {
	a := testApp(t)
	want, _ := actions.Find("files.toggle")
	for _, it := range a.viewItems() {
		if it.Label != locales.ActionToggleFiles() {
			continue
		}
		if k := it.Accelerator; k == "" || k != actions.Keys(want, a.set.Shortcuts) {
			t.Errorf("Toggle Files carries %q, want its action's %q", k, actions.Keys(want, a.set.Shortcuts))
		}
		return
	}
	var labels []string
	for _, it := range a.viewItems() {
		if it.Label != "" {
			labels = append(labels, it.Label)
		}
	}
	t.Errorf("the View menu has no Toggle Files; it holds %q", labels)
}
