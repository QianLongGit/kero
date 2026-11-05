//go:build !darwin

package main

import "github.com/egoist/mygo/ui"

// LaunchServices is macOS's own: no other system names the apps that
// open a file, so the Open With menu keeps its two cross-platform items
// and no submenu of apps to choose.
func (a *App) chooseAppMenu(m *ui.Menu, h *host, path string) {}

// installedApps is macOS's own list, of its Applications folders: no
// other system holds one, so no app is pinned to the menu and none is
// offered to pin in Settings — nil, quietly.
func installedApps() []AppEntry { return nil }
