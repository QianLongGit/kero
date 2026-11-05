package main

// The Open With menu of a file of the Files view: the system's own app
// for it, the apps the configuration pins, and on macOS the apps
// installed on the system — the Finder's Open With, for the tree. The
// commands that run the apps come back as plain values, so what the
// menu does tests without running it. A remote host's file opens
// through the cache, as any open of it does.

import (
	"os/exec"
	"runtime"
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"

	"kero/locales"
)

// AppEntry is an app as the menu shows it: by its name, at its bundle's
// path.
type AppEntry struct{ Name, Path string }

// pinnedApps are the apps the configuration pins to the file menu, as
// the installed apps name them: matched by the bundle's path, which a
// rename between installs does not lose. An app uninstalled is not
// there; a path pinned twice is one item, in the order the
// configuration holds.
func pinnedApps(pinned []string, apps []AppEntry) []AppEntry {
	var out []AppEntry
	seen := map[string]bool{}
	for _, p := range pinned {
		if p == "" || seen[p] {
			continue
		}
		for _, app := range apps {
			if app.Path != p {
				continue
			}
			seen[p] = true
			out = append(out, app)
			break
		}
	}
	return out
}

// togglePinned returns the pinned paths as toggling the app at path
// leaves them, in the order of the apps' rows — whatever order the file
// had.
func togglePinned(apps []AppEntry, pinned []string, path string, on bool) []string {
	var out []string
	for _, app := range apps {
		if app.Path == path && on || app.Path != path && slices.Contains(pinned, app.Path) {
			out = append(out, app.Path)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// openWithCommand runs an app on a file: through open on macOS, which
// finds the app by its bundle's path, as the command the app is
// elsewhere.
func openWithCommand(app, path string) []string {
	if runtime.GOOS == "darwin" {
		return []string{"open", "-a", app, path}
	}
	return []string{app, path}
}

// openWithMenu is the menu a right click on a file of the tree opens:
// the system's own app for it, the apps the configuration pins to the
// menu, and what the platform's own chooseAppMenu adds.
func (a *App) openWithMenu(m *ui.Menu, h *host, path string) {
	if m.Item(locales.CommonOpen()).Chosen() {
		a.withLocalFile(h, path, openPath)
	}
	// No app pinned is no walk of the Applications folders: the menu of
	// the default settings opens with no app of its own.
	if len(a.set.FilesPinnedApps) > 0 {
		for _, app := range pinnedApps(a.set.FilesPinnedApps, installedApps()) {
			if m.Item(locales.FilesOpenIn(app.Name)).Chosen() {
				a.withLocalFile(h, path, func(local string) {
					a.runOpenWith(openWithCommand(app.Path, local))
				})
			}
		}
	}
	a.chooseAppMenu(m, h, path)
}

// runOpenWith starts the commands that open a file in an app, each in
// its turn until one runs, off the UI thread, and tells of the last
// that would not, in its own words — as open says why it found no app
// by the name, before the CLI takes its turn.
func (a *App) runOpenWith(cmds ...[]string) {
	go func() {
		var out []byte
		var err error
		for _, args := range cmds {
			if len(args) == 0 {
				continue
			}
			out, err = exec.Command(args[0], args[1:]...).CombinedOutput()
			if err == nil {
				return
			}
		}
		if err == nil {
			return
		}
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		a.update(func() { a.toast(msg) })
	}()
}
