package main

import (
	"bytes"
	"path"
	"strings"
	"time"

	"github.com/egoist/mygo/transfer"
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"

	"kero/internal/client"
	"kero/internal/git"
	"kero/internal/proto"

	"kero/locales"
)

// The Files tab is the tree of the worktree of the pane the user is in,
// else of its directory, as kerod lists it (fs.list), a folder at a time
// as the user unfolds it: what git ignores shows faded, and .git not at
// all. A click on a file opens it in the viewer, as text the user edits
// (edits.go), which follows the file as it changes. A drag of a row
// offers the file to the system (files_drag.go), a right click opens the
// row's menu (files_ops.go, files_menu.go), and a remote host's rows
// carry the transfers too (files_transfer.go).

// filesState is the Files tab's.
type filesState struct {
	// dirs are the folders listed, open the folders unfolded, by host and
	// path (filesKey); places the trees' places, by host and root.
	dirs   map[string]*dirList
	open   map[string]bool
	places map[string]*ui.ListState
	// gen counts the times the tab came to show, each of which lists its
	// folders anew; shown tells it shows this frame, and showed that it
	// did the frame before.
	gen           int
	shown, showed bool
	// edit is the name the user types in the tree (files_ops.go).
	edit *nameEdit
	// clip is the file the user copied in the tab, pasted into a folder
	// of the tree; nil while none.
	clip *fileClip
	// xfer is the one transfer under way — a download of a host's file
	// or folder, an upload into its — nil while none: the busy gate
	// every transfer's flow takes. Its row rides at the top of the tab
	// and in the sidebar's tab bar while it is (files_transfer.go).
	xfer *transferState
}

// dirList is a folder as kerod listed it.
type dirList struct {
	entries []proto.FSEntry
	err     string
	busy    bool
	stamp   string // the worktree's status it was listed at (gitStamp)
	gen     int
	fetched time.Time
}

// fileItem is a row of the tree: a file or a folder, depth folders in, or
// why a folder shows nothing, err, or the field the user types a name in,
// edit.
type fileItem struct {
	proto.FSEntry
	path      string // on the host
	rel       string // in the root, with slashes
	parent    string // the folder it is in, on the host
	parentRel string
	depth     int
	open      bool
	err       string
	edit      bool
}

// fileView is a file the viewer shows, as its text.
type fileView struct {
	pane  string // a pane of the host it is on
	path  string // on the host
	rel   string // in the Files' root, which its header shows
	st    *editor.State
	hash  string // of the file as its text shows it (git.Hash)
	edits edited
	err   string
	// gone, large and binary say why it shows no text: there is no file
	// there any more, or one too large, or one that isn't text.
	gone, large, binary bool
	busy                bool
	stamp               string
	fetched             time.Time
}

// maxView bounds the files the viewer shows as text.
const maxView = 4 << 20

// filesKey keys a path of a host.
func filesKey(h *host, p string) string { return h.name + "\x00" + p }

// filesRoot is the folder the Files tab shows, and the pane it is of: the
// worktree of the one the user is in, else its directory.
func (a *App) filesRoot(s *proto.Session) (*proto.Pane, string) {
	p := withGit(s, s.PaneByID(a.focusOf(s)))
	switch {
	case p == nil:
		return nil, ""
	case p.Git != nil:
		return p, p.Git.Root
	case p.ProcessDir != "":
		return p, p.ProcessDir
	}
	return p, p.Dir
}

