package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"kero/internal/build"
	"kero/internal/remote"
)

// One computer's Kero to another's: a single file holds the hosts, the
// settings and the agents rules that travel, written by Export and merged
// back by Import. Sessions never travel — they live on their hosts, and
// once a host is connected, its sessions are there — and neither do the
// keys of a machine itself, whose paths another's would only lose by
// taking (transferLocalKeys).

const (
	// TransferFormat marks a transfer file as kero's.
	TransferFormat = "kero-data"
	// TransferVersion is the transfer file's version. A file of a version
	// this kero does not know is refused whole, rather than half-applied
	// by a reader that half-understands it.
	TransferVersion = 1
)

// TransferData is what one computer's kero writes for another's to read:
// its hosts, its settings — as the configuration file's keys, a list's
// values joined one a line, as no value the file holds spans lines — and
// its agents rules.
type TransferData struct {
	Format       string            `json:"format"`
	Version      int               `json:"version"`
	ExportedAt   string            `json:"exportedAt"` // RFC3339
	ExportedFrom string            `json:"exportedFrom"`
	AppVersion   string            `json:"appVersion,omitempty"`
	Hosts        []Host            `json:"hosts"`
	Settings     map[string]string `json:"settings"`
	AgentsToml   string            `json:"agentsToml,omitempty"`
}

// transferLocalKeys are the configuration's keys that carry one machine's
// own paths — where it found scp and rsync, the apps its Files view pins,
// where its worktrees and new sessions go, its shell. Another computer's
// answers to those differ (a brew path, a login shell), so Export leaves
// them and Import drops them again, should a hand-edited file carry one
// over: the target's own stand.
var transferLocalKeys = []string{"files-scp-path", "files-rsync-path", "files-pinned-app", "worktree-dir", "new-session-directory", "command"}

// Export collects what travels to another computer. Only what differs
// from kero's defaults goes — a setting the user never chose is the other
// computer's to choose, and carrying a default would quietly decide it —
// and skipped names the machine's own keys the configuration had set, for
// the summary to tell.
func Export() (td TransferData, skipped []string, err error) {
	s := Load()
	td = TransferData{
		Format:       TransferFormat,
		Version:      TransferVersion,
		ExportedAt:   time.Now().Format(time.RFC3339),
		ExportedFrom: runtime.GOOS,
		AppVersion:   build.Version(),
		Hosts:        slices.Clone(s.Hosts),
		Settings:     map[string]string{},
	}
	if b, err := os.ReadFile(AgentsPath()); err == nil {
		td.AgentsToml = string(b)
	} else if !os.IsNotExist(err) {
		return td, nil, err
	}
	def := Default()
	for i := range keys {
		k := &keys[i]
		if k.name == "host" {
			continue // the hosts field carries them, named for what they are
		}
		if slices.Equal(k.get(&s), k.get(&def)) {
			continue
		}
		if slices.Contains(transferLocalKeys, k.name) {
			skipped = append(skipped, k.name)
			continue
		}
		td.Settings[k.name] = strings.Join(k.get(&s), "\n")
	}
	return td, skipped, nil
}

// ParseTransfer reads a transfer file, refusing one that is not kero's or
// of a version this kero does not know.
func ParseTransfer(b []byte) (TransferData, error) {
	var td TransferData
	if err := json.Unmarshal(b, &td); err != nil {
		return td, fmt.Errorf("not a Kero data file: %v", err)
	}
	if td.Format != TransferFormat {
		return td, fmt.Errorf("not a Kero data file (format %q)", td.Format)
	}
	if td.Version != TransferVersion {
		return td, fmt.Errorf("Kero data of version %d; this Kero reads version %d", td.Version, TransferVersion)
	}
	return td, nil
}

// TransferSummary is what an import will do — or did: the hosts it
// merges, the settings it applies, the machine's own keys it skipped, and
// whether the agents rules are replaced.
type TransferSummary struct {
	HostsAdded, HostsReplaced int
	Settings                  []string // the keys applied, in order
	Skipped                   []string // the machine's own keys dropped
	AgentsToml                bool     // agents.toml is replaced
}

// PreviewImport tells what Import would do with a transfer file, writing
// nothing — the Settings window shows it before the user commits.
func PreviewImport(td TransferData) (TransferSummary, error) {
	_, sum, problems := merge(td)
	return sum, problemsErr(problems)
}

// Import merges a transfer file into the configuration — hosts by their
// names, settings key by key — keeping the user's comments, and replaces
// the agents rules when the file carries them. A file it cannot take
// whole changes nothing.
func Import(td TransferData) (TransferSummary, error) {
	s, sum, problems := merge(td)
	if len(problems) > 0 {
		return sum, problemsErr(problems)
	}
	if err := Save(s); err != nil {
		return sum, err
	}
	if td.AgentsToml != "" {
		if err := os.MkdirAll(filepath.Dir(AgentsPath()), 0o755); err != nil {
			return sum, err
		}
		if err := os.WriteFile(AgentsPath(), []byte(td.AgentsToml), 0o644); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

// problemsError joins a merge's problems into the one error they are: an
// import takes the file whole or not at all.
func problemsErr(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("cannot import: %s", strings.Join(problems, "; "))
}

// merge folds a transfer file into the settings as they stand, reporting
// what it would do and what it could not take. PreviewImport and Import
// share it; neither writes.
func merge(td TransferData) (Settings, TransferSummary, []string) {
	s := Load()
	var sum TransferSummary
	var problems []string
	for _, h := range td.Hosts {
		if !remote.ValidName(h.Name) {
			problems = append(problems, fmt.Sprintf("host %q can't name a host", h.Name))
			continue
		}
		if _, err := remote.ParseAddr(h.Addr); err != nil {
			problems = append(problems, fmt.Sprintf("host %s: %v", h.Name, err))
			continue
		}
		// The config reader refuses colors it does not know — a host with
		// one would vanish at the next start — so the import refuses it
		// here, while nothing is written yet.
		if h.Color != "" && !slices.Contains(HostColors, h.Color) {
			problems = append(problems, fmt.Sprintf("host %s: no color %q (%s)", h.Name, h.Color, strings.Join(HostColors, ", ")))
			continue
		}
		if old := s.Host(h.Name); old != nil {
			// The name keeps its case — it names the same host — while
			// what the host is takes the import's.
			old.Addr, old.RemoteControl, old.Color = h.Addr, h.RemoteControl, h.Color
			sum.HostsReplaced++
		} else {
			s.Hosts = append(s.Hosts, h)
			sum.HostsAdded++
		}
	}
	names := make([]string, 0, len(td.Settings))
	for k := range td.Settings {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		if slices.Contains(transferLocalKeys, k) {
			sum.Skipped = append(sum.Skipped, k)
			continue
		}
		key := findKey(k)
		if key == nil {
			problems = append(problems, fmt.Sprintf("no setting %q", k))
			continue
		}
		if err := key.set(&s, strings.Split(td.Settings[k], "\n")); err != nil {
			problems = append(problems, k+": "+err.Error())
			continue
		}
		sum.Settings = append(sum.Settings, k)
	}
	sum.AgentsToml = td.AgentsToml != ""
	return s, sum, problems
}
