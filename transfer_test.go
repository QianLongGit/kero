package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kero/internal/settings"
	"kero/locales"

	"github.com/egoist/mygo/ui"
)

// soon asks until it stops erroring, or fails the test.
func soon(t *testing.T, ask func() error) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for err := ask(); err != nil; err = ask() {
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func writeTransferConfig(t *testing.T, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(settings.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings.Path(), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func transferDataFile(t *testing.T, td settings.TransferData) string {
	t.Helper()
	b, err := json.Marshal(td)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "carry.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTransferExportWritesFile(t *testing.T) {
	a := testApp(t)
	writeTransferConfig(t, "host = dev=me@dev\ntheme = mist\n")
	a.updateFn = func(fn func()) { fn() }
	path := filepath.Join(t.TempDir(), "carry.json")
	a.xferSave = func(string) (string, error) { return path, nil }
	a.transferExport()
	soon(t, func() error {
		if _, err := os.Stat(path); err != nil {
			return err
		}
		if a.pendingToast == "" {
			return errors.New("no toast after export")
		}
		return nil
	})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	td, err := settings.ParseTransfer(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(td.Hosts) != 1 || td.Hosts[0].Name != "dev" {
		t.Errorf("hosts %+v", td.Hosts)
	}
	if td.Settings["theme"] != "mist" {
		t.Errorf("settings %q", td.Settings)
	}
	if a.pendingToast != locales.TransferExportedToast(path) {
		t.Errorf("toast %q", a.pendingToast)
	}
}

func TestTransferImportPreviewAndApply(t *testing.T) {
	a := testApp(t)
	writeTransferConfig(t, "host = dev=me@dev\n")
	path := transferDataFile(t, settings.TransferData{
		Format: "kero-data", Version: 1, ExportedFrom: "windows", AppVersion: "1.2.3",
		Hosts: []settings.Host{
			{Name: "dev", Addr: "me@ten", Color: "teal"},
			{Name: "win", Addr: "me@win"},
		},
		Settings: map[string]string{"theme": "mist", "files-scp-path": "/hand/edited"},
	})
	a.updateFn = func(fn func()) { fn() }
	a.xferPick = func(bool, bool) ([]string, error) { return []string{path}, nil }
	a.transferImportPick()
	soon(t, func() error {
		if !a.ss.xfer.open {
			return errors.New("no preview dialog")
		}
		return nil
	})
	a.ss.page = "transfer"
	tt := ui.NewTester(a.settingsView, 820, 640)
	tt.Frame()
	if !tt.HasText(locales.TransferImportFrom("windows", "1.2.3")) {
		t.Fatal("preview lacks the exported-from line")
	}
	if !tt.HasText(locales.TransferImportHosts("2", "1", "1")) {
		t.Fatal("preview lacks the hosts line")
	}
	if !tt.HasText(locales.TransferImportSkipped("files-scp-path")) {
		t.Fatal("preview lacks the skipped-settings line")
	}
	if err := tt.Click(locales.TransferPreviewButton()); err != nil {
		t.Fatal(err)
	}
	soon(t, func() error {
		if a.ss.xfer.open {
			return errors.New("dialog still open")
		}
		if a.pendingToast == "" {
			return errors.New("no toast after import")
		}
		return nil
	})
	s := settings.Load()
	if h := s.Host("dev"); h == nil || h.Addr != "me@ten" || h.Color != "teal" {
		t.Errorf("host dev not replaced: %+v", h)
	}
	if h := s.Host("win"); h == nil || h.Addr != "me@win" {
		t.Errorf("host win not added: %+v", h)
	}
	if s.FilesScpPath != "" {
		t.Errorf("files-scp-path should have stayed local: %q", s.FilesScpPath)
	}
	if a.set.Host("win") == nil {
		t.Errorf("the live settings were not refreshed")
	}
}

func TestTransferImportBadFileToasts(t *testing.T) {
	a := testApp(t)
	a.updateFn = func(fn func()) { fn() }
	path := filepath.Join(t.TempDir(), "carry.json")
	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.xferPick = func(bool, bool) ([]string, error) { return []string{path}, nil }
	a.transferImportPick()
	soon(t, func() error {
		if a.pendingToast == "" {
			return errors.New("no toast for the bad file")
		}
		return nil
	})
	if !strings.Contains(a.pendingToast, "not a Kero data file") {
		t.Errorf("toast %q", a.pendingToast)
	}
	if a.ss.xfer.open {
		t.Errorf("a bad file opened the preview")
	}
}
