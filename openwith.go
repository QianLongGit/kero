package main

// The Open With menu of a file of the Files view: the system's own app
// for it, VS Code, and on macOS the apps installed on the system — the
// Finder's Open With, for the tree. The commands that run the apps come
// back as plain values, so what the menu does tests without running it.
// A remote host's file opens through the cache, as any open of it does.

import (
	"os/exec"
	"runtime"
	"strings"

	"github.com/egoist/mygo/ui"

	"kero/locales"
)

// openWithCommand runs an app on a file: by its name through open on
// macOS, which asks LaunchServices for it, as the command the app is
// elsewhere.
func openWithCommand(app, path string) []string {
	if runtime.GOOS == "darwin" {
		return []string{"open", "-a", app, path}
	}
	return []string{app, path}
}

// vscodeCommands opens a file in VS Code: the app by name on macOS and,
// where open finds none installed, the CLI in the PATH; elsewhere the
// CLI alone.
func vscodeCommands(path string) [][]string {
	if runtime.GOOS == "darwin" {
		return [][]string{openWithCommand("Visual Studio Code", path), {"code", path}}
	}
	return [][]string{{"code", path}}
}

// openWithMenu is the menu a right click on a file of the tree opens:
// the system's own app for it, VS Code, and what the platform's own
// chooseAppMenu adds.
func (a *App) openWithMenu(m *ui.Menu, h *host, path string) {
	if m.Item(locales.CommonOpen()).Chosen() {
		a.withLocalFile(h, path, openPath)
	}
	if m.Item(locales.FilesOpenVSCode()).Chosen() {
		a.withLocalFile(h, path, func(local string) { a.runOpenWith(vscodeCommands(local)...) })
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
