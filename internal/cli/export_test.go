package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kero/internal/settings"
)

// isolateCli points kero at an empty home and configuration.
func isolateCli(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KERO_HOME", dir)
	t.Setenv("KERO_CONFIG", filepath.Join(dir, "config"))
	return dir
}

func TestExportCmdWritesFile(t *testing.T) {
	isolateCli(t)
	if err := os.MkdirAll(filepath.Dir(settings.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings.Path(), []byte("host = devbox=me@devbox\n"+
		"theme = mist\nfiles-scp-path = /usr/local/bin/scp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "carry.json")
	if err := exportCmd([]string{"-o", out}); err != nil {
		t.Fatal(err)
	}
	td, err := settings.ParseTransfer(mustReadCli(t, out))
	if err != nil {
		t.Fatal(err)
	}
	if len(td.Hosts) != 1 || td.Hosts[0].Name != "devbox" {
		t.Errorf("hosts %+v", td.Hosts)
	}
	if td.Settings["theme"] != "mist" {
		t.Errorf("settings %q", td.Settings)
	}
	if _, local := td.Settings["files-scp-path"]; local {
		t.Errorf("files-scp-path traveled: %q", td.Settings)
	}
}

func mustReadCli(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestExportCmdStdoutDash(t *testing.T) {
	isolateCli(t)
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = exportCmd([]string{"-o", "-"})
	w.Close()
	os.Stdout = old
	b, err2 := io.ReadAll(r)
	if err != nil || err2 != nil {
		t.Fatalf("%v %v", err, err2)
	}
	if _, err := settings.ParseTransfer(b); err != nil {
		t.Errorf("stdout held no transfer file: %v (%q)", err, b)
	}
}

func TestExportCmdDefaultName(t *testing.T) {
	isolateCli(t)
	t.Chdir(t.TempDir())
	if err := exportCmd(nil); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "kero-data-") || !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Fatalf("no default file written: %+v", entries)
	}
	if _, err := settings.ParseTransfer(mustReadCli(t, entries[0].Name())); err != nil {
		t.Errorf("%s: %v", entries[0].Name(), err)
	}
}

func TestExportCmdUsage(t *testing.T) {
	isolateCli(t)
	if err := exportCmd([]string{"stray"}); !errors.Is(err, errUsage) {
		t.Errorf("stray argument: %v", err)
	}
}
