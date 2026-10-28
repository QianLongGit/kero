package main

// What a drag of a file of the Files view offers the system: a drag out
// of the tree, to another app, to a folder of the Finder's. A local
// host's file goes as itself; a remote host's as the cache's copy of it,
// which a preview or an open brings down — and a drag asks for too, so
// the next one of it drags the file. Until then, and a remote folder,
// the drag offers the path as text, which an app that takes text pastes.

import (
	"os"

	"github.com/egoist/mygo/transfer"
)

// fileDragData is what a drag of a file or folder of the tree offers.
func (a *App) fileDragData(h *host, path string, dir bool) transfer.Data {
	if !h.remote() {
		if d, err := transfer.FileData(path); err == nil {
			return d
		}
		// No path of this machine is absolute? Offer what it is.
		return transfer.TextData(path)
	}
	if !dir {
		if local := a.cachedCopy(h, path); local != "" {
			if d, err := transfer.FileData(local); err == nil {
				return d
			}
		}
		// The file is not down here: bring it, so the next drag of it is
		// the file itself. This one offers its path.
		if cl := h.cl; cl != nil {
			go a.materialize(cl, h.name, path)
		}
	}
	return transfer.TextData(path)
}

// cachedCopy is the cache's local copy of a remote host's file, "" for
// none: a read on the UI thread a drag asks of it, a map and a stat.
func (a *App) cachedCopy(h *host, path string) string {
	local := cachePath(h.name, path)
	a.cache.mu.Lock()
	cached := a.cache.entries[local] != nil
	a.cache.mu.Unlock()
	if !cached {
		return ""
	}
	if _, err := os.Stat(local); err != nil {
		return ""
	}
	return local
}
