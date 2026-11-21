package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"kero/internal/build"
)

// isolate points kero at an empty home and configuration, and returns the
// home's path.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KERO_HOME", dir)
	t.Setenv("KERO_CONFIG", filepath.Join(dir, "config"))
	return dir
}

// writeConfig writes a configuration file verbatim.
func writeConfig(t *testing.T, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// configText reads the configuration file back.
func configText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(Target())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// data is a transfer file's contents as JSON, for ParseTransfer and Import.
func data(t *testing.T, td TransferData) []byte {
	t.Helper()
	b, err := json.Marshal(td)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestExportFiltersLocalKeys(t *testing.T) {
	isolate(t)
	writeConfig(t, "# my comment\n"+
		"host = devbox=me@devbox\n"+
		"files-scp-path = /usr/local/bin/scp\n"+
		"command = /bin/zsh\n"+
		"theme = mist\n")
	td, skipped, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	// The keys of this machine — a tool's path, a shell — stay out of the
	// file, but are named, so the summary can say so.
	if !slices.Equal(skipped, []string{"files-scp-path", "command"}) {
		t.Errorf("skipped %q", skipped)
	}
	if want := map[string]string{"theme": "mist"}; !mapsEqual(td.Settings, want) {
		t.Errorf("settings %q", td.Settings)
	}
	if len(td.Hosts) != 1 || td.Hosts[0].Name != "devbox" || td.Hosts[0].Addr != "me@devbox" {
		t.Errorf("hosts %+v", td.Hosts)
	}
}

// mapsEqual compares two maps of strings, for the tests of this file.
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestExportSkipsDefaultsAndHostKey(t *testing.T) {
	isolate(t) // no configuration: the defaults
	td, skipped, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	if len(td.Settings) != 0 || len(skipped) != 0 {
		t.Errorf("settings %q, skipped %q", td.Settings, skipped)
	}
	if td.Format != TransferFormat || td.Version != TransferVersion {
		t.Errorf("format %q version %d", td.Format, td.Version)
	}
	if td.ExportedFrom == "" || td.ExportedAt == "" {
		t.Errorf("exported from %q at %q", td.ExportedFrom, td.ExportedAt)
	}
	if td.AppVersion != build.Version() {
		t.Errorf("appVersion %q, want the build's", td.AppVersion)
	}
}

func TestExportReadsAgentsToml(t *testing.T) {
	isolate(t)
	if td, _, _ := Export(); td.AgentsToml != "" {
		t.Errorf("agentsToml %q with no file", td.AgentsToml)
	}
	if err := os.MkdirAll(filepath.Dir(AgentsPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AgentsPath(), []byte("# my agents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if td, _, err := Export(); err != nil || td.AgentsToml != "# my agents\n" {
		t.Errorf("agentsToml %q, err %v", td.AgentsToml, err)
	}
}

func TestParseTransferRejects(t *testing.T) {
	for _, c := range []struct {
		name, data, wantErr string
	}{
		{"broken json", "{", "not a Kero data file"},
		{"another format", `{"format":"nope","version":1}`, "not a Kero data file"},
		{"future version", `{"format":"kero-data","version":2}`, "version"},
	} {
		_, err := ParseTransfer([]byte(c.data))
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: err %v, want it to say %q", c.name, err, c.wantErr)
		}
	}
	var td TransferData
	if err := json.Unmarshal(data(t, TransferData{Format: TransferFormat, Version: TransferVersion}), &td); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTransfer(data(t, TransferData{Format: TransferFormat, Version: TransferVersion})); err != nil {
		t.Errorf("good file: %v", err)
	}
}

func TestImportHostsUpsert(t *testing.T) {
	isolate(t)
	writeConfig(t, "host = build=ci@build.lab:2222\n")
	sum, err := Import(TransferData{Format: TransferFormat, Version: TransferVersion, Hosts: []Host{
		{Name: "BUILD", Addr: "new@build", Color: "teal"},
		{Name: "new", Addr: "me@new"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if sum.HostsAdded != 1 || sum.HostsReplaced != 1 {
		t.Errorf("summary %+v", sum)
	}
	hosts := Load().Hosts
	// The name keeps its case — it is the same host — while its address
	// and color take the import's.
	if len(hosts) != 2 || hosts[0].Name != "build" || hosts[0].Addr != "new@build" || hosts[0].Color != "teal" ||
		hosts[1].Name != "new" || hosts[1].Addr != "me@new" {
		t.Errorf("hosts %+v", hosts)
	}
}

func TestImportSettingsApplyAndDoubleFilter(t *testing.T) {
	isolate(t)
	writeConfig(t, "files-scp-path = /usr/local/bin/scp\n")
	sum, err := Import(TransferData{Format: TransferFormat, Version: TransferVersion,
		Settings: map[string]string{"theme": "mist", "files-scp-path": "/opt/homebrew/bin/scp"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sum.Settings, []string{"theme"}) || !slices.Equal(sum.Skipped, []string{"files-scp-path"}) {
		t.Errorf("summary %+v", sum)
	}
	if s := Load(); s.LightTheme != "mist" || s.DarkTheme != "mist" {
		t.Errorf("theme %q/%q", s.LightTheme, s.DarkTheme)
	}
	// The local key keeps what this machine had, even though a hand-edited
	// file carried one over.
	if s := Load(); s.FilesScpPath != "/usr/local/bin/scp" {
		t.Errorf("files-scp-path %q", s.FilesScpPath)
	}
}

func TestImportAgentsTomlReplaceAndKeep(t *testing.T) {
	isolate(t)
	writeConfig(t, "")
	if err := os.WriteFile(AgentsPath(), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := Import(TransferData{Format: TransferFormat, Version: TransferVersion})
	if err != nil {
		t.Fatal(err)
	}
	if sum.AgentsToml || string(mustRead(t, AgentsPath())) != "old" {
		t.Errorf("empty agentsToml replaced the file: %+v", sum)
	}
	sum, err = Import(TransferData{Format: TransferFormat, Version: TransferVersion, AgentsToml: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if !sum.AgentsToml || string(mustRead(t, AgentsPath())) != "new" {
		t.Errorf("agentsToml not replaced: %+v", sum)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestImportKeepsComments(t *testing.T) {
	isolate(t)
	writeConfig(t, "# my comment\n"+
		"unknown-key = 42\n"+
		"host = build=ci@build.lab:2222\n"+
		"theme = paper\n")
	if _, err := Import(TransferData{Format: TransferFormat, Version: TransferVersion,
		Settings: map[string]string{"theme": "mist"}, Hosts: []Host{{Name: "new", Addr: "me@new"}}}); err != nil {
		t.Fatal(err)
	}
	text := configText(t)
	for _, want := range []string{"# my comment", "unknown-key = 42", "theme = mist", "host = new=me@new", "host = build=ci@build.lab:2222"} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("file lost %q:\n%s", want, text)
		}
	}
}

func TestImportRefusesBadData(t *testing.T) {
	for _, c := range []struct {
		name string
		td   TransferData
		want string
	}{
		{"unknown key", TransferData{Settings: map[string]string{"no-such-key": "x"}}, "no setting \"no-such-key\""},
		{"bad value", TransferData{Settings: map[string]string{"font-size": "abc"}}, "not a number"},
		{"bad host name", TransferData{Hosts: []Host{{Name: "a b", Addr: "me@x"}}}, "can't name a host"},
	} {
		isolate(t)
		writeConfig(t, "# keep me\ntheme = mist\n")
		before := configText(t)
		_, err := Import(c.td)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want it to say %q", c.name, err, c.want)
		}
		if after := configText(t); after != before {
			t.Errorf("%s: the configuration changed:\n%s", c.name, after)
		}
	}
}

func TestRoundTripIdempotent(t *testing.T) {
	isolate(t)
	writeConfig(t, "# my comment\n"+
		"host = devbox=me@devbox\n"+
		"theme = mist\n"+
		"files-scp-path = /usr/local/bin/scp\n")
	if err := os.WriteFile(AgentsPath(), []byte("# agents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	td, _, err := Export()
	if err != nil {
		t.Fatal(err)
	}
	before, agentsBefore := configText(t), string(mustRead(t, AgentsPath()))
	if _, err := Import(td); err != nil {
		t.Fatal(err)
	}
	if after := configText(t); after != before {
		t.Errorf("the configuration changed:\n%s\n— was:\n%s", after, before)
	}
	if agentsAfter := string(mustRead(t, AgentsPath())); agentsAfter != agentsBefore {
		t.Errorf("agents.toml changed: %q — was %q", agentsAfter, agentsBefore)
	}
}

func TestImportRefusesBadColor(t *testing.T) {
	isolate(t)
	writeConfig(t, "host = a=me@a\n")
	_, err := Import(TransferData{Format: TransferFormat, Version: TransferVersion,
		Hosts: []Host{{Name: "evil", Addr: "me@e", Color: "bogus"}}})
	if err == nil || !strings.Contains(err.Error(), `no color "bogus"`) {
		t.Fatalf("bogus color imported: %v", err)
	}
	if text := string(mustRead(t, Path())); text != "host = a=me@a\n" {
		t.Errorf("config changed by refused import:\n%s", text)
	}
}