// filesSide is the Files tab: the root's tree, a folder listed as it
// unfolds, and again as the worktree changes.
func (a *App) filesSide(c *ui.Context, pal *palette, s *proto.Session) {
	fs := &a.side.files
	if !fs.showed {
		fs.gen++
	}
	fs.shown, fs.showed = true, true
	p, root := a.filesRoot(s)
	if root == "" {
		a.sideMessage(c, pal, locales.FilesNoFolder(), "")
		return
	}
	h := a.hostOf(p.ID)
	if fs.dirs == nil || len(fs.dirs) > 2000 {
		fs.dirs = map[string]*dirList{}
	}
	if fs.open == nil {
		fs.open = map[string]bool{}
	}
	stamp := gitStamp(p.Git)
	working := sessionWorking(s)
	if working {
		c.After(5 * time.Second)
	}
	asking := 0
	for _, l := range fs.dirs {
		if l.busy {
			asking++
		}
	}
	// The folders shown, listed again as the worktree's status changes,
	// as the tab shows anew, and every few seconds while an agent works,
	// a few at a time.
	list := func(dir string) *dirList {
		l := fs.dirs[filesKey(h, dir)]
		stale := l == nil || !l.busy && (l.stamp != stamp || l.gen != fs.gen || working && time.Since(l.fetched) > 5*time.Second)
		if stale && asking < 4 && h.cl != nil {
			asking++
			l = a.fetchDir(h, dir, p.Git != nil, stamp)
		}
		return l
	}
	var items []fileItem
	var walk func(dir, rel string, depth int)
	walk = func(dir, rel string, depth int) {
		l := list(dir)
		if l == nil || l.entries == nil && l.err == "" {
			return
		}
		if l.err != "" && l.entries == nil {
			items = append(items, fileItem{path: dir, rel: rel + "/!", depth: depth, err: l.err})
			return
		}
		// A new entry's name, at the top of its folder.
		if e := fs.edit; e != nil && e.op != editRename && e.host == h.name && e.dir == dir {
			items = append(items, fileItem{FSEntry: proto.FSEntry{Dir: e.op == editNewFolder}, rel: rel + "/+", parent: dir, parentRel: rel, depth: depth, edit: true})
		}
		for _, e := range l.entries {
			if e.Dir && e.Name == ".git" {
				continue
			}
			it := fileItem{FSEntry: e, path: h.joinPath(dir, e.Name), rel: path.Join(rel, e.Name), parent: dir, parentRel: rel, depth: depth}
			it.open = e.Dir && fs.open[filesKey(h, it.path)]
			it.edit = fs.edit != nil && fs.edit.op == editRename && fs.edit.host == h.name && fs.edit.path == it.path
			items = append(items, it)
			if it.open {
				walk(it.path, it.rel, depth+1)
			}
		}
	}
	walk(root, "", 0)

	ui.Row(c.Key("root")).Height(28).Shrink(0).Padding(0, 10, 6, 16).Gap(0).AlignItems(ui.Center).Children(func() {
		// The label is the folder it names: its menu the upload into it,
		// as a folder's row of the tree has — a remote host's, for this
		// machine's files need no upload.
		lbl := ui.Text(c, h.shortDir(root)).FontSize(12.5).TextColor(pal.textFaint).SingleLine().Ellipsis("…").MinWidth(0).Grow(1).Tooltip(root)
		if h.remote() {
			lbl.ContextMenu(func(m *ui.Menu) {
				if m.Item(locales.FilesUploadHere()).Chosen() {
					a.uploadTo(h, root)
				}
			})
		}
		iconButton(c, "new-file", newFileIcon, locales.FilesNewFile(), pal, func() { a.startCreate(s, p.ID, root, "", false) })
		iconButton(c, "new-folder", newFolderIcon, locales.FilesNewFolder(), pal, func() { a.startCreate(s, p.ID, root, "", true) })
		iconButton(c, "fold-folders", collapseIcon, locales.FilesCollapseFolders(), pal, func() {
			prefix := filesKey(h, root)
			for k := range fs.open {
				if strings.HasPrefix(k, prefix) {
					delete(fs.open, k)
				}
			}
		})
	})
	// The transfer under way, below the root's row.
	a.xferRow(c, pal)
	if l := fs.dirs[filesKey(h, root)]; l == nil || l.entries == nil {
		switch {
		case l != nil && l.err != "" && !l.busy:
			a.sideMessage(c, pal, locales.FilesCantReadFolder(), l.err)
		case h.cl != nil:
			ui.Column(c).Grow(1).Center().Children(func() { ui.Spinner(c) })
		}
		return
	}

	if fs.places == nil {
		fs.places = map[string]*ui.ListState{}
	}
	ls := fs.places[filesKey(h, root)]
	if ls == nil {
		ls = &ui.ListState{}
		fs.places[filesKey(h, root)] = ls
	}
	ls.Key = func(i int) any { return treeKey{items[i].rel, items[i].Dir} }
	var shown string
	if v := a.viewOf(s); v != nil && v.kind == sideFiles && v.file != nil {
		shown = v.file.path
	}
	sid, pid := s.ID, p.ID
	ui.List(c.Key("files"), ls, len(items), func(i int) {
		it := items[i]
		switch {
		case it.edit:
			a.nameRow(c, pal, it)
		case it.err != "":
			ui.Text(c, it.err).FontSize(12).TextColor(pal.textFaint).SingleLine().Ellipsis("…").Padding(4, 8, 4, 6+12*float32(it.depth)+28).Tooltip(it.err)
		case it.Dir:
			key := filesKey(h, it.path)
			row := a.treeFolder(c, pal, it.Name, it.rel, it.depth, it.open, it.Ignored, func() {
				if it.open {
					delete(fs.open, key)
				} else {
					fs.open[key] = true
				}
			})
			row.ContextMenu(func(m *ui.Menu) { a.entryMenu(m, s, pid, it) })
			// A drag of the folder offers it to the system, as a file's
			// row does (files_drag.go).
			row.DragDataFrom(func() transfer.Data { return a.fileDragData(h, it.path, true) })
		default:
			row := a.treeEntry(c, pal, h, pid, it, it.path == shown, func() {
				s := a.session(sid)
				if s == nil {
					return
				}
				v := a.side.views[sid]
				if v != nil && v.file != nil && v.file.path == it.path {
					a.openView(s, sideFiles, nil) // the one it shows
					return
				}
				// In the place of the one it shows, saved first.
				var ts []editTarget
				if v != nil && v.file != nil {
					if t := a.fileTarget(v.file); t.dirty() {
						ts = append(ts, t)
					}
				}
				a.askToSave(ts, func() {
					if s := a.session(sid); s != nil {
						a.openView(s, sideFiles, &fileView{pane: pid, path: it.path, rel: it.rel})
					}
				})
			})
			row.ContextMenu(func(m *ui.Menu) { a.entryMenu(m, s, pid, it) })
		}
	}).Grow(1).MinHeight(0).Padding(0, 10, 16, 10)
	// Space previews the file the viewer shows, as macOS does — a remote
	// host's through the cache, as any preview of it is.
	if shown != "" && ls.Shortcut(c, 0, ui.KeySpace) {
		path := shown
		a.defer_(func() {
			a.withLocalFile(h, path, func(local string) {
				if err := quickLook(local); err != nil {
					a.toast(err.Error())
				}
			})
		})
	}
}

