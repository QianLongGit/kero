package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"kero/internal/settings"
	"kero/locales"
)

// importPreview holds a picked transfer file and what merging it would do
// here, while the user decides.
type importPreview struct {
	open bool
	data settings.TransferData
	sum  settings.TransferSummary
	err  string
	busy bool
	gen  int // counts picks, so a late answer lands only on the latest one
}

// transferPage is the Settings page that carries data between computers:
// one file out, one file in.
func (a *App) transferPage(c *ui.Context, pal *palette) {
	settingsGroup(c, pal, locales.PageTransfer(), locales.TransferNote(), func() {
		settingRow(c, pal, locales.TransferExport(), locales.TransferExportDetail(), func() {
			if button(c, "transfer-export", locales.TransferExportButton(), "", false, pal) {
				a.transferExport()
			}
		})
		settingRow(c, pal, locales.TransferImport(), locales.TransferImportDetail(), func() {
			if button(c, "transfer-import", locales.TransferImportButton(), "", true, pal) {
				a.transferImportPick()
			}
		})
	})
}

// transferExport asks where to write, then writes this machine's hosts,
// settings and agents rules there (settings.Export). The file's name says
// what it is and when.
func (a *App) transferExport() {
	go func() {
		path, err := a.xferSave("kero-data-" + time.Now().Format("20060102") + ".json")
		switch {
		case err != nil:
			a.update(func() { a.toast(err.Error()) })
			return
		case path == "":
			return // dismissed; nothing to do
		}
		td, _, err := settings.Export()
		if err == nil {
			var b []byte
			if b, err = json.MarshalIndent(td, "", "  "); err == nil {
				err = os.WriteFile(path, append(b, '\n'), 0o644)
			}
		}
		a.update(func() {
			if err != nil {
				a.toast(err.Error())
				return
			}
			a.toast(locales.TransferExportedToast(path))
		})
	}()
}

// transferImportPick asks for a transfer file, then opens a preview of
// what merging it would do — before anything changes.
func (a *App) transferImportPick() {
	go func() {
		paths, err := a.xferPick(false, false)
		switch {
		case err != nil:
			a.update(func() { a.toast(err.Error()) })
			return
		case len(paths) == 0:
			return // dismissed; nothing to do
		}
		var b []byte
		var td settings.TransferData
		var sum settings.TransferSummary
		if b, err = os.ReadFile(paths[0]); err == nil {
			if td, err = settings.ParseTransfer(b); err == nil {
				sum, err = settings.PreviewImport(td)
			}
		}
		a.update(func() {
			if err != nil {
				a.toast(err.Error())
				return
			}
			a.ss.xfer = importPreview{open: true, data: td, sum: sum, gen: a.ss.xfer.gen + 1}
		})
	}()
}

// transferImportView lays the preview over the Settings window: what the
// file carries, and the choice to merge it.
func (a *App) transferImportView(c *ui.Context, pal *palette) {
	d := &a.ss.xfer
	ui.DialogBase(c.Key("transfer-preview"), &d.open, func(backdrop, panel ui.Element) {
		backdrop.Background(pal.overlay).Justify(ui.Start).Padding(84, 0, 0, 0)
		floating(panel.Width(480).Padding(22).Gap(8).Radius(14).Background(pal.popover), pal)
		panel.Children(func() {
			ui.Text(c, locales.TransferImportTitle()).FontSize(15).FontWeight(650).TextColor(pal.text)
			if d.data.ExportedFrom != "" && d.data.AppVersion != "" {
				previewLine(c, pal, locales.TransferImportFrom(d.data.ExportedFrom, d.data.AppVersion))
			}
			previewLine(c, pal, locales.TransferImportHosts(
				strconv.Itoa(len(d.data.Hosts)), strconv.Itoa(d.sum.HostsAdded), strconv.Itoa(d.sum.HostsReplaced)))
			if n := len(d.sum.Settings); n > 0 {
				previewLine(c, pal, locales.TransferImportSettings(strconv.Itoa(n)))
			}
			if len(d.sum.Skipped) > 0 {
				previewLine(c, pal, locales.TransferImportSkipped(strings.Join(d.sum.Skipped, ", ")))
			}
			if d.data.AgentsToml != "" {
				previewLine(c, pal, locales.TransferAgentsReplace())
			} else {
				previewLine(c, pal, locales.TransferAgentsKeep())
			}
			if d.err != "" {
				ui.Text(c, d.err).FontSize(12.5).TextColor(pal.danger).LineHeight(1.35)
			}
			ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
				if d.busy {
					ui.Spinner(c)
					return
				}
				if button(c, "preview-cancel", locales.CommonCancel(), "esc", false, pal) {
					d.open = false
				}
				if button(c, "preview-import", locales.TransferPreviewButton(), "", true, pal) {
					d.busy = true
					a.transferImportApply()
				}
			})
		})
	})
}

func previewLine(c *ui.Context, pal *palette, text string) {
	ui.Text(c, text).FontSize(12.5).TextColor(pal.textMuted).LineHeight(1.35)
}

// transferImportApply merges the previewed file, then brings the app onto
// what changed — hosts, settings, agents rules — with the Settings
// controls reading the new state.
func (a *App) transferImportApply() {
	td, g := a.ss.xfer.data, a.ss.xfer.gen
	go func() {
		sum, err := settings.Import(td)
		a.update(func() {
			if !a.ss.xfer.open || a.ss.xfer.gen != g {
				return // a newer preview replaced this one
			}
			if err != nil {
				a.ss.xfer.busy = false
				a.ss.xfer.err = err.Error()
				return
			}
			a.ss.xfer = importPreview{gen: g}
			a.apply(settings.Load(), false)
			a.toast(locales.TransferImportedToast(
				strconv.Itoa(sum.HostsAdded+sum.HostsReplaced), strconv.Itoa(len(sum.Settings))))
		})
	}()
}
