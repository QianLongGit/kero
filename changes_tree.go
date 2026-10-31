package main

import (
	"strings"

	"github.com/egoist/mygo/ui"

	"kero/internal/proto"

	"kero/locales"
)

// The Changes view's tree is the files changed, in their folders, beside
// their diffs: a click on a file scrolls the diffs to it, one on a folder
// folds it, and the file whose diff is at the top shows as the one there,
// the tree scrolling along to it. It lists them in the diffs' order, the
// worktree's paths', so that it reads down as they do.

// treeNode is a folder of the tree, or a file: a folder with a folder
// alone in it is one, named "internal/auth", as GitHub shows them.
type treeNode struct {
	name string
	path string // the folder's, or the file's
	file int    // the file's index in the list, -1 for a folder
	kids []*treeNode
}

// treeItem is a row of the tree as it shows, its folders' folds as the
// user left them.
type treeItem struct {
	*treeNode
	depth int
	open  bool
}

// filesTree makes the tree of a list's files, in their order.
func filesTree(files []proto.GitFile) []*treeNode {
	root := &treeNode{file: -1}
	dirs := map[string]*treeNode{}
	for i, f := range files {
		parent := root
		parts := strings.Split(f.Path, "/")
		for j, name := range parts[:len(parts)-1] {
			p := strings.Join(parts[:j+1], "/")
			d := dirs[p]
			if d == nil {
				d = &treeNode{name: name, path: p, file: -1}
				dirs[p] = d
				parent.kids = append(parent.kids, d)
			}
			parent = d
		}
		parent.kids = append(parent.kids, &treeNode{name: parts[len(parts)-1], path: f.Path, file: i})
	}
	var squash func(n *treeNode)
	squash = func(n *treeNode) {
		for _, k := range n.kids {
			for k.file < 0 && len(k.kids) == 1 && k.kids[0].file < 0 {
				only := k.kids[0]
				k.name, k.path, k.kids = k.name+"/"+only.name, only.path, only.kids
			}
			squash(k)
		}
	}
	squash(root)
	return root.kids
}

// treeItems are the rows of a tree, but those inside a folder closed.
func treeItems(nodes []*treeNode, closed func(dir string) bool) []treeItem {
	var items []treeItem
	var walk func(nodes []*treeNode, depth int)
	walk = func(nodes []*treeNode, depth int) {
		for _, n := range nodes {
			open := n.file < 0 && !closed(n.path)
			items = append(items, treeItem{n, depth, open})
			if open {
				walk(n.kids, depth+1)
			}
		}
	}
	walk(nodes, 0)
	return items
}

// treeWidth is the tree's width in a panel this wide.
func treeWidth(panel float32) float32 {
	return min(max(panel*0.3, 140), 240)
}

// changesTree is the tree of a list's files, beside list, the list of
// their diffs, whose rows are rows, and heads the row of each file's
// header.
func (a *App) changesTree(c *ui.Context, pal *palette, scope string, res *proto.GitChanges, width float32, list *ui.ListState, rows []changeRow, heads []int) {
	ch := &a.side.changes
	if ch.treeOf != res {
		ch.tree, ch.treeOf = filesTree(res.Files), res
	}
	items := treeItems(ch.tree, func(dir string) bool { return ch.treeClosed[scope+"|"+dir] })

	// The file there: the one whose diff is at the top; at the end of the
	// list, where the file chosen last may not come to the top, that one
	// while it shows.
	cur := ""
	first, last := list.Visible()
	if first >= 0 && first < len(rows) {
		cur = rows[first].f.Path
	}
	if ch.picked != "" && list.AtEnd() {
		for i, f := range res.Files {
			if f.Path == ch.picked && heads[i] >= first && heads[i] <= last {
				cur = f.Path
			}
		}
	}

	if ch.treePlaces == nil {
		ch.treePlaces = map[string]*ui.ListState{}
	}
	tl := ch.treePlaces[scope]
	if tl == nil {
		tl = &ui.ListState{}
		ch.treePlaces[scope] = tl
	}
	tl.Key = func(i int) any { return treeKey{items[i].path, items[i].file < 0} }
	if cur != ch.current {
		ch.current = cur
		for i, it := range items {
			if it.file >= 0 && it.path == cur {
				tl.ScrollIntoView(i)
			}
		}
	}
	ui.List(c.Key("tree"), tl, len(items), func(i int) {
		it := items[i]
		if it.file < 0 {
			a.treeFolder(c, pal, it, func() {
				if ch.treeClosed == nil {
					ch.treeClosed = map[string]bool{}
				}
				ch.treeClosed[scope+"|"+it.path] = it.open
			})
			return
		}
		f := res.Files[it.file]
		a.treeFile(c, pal, it, f, f.Path == cur, func() {
			ch.picked = f.Path
			list.ScrollTo(heads[it.file], ui.Start)
		})
	}).Width(width).Shrink(0).MinHeight(0).Padding(0, 4, 16, 8)
}

