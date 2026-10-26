//go:build !darwin

package main

import "github.com/egoist/mygo/ui"

// LaunchServices is macOS's own: no other system names the apps that
// open a file, so the Open With menu keeps its two cross-platform items
// and no submenu of apps to choose.
func (a *App) chooseAppMenu(m *ui.Menu, h *host, path string) {}
