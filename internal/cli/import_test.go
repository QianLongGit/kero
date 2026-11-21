package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kero/internal/settings"
)

// dataFile writes a transfer file and returns its path.
func dataFile(t *testing.T, td settings.TransferData) string {
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

func TestImportCmdMerges(t *testing.T) {
	isolateCli(t)
	config := "host = a=me@a\n# a comment that must survive\ntheme = paper\n"
	writeCliConfig(t, config)
	path := dataFile(t, settings.TransferData{
		Format: "kero-data", Version: 1,
		ExportedAt: time.Now().Format(time.RFC3339), ExportedFrom: "windows",
		Hosts: []settings.Host{
			{Name: "a", Addr: "new@a", Color: "teal"},
			{Name: "b", Addr: "me@b"},
		},
		Settings:   map[string]string{"theme": "mist", "files-rsync-path": "/hand/edited"},
		AgentsToml: "",
	})
	if err := importCmd([]string{"--yes", path}); err != nil {
		t.Fatal(err)
	}
	s := settings.Load()
	if h := s.Host("a"); h == nil || h.Addr != "new@a" || h.Color != "teal" {
		t.Errorf("host a not replaced: %+v", h)
	}
	if h := s.Host("b"); h == nil || h.Addr != "me@b" {
		t.Errorf("host b not added: %+v", h)
	}
	if s.LightTheme != "mist" || s.DarkTheme != "mist" {
		t.Errorf("theme not applied: %q/%q", s.LightTheme, s.DarkTheme)
	}
	if s.FilesRsyncPath != "" {
		t.Errorf("files-rsync-path should have stayed local: %q", s.FilesRsyncPath)
	}
	text := string(mustReadCli(t, settings.Path()))
	if !strings.Contains(text, "# a comment that must survive") {
		t.Errorf("comment lost:\n%s", text)
	}
}

func writeCliConfig(t *testing.T, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(settings.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings.Path(), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestImportCmdBadFile(t *testing.T) {
	isolateCli(t)
	writeCliConfig(t, "host = a=me@a\n")
	path := filepath.Join(t.TempDir(), "carry.json")
	if err := os.WriteFile(path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := importCmd([]string{path})
	if err == nil || !strings.Contains(err.Error(), "not a Kero data file") {
		t.Fatalf("garbage accepted: %v", err)
	}
	if string(mustReadCli(t, settings.Path())) != "host = a=me@a\n" {
		t.Errorf("config changed by refused import:\n%s", mustReadCli(t, settings.Path()))
	}
}

func TestImportCmdFutureVersion(t *testing.T) {
	isolateCli(t)
	path := dataFile(t, settings.TransferData{Format: "kero-data", Version: 99})
	if err := importCmd([]string{path}); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("future version accepted: %v", err)
	}
}

func TestImportCmdUsage(t *testing.T) {
	isolateCli(t)
	if err := importCmd(nil); !errors.Is(err, errUsage) {
		t.Errorf("no file given: %v", err)
	}
	if err := importCmd([]string{"a", "b"}); !errors.Is(err, errUsage) {
		t.Errorf("two files given: %v", err)
	}
}

func TestImportCmdRefusesWithoutAnswer(t *testing.T) {
	isolateCli(t)
	writeCliConfig(t, "host = a=me@a\n")
	path := dataFile(t, settings.TransferData{Format: "kero-data", Version: 1,
		Hosts: []settings.Host{{Name: "b", Addr: "me@b"}}})
	// Stdin at EOF — nobody to ask — must refuse and touch nothing.
	eof, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	no, w2, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w2.Write([]byte("n\n")); err != nil {
		t.Fatal(err)
	}
	w2.Close()
	old := os.Stdin
	for _, in := range []*os.File{eof, no} {
		os.Stdin = in
		err := importCmd([]string{path})
		os.Stdin = old
		if err == nil {
			t.Fatal("imported with no yes given")
		}
		if text := string(mustReadCli(t, settings.Path())); text != "host = a=me@a\n" {
			t.Errorf("config changed by refused import:\n%s", text)
		}
	}
	// --yes asks nothing of anyone.
	os.Stdin = eof
	err = importCmd([]string{"--yes", path})
	os.Stdin = old
	if err != nil {
		t.Fatalf("--yes with no terminal: %v", err)
	}
	if s := settings.Load(); s.Host("b") == nil {
		t.Errorf("--yes did not import")
	}
}