// treeEntry is a file of the Files tab's tree, which a click opens in the
// viewer; faded, it is one git ignores. A double click opens it as
// Settings say, in a file pane beside its own pane or in the system's app
// for it (openBySetting); a drag of it offers the file to the system: to
// another app, to a folder of the Finder's — a remote host's as the
// cache's copy of it, as any drag of it is.
func (a *App) treeEntry(c *ui.Context, pal *palette, h *host, pane string, it fileItem, current bool, open func()) ui.Element {
	row := treeRow(c, pal, it.depth, current).Tooltip(it.rel)
	if it.Ignored {
		row.Opacity(0.55)
	}
	if row.Clicked() {
		open()
	}
	if row.DoubleClicked() {
		a.defer_(func() { a.openBySetting(pane, it.path) })
	}
	row.DragDataFrom(func() transfer.Data { return a.fileDragData(h, it.path, false) })
	row.Children(func() {
		ui.Box(c).Size(9, 9).Shrink(0)
		fileIconImage(c, pal, it.Name, false, false)
		ui.Text(c, it.Name).FontSize(12.5).TextColor(pal.text).SingleLine().Ellipsis("…").MinWidth(0)
	})
	return row
}

// openBySetting opens a file of the tree the way a double click does, as
// Settings say: in a file pane beside the pane the tree is of, or in the
// system's app for the file. A remote host's file opens through the
// cache, as any open of it does.
func (a *App) openBySetting(pane, path string) {
	switch a.set.FileOpen {
	case "system":
		a.withLocalFile(a.hostOf(pane), path, openPath)
	default:
		a.openInPane(pane, path)
	}
}

