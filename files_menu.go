package main

// The menu a right click on a file or folder of the Files view opens,
// past the Open With a file has: the tree's own copy and paste, and the
// delete of either. A copy is of the Files view alone — it notes the
// file and the host it is on, and a paste reads it there and writes it
// into the folder the paste is of, any host's folder as any other's, so
// a file crosses machines on a paste. A remote host's rows carry the
// transfers too (files_transfer.go): a Download… of the row's own, an
// Upload Here… into a folder. A delete asks first, as it cannot be
// undone, and the folder that held what it deleted lists again.

import (
	"os"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"

	"kero/internal/proto"

	"kero/locales"
)

// fileClip is the file the user copied in the Files view: the host it is
// on — "" this machine — and its path there.
type fileClip struct {
	host string
	path string
}

// fileMenu is the menu a right click on a file or folder of the tree
// opens: a file's Open With, then the items fileMenuEntries orders —
// the download of a remote host's file or folder and the upload into
// its folder, the copy of a file, the paste of what was copied into the
// folder the click is of — and the delete of either, which asks first,
// as it cannot be undone.
func (a *App) fileMenu(m *ui.Menu, h *host, t *fileTree, path string, dir bool) {
	if !dir {
		a.openWithMenu(m, h, path)
		m.Separator()
	}
	into := path
	if !dir {
		into = parentPath(path)
	}
	// The items in fileMenuEntries' order, each label its own item; the
	// Open With labels a file's list begins with pass unnamed here, for
	// the group above built them already.
	for _, label := range fileMenuEntries(h, path, dir) {
		switch label {
		case locales.FilesDownload():
			if m.Item(label).Chosen() {
				a.downloadFrom(h, path, dir)
			}
		case locales.FilesUploadHere():
			if m.Item(label).Chosen() {
				a.uploadTo(h, path)
			}
		case locales.FilesCopy():
			if m.Item(label).Chosen() {
				a.side.files.clip = &fileClip{host: h.name, path: path}
			}
		case locales.FilesPaste():
			if m.Item(label).Disabled(a.side.files.clip == nil).Chosen() {
				a.pasteFileInto(t, into)
			}
		}
	}
	m.Separator()
	if m.Item(locales.FilesDelete()).Chosen() {
		a.deleteFile(t, path, dir)
	}
}

// fileMenuEntries is the labels of the menu a right click on a row of
// the tree opens, in their order: a file's Open With, the transfer
// items a remote host's rows carry — a file's Download…, a folder's
// Download… and Upload Here… — the tree's own copy and paste, and the
// delete of either. This machine has its files already: no transfer
// items, and a local file's menu holds its Open With in fileMenu's
// hand, which this list walks.
func fileMenuEntries(h *host, path string, isDir bool) []string {
	var out []string
	if !isDir {
		out = append(out, locales.CommonOpen(), locales.FilesOpenVSCode())
		if h.remote() {
			out = append(out, locales.FilesDownload())
		}
		out = append(out, locales.FilesCopy())
	} else if h.remote() {
		out = append(out, locales.FilesDownload(), locales.FilesUploadHere())
	}
	return append(out, locales.FilesPaste(), locales.FilesDelete())
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
func (a *App) pasteFileInto(t *fileTree, dir string) {
	clip := a.side.files.clip
	if clip == nil {
		return
	}
	dst := a.hostOf(t.pane)
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
				target = hostJoin(dir, pasteName(base, n))
				var st proto.FSStat
				if err = dstCl.Call("fs.stat", proto.FSStatParams{Path: target}, &st); err != nil || !st.Exists {
					break
				}
			}
			if err == nil {
				var sum string
				err = dstCl.Call("fs.write", proto.FSWriteParams{Path: target, Content: data}, &sum)
			}
		}
		a.update(func() {
			if err != nil {
				a.toast(locales.FilesCantPaste() + ": " + remoteError(err))
				return
			}
			a.toast(locales.FilesPasted(base))
			a.fetchDir(t, dir)
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

// deleteFile asks, then deletes a file or folder of the tree from its
// host — a folder with all that is in it. What it held lists again.
func (a *App) deleteFile(t *fileTree, path string, dir bool) {
	cl := a.hostOf(t.pane).cl
	if cl == nil {
		a.toast(locales.HostNotConnected(a.hostOf(t.pane).label()))
		return
	}
	name := proto.DirName(path)
	title, detail := locales.FilesDeleteFileTitle(name), locales.FilesDeleteFileDetail()
	if dir {
		title, detail = locales.FilesDeleteFolderTitle(name), locales.FilesDeleteFolderDetail()
	}
	a.confirm = &confirmation{
		open:    true,
		title:   title,
		detail:  detail,
		action:  locales.FilesDelete(),
		destroy: true,
		run: func() {
			go func() {
				err := cl.Call("fs.delete", proto.FSDeleteParams{Path: path}, nil)
				a.update(func() {
					if err != nil {
						a.toast(locales.FilesCantDelete() + ": " + remoteError(err))
						return
					}
					a.fileDeleted(t, path)
				})
			}()
		},
	}
}

// fileDeleted is what the tree does once its host deleted a path of it:
// the folder that held it lists again, an open folder of it closes, and
// the file under the tree, if it was it, hides.
func (a *App) fileDeleted(t *fileTree, path string) {
	a.toast(locales.FilesDeleted(proto.DirName(path)))
	t.outline.Open.Remove(path)
	delete(t.dirs, path)
	if parent := parentPath(path); parent != "" {
		a.fetchDir(t, parent)
	}
	if t.shown == t.shownPath(path) {
		t.shown, t.file = "", nil
	}
}
