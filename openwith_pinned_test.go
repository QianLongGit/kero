package main

import (
	"slices"
	"testing"
)

// The apps the configuration pins to the file menu come as the
// installed apps name them: an uninstalled one is not there, and VS
// Code's item the menu has always had is not duplicated. A path pinned
// twice is one item, in the order the configuration holds.
func TestPinnedApps(t *testing.T) {
	app := func(name, path string) AppEntry { return AppEntry{Name: name, Path: path} }
	installed := []AppEntry{
		app("Alpha", "/Applications/Alpha.app"),
		app("Beta", "/Applications/Beta.app"),
		app("Visual Studio Code", "/Applications/Visual Studio Code.app"),
		app("Gamma", "/System/Applications/Gamma.app"),
	}
	for _, tc := range []struct {
		name   string
		pinned []string
		want   []AppEntry
	}{
		{"none", nil, nil},
		{"a pinned app by its path", []string{"/Applications/Beta.app"}, []AppEntry{app("Beta", "/Applications/Beta.app")}},
		{"in the order the configuration holds", []string{"/Applications/Beta.app", "/Applications/Alpha.app"},
			[]AppEntry{app("Beta", "/Applications/Beta.app"), app("Alpha", "/Applications/Alpha.app")}},
		{"an uninstalled app is not there", []string{"/Applications/Gone.app", "/Applications/Alpha.app"},
			[]AppEntry{app("Alpha", "/Applications/Alpha.app")}},
		{"VS Code's own item is not duplicated", []string{"/Applications/Visual Studio Code.app"}, nil},
		{"a path pinned twice is one item", []string{"/Applications/Alpha.app", "/Applications/Alpha.app"},
			[]AppEntry{app("Alpha", "/Applications/Alpha.app")}},
		{"an empty line pins nothing", []string{"", "/Applications/Alpha.app"}, []AppEntry{app("Alpha", "/Applications/Alpha.app")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pinnedApps(tc.pinned, installed); !slices.Equal(got, tc.want) {
				t.Errorf("pinnedApps(%q) = %v, want %v", tc.pinned, got, tc.want)
			}
		})
	}
	// No installed apps, as off macOS, is no pinned items whatever the
	// configuration holds.
	if got := pinnedApps([]string{"/Applications/Beta.app"}, nil); got != nil {
		t.Errorf("pinnedApps with no installed apps = %v, want none", got)
	}
}

// Toggling an app of the Settings window's group leaves the pinned
// paths in the order of its rows, whatever order the file had.
func TestTogglePinned(t *testing.T) {
	app := func(name, path string) AppEntry { return AppEntry{Name: name, Path: path} }
	apps := []AppEntry{app("Alpha", "/Applications/Alpha.app"), app("Beta", "/Applications/Beta.app")}
	pinned := []string{"/Applications/Beta.app"}
	for _, tc := range []struct {
		name string
		path string
		on   bool
		want []string
	}{
		{"pinning an app", "/Applications/Alpha.app", true, []string{"/Applications/Alpha.app", "/Applications/Beta.app"}},
		{"unpinning an app", "/Applications/Beta.app", false, nil},
		{"pinning what the file pins again", "/Applications/Beta.app", true, []string{"/Applications/Beta.app"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := togglePinned(apps, pinned, tc.path, tc.on); !slices.Equal(got, tc.want) {
				t.Errorf("togglePinned(%q, %q, %v) = %q, want %q", pinned, tc.path, tc.on, got, tc.want)
			}
		})
	}
}
