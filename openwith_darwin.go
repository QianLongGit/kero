//go:build darwin

package main

// The apps installed on the system, read out of the folders that hold
// them. LaunchServices names only the apps that claim the file's type,
// and a type almost nothing claims — the dynamic UTI of a .go file —
// leaves the Open With list all but empty; a walk of the Applications
// folders names every app there is, which is what choosing one means.
// Suite folders hold their bundles one level down, and a bundle's own
// insides are its own, not apps of the menu.

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"

	"kero/locales"
)

// AppEntry is an app as the menu shows it: by its name, at its bundle's
// path.
type AppEntry struct{ Name, Path string }

// appsInDir lists the .app bundles under root, walking plain
// subdirectories depth levels down (Applications folders need two: a
// suite's bundles sit one level in) and never into a bundle itself. A
// folder that will not read is no apps of it, not a failure.
func appsInDir(root string, depth int) []AppEntry {
	des, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var apps []AppEntry
	for _, de := range des {
		// A symlink stands for its target — bundles and suite folders
		// are symlinked into Applications folders — and one that points
		// nowhere holds nothing.
		isDir := de.IsDir()
		if de.Type()&fs.ModeSymlink != 0 {
			fi, err := os.Stat(filepath.Join(root, de.Name()))
			isDir = err == nil && fi.IsDir()
		}
		if !isDir {
			continue
		}
		name := de.Name()
		if strings.HasSuffix(name, ".app") {
			apps = append(apps, AppEntry{
				Name: strings.TrimSuffix(name, ".app"),
				Path: filepath.Join(root, name),
			})
			continue
		}
		if depth > 1 {
			apps = append(apps, appsInDir(filepath.Join(root, name), depth-1)...)
		}
	}
	return apps
}

// appsInDirs merges the apps of each folder into one list: by name,
// the case folded away, and by path where names agree. The same bundle
// twice is one entry, the same name at two paths two, as the system
// holds it.
func appsInDirs(dirs ...string) []AppEntry {
	var apps []AppEntry
	for _, dir := range dirs {
		apps = append(apps, appsInDir(dir, 2)...)
	}
	slices.SortFunc(apps, func(a, b AppEntry) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
	return slices.CompactFunc(apps, func(a, b AppEntry) bool {
		return a.Name == b.Name && a.Path == b.Path
	})
}

// installedApps lists the apps the system has: the Applications
// folders, the user's own beside the system's. A machine that holds
// none is an empty list — the menu's no apps — not a failure.
func installedApps() []AppEntry {
	dirs := []string{"/Applications", "/System/Applications"}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "Applications"))
	}
	return appsInDirs(dirs...)
}

// chooseAppMenu adds the submenu of the apps installed on the system,
// any one of which runs the file — macOS's own Open With, of the tree.
func (a *App) chooseAppMenu(m *ui.Menu, h *host, path string) {
	m.Submenu(locales.FilesChooseApp(), func(m *ui.Menu) {
		apps := installedApps()
		if len(apps) == 0 {
			m.Item(locales.FilesNoApps()).Disabled(true)
			return
		}
		for _, app := range apps {
			if m.Item(app.Name).Chosen() {
				a.withLocalFile(h, path, func(local string) {
					a.runOpenWith(openWithCommand(app.Path, local))
				})
			}
		}
	})
}