// openInPane opens a file of the tree in a file pane beside a pane of its
// host: the app's editor shows it, colored by its language, on the host
// it is on. A remote host's file is the cache's copy here, its title the
// file's own name.
func (a *App) openInPane(pane, path string) {
	s := a.sessionOfPane(pane)
	if s == nil {
		return
	}
	a.withLocalFile(a.hostOf(pane), path, func(local string) {
		a.split(s, proto.CreateParams{Split: pane, Direction: "right", File: local})
	})
}

// fetchDir asks kerod for a folder's entries, dotfiles too, marking what
// git ignores in a worktree.
func (a *App) fetchDir(h *host, dir string, ignored bool, stamp string) *dirList {
	fs := &a.side.files
	key := filesKey(h, dir)
	l := fs.dirs[key]
	if l == nil {
		l = &dirList{}
		fs.dirs[key] = l
	}
	l.busy = true
	gen, cl := fs.gen, h.cl
	go func() {
		var res proto.FSList
		err := cl.Call("fs.list", proto.FSListParams{Dir: dir, Hidden: true, Ignored: ignored}, &res)
		a.update(func() {
			l.busy, l.fetched, l.stamp, l.gen = false, time.Now(), stamp, gen
			if err != nil {
				l.err = remoteError(err)
				return
			}
			l.entries, l.err = res.Entries, ""
		})
	}()
	return l
}

// remoteError says what a call to a host's kerod came to: a method its
// kerod has not is told to update.
func remoteError(err error) string {
	if strings.Contains(err.Error(), "unknown method") {
		return locales.FilesUpdateHost()
	}
	return err.Error()
}

// relistDir marks a folder of a host — and the folders open in it — for
// the listing anew, as a change the tree did not watch lands in it: a
// paste, an upload. The folder lists again as the tab shows it.
func (a *App) relistDir(h *host, dir string) {
	prefix := filesKey(h, "")
	for k, l := range a.side.files.dirs {
		if p := strings.TrimPrefix(k, prefix); strings.HasPrefix(k, prefix) && (p == dir || withinDir(p, dir)) {
			l.gen-- // stale: no longer the tab's own listing
		}
	}
}

