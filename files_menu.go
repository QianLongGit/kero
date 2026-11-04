package main

// The extras a right click on a row of the Files tab carries past what
// files_ops.go gives it (the new, the rename and the delete): a file's
// Open With, the transfers a remote host's rows carry (files_transfer.go)
// — a Download… of the row's own, an Upload Here… into a folder — the
// tree's own copy and paste, and the row's own path to the clipboard,
// whole and in the root the tree stands in. A copy is of the tab alone —
// it notes the file and the host it is on, and a paste reads it there
// and writes it into the folder the paste is of, any host's folder as
// any other's, so a file crosses machines on a paste.

import (
	"os"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"

	"kero/internal/proto"

	"kero/locales"
)

// fileClip is the file the user copied in the Files tab: the host it is
// on — "" this machine — and its path there.
type fileClip struct {
	host string
	path string
}

// entryExtras is what a row's menu (files_ops.go's entryMenu) carries
// past the new, the rename and the delete: a file's Open With, then the
// items fileMenuEntries orders — the download of a remote host's file or
// folder and the upload into its folder, the copy of a file, the paste
// of what was copied into the folder the click is of — and the row's own
// path to the clipboard, whole and in the tree's root.
func (a *App) entryExtras(m *ui.Menu, pane string, it fileItem) {
	h := a.hostOf(pane)
	if !it.Dir {
		a.openWithMenu(m, h, it.path)
		m.Separator()
	}
	into := it.parent
	if it.Dir {
		into = it.path
	}
	// The items in fileMenuEntries' order, each label its own item; the
	// Open With labels a file's list begins with pass unnamed here, for
	// the group above built them already.
	for _, label := range fileMenuEntries(h, it.path, it.Dir) {
		switch label {
		case locales.FilesDownload():
			if m.Item(label).Chosen() {
				a.downloadFrom(h, it.path, it.Dir)
			}
		case locales.FilesUploadHere():
			if m.Item(label).Chosen() {
				a.uploadTo(h, it.path)
			}
		case locales.FilesCopy():
			if m.Item(label).Chosen() {
				a.side.files.clip = &fileClip{host: h.name, path: it.path}
			}
		case locales.FilesPaste():
			if m.Item(label).Disabled(a.side.files.clip == nil).Chosen() {
				a.pasteFileInto(pane, into)
			}
		}
	}
	// The row's path to the clipboard as any copy of the app's is
	// (palette.go), on the host it is on and in the root the tree stands
	// in, a file's row and a folder's alike.
	if m.Item(locales.FilesCopyPath()).Chosen() {
		a.copyText = it.path
	}
	if m.Item(locales.FilesCopyRelativePath()).Chosen() {
		a.copyText = it.rel
	}
}

// fileMenuEntries is the labels of a row's transfer, copy and paste
// items, past its Open With, in their order: the transfer items a
// remote host's rows carry — a file's Download…, a folder's Download…
// and Upload Here… — the tree's own copy and paste. This machine has
// its files already: no transfer items.
func fileMenuEntries(h *host, path string, isDir bool) []string {
	var out []string
	if !isDir {
		if h.remote() {
			out = append(out, locales.FilesDownload())
		}
		out = append(out, locales.FilesCopy())
	} else if h.remote() {
		out = append(out, locales.FilesDownload(), locales.FilesUploadHere())
	}
	return append(out, locales.FilesPaste())
}

// parentPath is the folder a path of the tree is in: the path less its
// last name, the host's own root kept whole — "" for a root itself.
func parentPath(path string) string {
	i := strings.LastIndexAny(path, `/\`)
	if i < 0 {
		return ""
	}
	parent := strings.TrimRight(path[:i], `/\`)
	if parent == "" {
		return "/"
	}
	// A Windows drive keeps its root slash: "C:\".
	if len(parent) == 2 && parent[1] == ':' {
		parent += `\`
	}
	return parent
}

// pasteName is the name a paste of a copied file takes beside its own:
// "x copy.txt", then "x copy 2.txt", … as the Finder's own paste does.
func pasteName(base string, n int) string {
	stem, ext := base, ""
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		stem, ext = base[:i], base[i:]
	}
	switch n {
	case 0:
		return stem + ext
	case 1:
		return stem + " copy" + ext
	}
	return stem + " copy " + strconv.Itoa(n) + ext
}

// pasteFileInto reads the copied file where it was copied and writes it
// into a folder of the tree, under its own name or the next free copy of
// it. The hosts' connections come down here, on the UI thread that owns
// them; the reading and the writing run off it.
func (a *App) pasteFileInto(pane, dir string) {
	clip := a.side.files.clip
	if clip == nil {
		return
	}
	dst := a.hostOf(pane)
	if dst.cl == nil {
		a.toast(locales.HostNotConnected(dst.label()))
		return
	}
	dstCl := dst.cl
	base := proto.DirName(clip.path)
	go func() {
		data, err := a.readFileOf(clip.host, clip.path)
		if err == nil {
			// Its own name, or a copy beside it: never over a file there.
			var target string
			for n := 0; n < 100; n++ {
				target = dst.joinPath(dir, pasteName(base, n))
				var st proto.FSStat
				if err = dstCl.Call("fs.stat", proto.FSStatParams{Path: target}, &st); err != nil || !st.Exists {
					break
				}
			}
			if err == nil {
				var sum string
				err = dstCl.Call("fs.write", proto.FSWriteParams{Path: target, Text: string(data), Force: true}, &sum)
			}
		}
		a.update(func() {
			if err != nil {
				a.toast(locales.FilesCantPaste() + ": " + remoteError(err))
				return
			}
			a.toast(locales.FilesPasted(base))
			a.relistDir(dst, dir)
		})
	}()
}

// readFileOf reads a file of a host whole: this machine's from its disk,
// another's from its kerod. It runs off the UI thread, as a paste of it.
func (a *App) readFileOf(hostName, path string) ([]byte, error) {
	if hostName == "" {
		return os.ReadFile(path)
	}
	h := a.hostNamed(hostName)
	if h == nil || h.cl == nil {
		return nil, errCacheOffline
	}
	var r proto.FSRead
	if err := h.cl.Call("fs.read", proto.FSReadParams{Path: path, Max: max(maxImage, maxText)}, &r); err != nil {
		return nil, err
	}
	return r.Data, nil
}
