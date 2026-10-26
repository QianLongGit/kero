//go:build !darwin

package main

// quickLook is macOS's spacebar preview; the other platforms have no
// such panel, and there Space in the Files view does nothing: a
// double-click still opens the file.
func quickLook(path string) error { return nil }
