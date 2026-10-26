package main

// The Open With menu of a file of the Files view: the system's own app
// for it, VS Code, and on macOS the apps the system says open it — the
// Finder's Open With, for the tree. The commands that run the apps come
// back as plain values, so what the menu does tests without running it.

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

// vscodeCommand opens a file in VS Code: the app by name on macOS, the
// CLI in the PATH elsewhere.
func vscodeCommand(path string) []string {
	if runtime.GOOS == "darwin" {
		return openWithCommand("Visual Studio Code", path)
	}
	return []string{"code", path}
}

// openWithMenu is the menu a right click on a file of the tree opens:
// the system's own app for it, VS Code, and what the platform's own
// chooseAppMenu adds.
func (a *App) openWithMenu(m *ui.Menu, path string) {
	if m.Item(locales.CommonOpen()).Chosen() {
		openPath(path)
	}
	if m.Item(locales.FilesOpenVSCode()).Chosen() {
		a.runOpenWith(vscodeCommand(path))
	}
	a.chooseAppMenu(m, path)
}

// runOpenWith starts a command that opens a file in an app, off the UI
// thread, and tells of one that would not run in the app's own words,
// as open says why it found no app by the name.
func (a *App) runOpenWith(args []string) {
	go func() {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
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
