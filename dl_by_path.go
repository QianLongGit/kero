package main

import (
	"strings"

	"github.com/egoist/mygo/ui"

	"kero/internal/proto"
	"kero/locales"
)

// The Files tab's download by path: a path the user knows, typed rather
// than walked to in the tree. fs.stat — the daemon itself resolving the
// path, relative against the folder the tree shows, absolute, ~/ from
// the home — says what it names before anything downloads, so a wrong
// one is answered in the dialog, not by a failed transfer. The view and
// its two entrances live here too (Task 2); this file's first half is
// the flow they share.

// dlByPath is the Files tab's download-by-path dialog (dl_by_path.go):
// one path typed, checked against the host before it downloads.
type dlByPath struct {
	open  bool
	host  *host  // the remote it downloads from; nil never while open
	base  string // the root the tree showed: a relative path's base
	query string
	err   string // why the last path did not download, shown in red
	busy  bool   // a stat is under way; the Download button waits
	gen   int    // counts openings; an answer from a dialog since closed is dropped
}

// dlByPathSubmit takes the dialog's query to the host: fs.stat says what
// the path names — the daemon resolves a relative one against Base, and
// ~ and ~/… as the home's — and what exists downloads, files and folders
// both, through downloadFrom as its own menu item does. What does not
// stays in the dialog, told why in red.
func (a *App) dlByPathSubmit() {
	d := &a.side.files.dl
	q := strings.TrimSpace(d.query)
	if q == "" || d.busy {
		return
	}
	h := d.host
	if h.cl == nil {
		d.err = locales.HostNotConnected(h.name)
		return
	}
	g, base := d.gen, d.base
	d.busy, d.err = true, ""
	p := proto.FSStatParams{Path: q, Base: base}
	go func() {
		var st proto.FSStat
		err := h.cl.Call("fs.stat", p, &st)
		a.update(func() {
			d := &a.side.files.dl
			if !d.open || d.gen != g {
				// The dialog this answer is of is gone — closed, or
				// replaced by a newer one. Nothing of it is written:
				// what shows now is another dialog's, a busy flag of
				// the old one's included.
				return
			}
			d.busy = false
			switch {
			case err != nil:
				d.err = remoteError(err)
			case !st.Exists:
				d.err = locales.FilesPathNotFound()
			default:
				d.open = false
				a.downloadFrom(h, st.Path, st.Dir)
			}
		})
	}()
}

// downloadByPath opens the download-by-path dialog on the root the Files
// tree shows: the paths typed in it are that folder's relatives. A host
// not connected says so and opens nothing — fs.stat needs the wire.
func (a *App) downloadByPath(h *host, root string) {
	if h.cl == nil {
		a.toast(locales.HostNotConnected(h.name))
		return
	}
	// A whole new struct, so nothing of the last dialog survives; gen
	// carries over, monotonic, which is what drops stale answers.
	d := &a.side.files.dl
	a.side.files.dl = dlByPath{open: true, host: h, base: root, gen: d.gen + 1}
}

// dlByPathView is the download-by-path dialog: one input, the folder its
// relatives are of, and the answer a wrong path gets in red, the dialog
// staying open to take a better one.
func (a *App) dlByPathView(c *ui.Context, pal *palette) {
	d := &a.side.files.dl
	if !d.open || d.host == nil {
		return
	}
	h := d.host
	submit := false
	ui.DialogBase(c.Key("dl-by-path"), &d.open, func(backdrop, panel ui.Element) {
		backdrop.Background(pal.overlay).Justify(ui.Start).Padding(84, 0, 0, 0)
		floating(panel.Width(480).Padding(22).Gap(8).Radius(14).Background(pal.popover), pal).
			Label(locales.FilesDownloadByPath())
		ui.Text(c, locales.FilesDownloadByPath()).FontSize(15).FontWeight(650).TextColor(pal.text)
		ui.Text(c, locales.FilesDownloadByPathDetail()).FontSize(13).TextColor(pal.textMuted).LineHeight(1.4)
		// The folder its relatives are of, as the picker shows its listed
		// one.
		ui.Text(c, h.shortDir(d.base)).FontSize(12.5).TextColor(pal.textFaint).SingleLine()
		in := ui.TextInputBase(c.Key("q"), &d.query).Grow(1).FontSize(15).
			Placeholder(locales.FilesDownloadByPathPlaceholder()).AutoFocus()
		if in.Changed() {
			d.err = "" // a new path wipes the old answer
		}
		if in.Submitted() {
			submit = true
		}
		if d.err != "" {
			ui.Text(c, d.err).FontSize(13).TextColor(pal.danger).Selectable()
		}
		ui.Row(c).Gap(8).Justify(ui.End).Margin(14, 0, 0, 0).Children(func() {
			if button(c, "cancel", locales.CommonCancel(), "esc", false, pal) {
				d.open = false
			}
			// A stat under way takes the button's place: the Download
			// button waits, it does not take a second ask. The button is
			// no defaultButton: its autofocus would take the focus the
			// input's own needs — the picker's and palette's buttons are
			// plain ones for the same reason — and Enter reaches the
			// input, whose Submitted submits.
			if d.busy {
				ui.Spinner(c)
			} else if button(c, "dl", locales.FilesDownloadByPath(), "↩", true, pal) || c.Shortcut(0, ui.KeyEnter) {
				submit = true
			}
		})
	})
	if submit {
		a.dlByPathSubmit()
	}
	// pickerView's own closing shape; no struct reset — the next
	// downloadByPath writes a whole new one.
	if !d.open {
		a.focusTerminal()
	}
}