// fileViewer is the viewer of a file of Files: its text, which it reads
// again as the worktree changes, and every few seconds while an agent
// works, while the user has no edits of it to save.
func (a *App) fileViewer(c *ui.Context, pal *palette, s *proto.Session, fv *fileView) {
	h := a.hostOf(fv.pane)
	stamp := ""
	if p := withGit(s, s.PaneByID(fv.pane)); p != nil {
		stamp = gitStamp(p.Git)
	}
	working := sessionWorking(s)
	if working {
		c.After(3 * time.Second)
	}
	editing := fv.st != nil && (fv.st.Buffer.Modified() || fv.edits.saving)
	if !fv.busy && !editing && h.cl != nil && (fv.fetched.IsZero() || fv.stamp != stamp || working && time.Since(fv.fetched) > 3*time.Second) {
		a.loadFile(h.cl, fv, stamp)
	}

	dir, name := path.Split(fv.rel)
	a.viewerHeader(c, pal, s, fileIcon, name, editing, func() {
		if dir = strings.TrimSuffix(dir, "/"); dir != "" {
			ui.Text(c, dir).FontSize(13).TextColor(pal.textFaint).SingleLine().Ellipsis("…").Shrink(2).MinWidth(0).Tooltip(fv.path)
		}
	}, func() {
		// The pane the file opens in beside its own, as a double click of
		// the tree's does; the open of the system beside it, a local
		// file's alone.
		iconButton(c, "files-edit", fileIcon, locales.FilesOpenInPane(), pal, func() { a.defer_(func() { a.openInPane(fv.pane, fv.path) }) })
		if !h.remote() && !fv.gone {
			full := fv.path
			iconButton(c, "open", openFileIcon, locales.ChangesOpenFile(), pal, func() { openPath(full) })
		}
	})
	switch {
	case fv.gone:
		a.sideMessage(c, pal, locales.FilesGone(), fv.rel)
	case fv.large:
		a.sideMessage(c, pal, locales.ChangesTooLarge(), "")
	case fv.binary:
		a.sideMessage(c, pal, locales.ChangesBinary(), "")
	case fv.st == nil && fv.err != "":
		a.sideMessage(c, pal, locales.FilesCantReadFile(), fv.err)
	case fv.st == nil:
		ui.Column(c).Grow(1).Center().Children(func() { ui.Spinner(c) })
	default:
		fv.st.Font = ui.Font{Family: a.termFamily(), Size: max(a.set.FontSize-1, 10)}
		fv.st.Theme = a.codeTheme(c, pal)
		fv.st.NoScrollPastEnd = true
		editor.View(c.Key("text-"+fv.path), fv.st).Grow(1).FillWidth().MinHeight(0)
	}
}

// loadFile reads a file of the viewer: its text into its editor, anew for
// its first, else as an edit, so that it stays scrolled where it was.
func (a *App) loadFile(cl *client.Client, fv *fileView, stamp string) {
	fv.busy = true
	go func() {
		var st proto.FSStat
		var data []byte
		err := cl.Call("fs.stat", proto.FSStatParams{Path: fv.path}, &st)
		if err == nil && st.Exists && !st.Dir && st.Size <= maxView {
			var r proto.FSRead
			err = cl.Call("fs.read", proto.FSReadParams{Path: fv.path, Max: maxView}, &r)
			data = r.Data
		}
		a.update(func() {
			fv.busy, fv.fetched, fv.stamp = false, time.Now(), stamp
			if err != nil {
				fv.err = remoteError(err)
				return
			}
			fv.err = ""
			fv.gone = !st.Exists || st.Dir
			fv.large = !fv.gone && st.Size > maxView
			fv.binary = !fv.gone && !fv.large && isBinary(data)
			if fv.gone || fv.large || fv.binary {
				return
			}
			// Not over what the user typed as it came.
			if fv.st != nil && (fv.st.Buffer.Modified() || fv.edits.saving) {
				return
			}
			a.showFile(fv, data)
		})
	}()
}

// showFile puts a file as read in its viewer, and with its first text the
// user's edit of it backed up as the app last quit, if any.
func (a *App) showFile(fv *fileView, data []byte) {
	first := fv.st == nil
	fv.hash = git.Hash(data)
	showFileText(fv, string(data))
	if first {
		a.restoreEdit(sideFiles, a.hostOf(fv.pane), fv.path, fv.st.Buffer, &fv.hash)
	}
}

// showFileText puts a file's text in its viewer's editor, as saved.
func showFileText(fv *fileView, text string) {
	if fv.st == nil {
		buf := editor.NewBuffer(text)
		buf.SetLanguage(editor.LanguageFor(path.Base(fv.rel)))
		fv.st = &editor.State{Buffer: buf}
		return
	}
	replaceText(fv.st.Buffer, text)
	fv.st.Buffer.MarkSaved()
}

// isBinary tells a file that isn't text, as git does: a NUL in its first
// 8000 bytes.
func isBinary(data []byte) bool {
	return bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0
}