// treeKey is what a row of the tree is, as its list keeps its state by:
// a file and a folder may have the same path, as one became the other.
type treeKey struct {
	path string
	dir  bool
}

// treeRow is a row of the tree, depth folders in.
func treeRow(c *ui.Context, pal *palette, depth int, current bool) ui.Element {
	row := ui.Row(c.Key("row")).Height(24).Padding(0, 8, 0, 6+12*float32(depth)).Gap(5).AlignItems(ui.Center).Radius(6)
	switch {
	case current:
		row.Background(pal.chip)
	case row.Hovered():
		row.Background(pal.rowHover)
	}
	return row
}

// treeFolder is a folder of the tree, which a click folds or unfolds.
func (a *App) treeFolder(c *ui.Context, pal *palette, it treeItem, toggle func()) {
	row := treeRow(c, pal, it.depth, false).Tooltip(it.path)
	if row.Clicked() {
		toggle()
	}
	row.Children(func() {
		chev := ui.Icon(c, chevronIcon).Size(9, 9).TextColor(pal.textFaint).Shrink(0)
		chev.Rotate(chev.AnimateTo("turn", 90*b2f(it.open), ui.Snappy))
		ui.Icon(c, folderIcon).Size(13, 13).TextColor(pal.textFaint).Shrink(0)
		ui.Text(c, it.name).FontSize(12.5).TextColor(pal.textMuted).SingleLine().Ellipsis("…").MinWidth(0)
	})
}

// treeFile is a file of the tree, its change as a letter in its color
// where a folder shows its icon; a click scrolls the diffs to it.
func (a *App) treeFile(c *ui.Context, pal *palette, it treeItem, f proto.GitFile, current bool, pick func()) {
	tip := f.Path
	if f.OldPath != "" {
		tip = f.OldPath + " → " + f.Path
	}
	row := treeRow(c, pal, it.depth, current).Tooltip(tip)
	if row.Clicked() {
		pick()
	}
	row.Children(func() {
		ui.Box(c).Size(9, 9).Shrink(0)
		letter, col := a.statusLetter(pal, f.Status)
		ui.Box(c).Width(13).Shrink(0).Center().Children(func() {
			ui.Text(c, letter).FontSize(10.5).FontWeight(650).TextColor(col).SingleLine()
		})
		fg := pal.text
		if f.Status == "deleted" {
			fg = pal.textMuted
		}
		ui.Text(c, it.name).FontSize(12.5).TextColor(fg).SingleLine().Ellipsis("…").MinWidth(0)
	})
}

// treeToggle is the button that shows the tree beside the diffs, or hides
// it, which shows it is shown as a pressed button does.
func (a *App) treeToggle(c *ui.Context, pal *palette) {
	on := a.prefs.SideTree
	label := locales.ChangesShowTree()
	if on {
		label = locales.ChangesHideTree()
	}
	b := ui.ButtonBase(c.Key("tree")).Size(28, 26).Radius(6).Center().TextColor(pal.textMuted).Label(label).Tooltip(label)
	switch {
	case b.Pressed():
		b.Background(pal.rowHover.Alpha(0.12))
	case on:
		b.Background(pal.chip).TextColor(pal.text)
	case b.Hovered():
		b.Background(pal.rowHover).TextColor(pal.text)
	}
	b.Children(func() { ui.Icon(c, treeIcon).Size(15, 15) })
	if b.Clicked() {
		a.prefs.SideTree = !on
		a.savePrefs()
	}
}
