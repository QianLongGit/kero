package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kero/internal/settings"
)

// exportCmd writes what travels to another computer — the hosts, the
// settings and the agents rules (settings.Export) — into one file that
// `kero import` there reads.
func exportCmd(args []string) error {
	f, err := parse(args, map[string]string{"o": "out", "output": "out"}, nil)
	if err != nil {
		return err
	}
	if len(f.pos) > 0 {
		return usageErr("kero export [-o path]")
	}
	td, skipped, err := settings.Export()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(td, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	out := f.vals["out"]
	if out == "" {
		out = "kero-data-" + time.Now().Format("20060102") + ".json"
	}
	if out == "-" {
		_, err = os.Stdout.Write(b)
	} else {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		err = os.WriteFile(out, b, 0o644)
	}
	if err != nil {
		return err
	}
	// The summary goes to stderr, so `-o -` keeps stdout the file alone.
	agents := "no agents.toml"
	if td.AgentsToml != "" {
		agents = "agents.toml"
	}
	where := out
	if out == "-" {
		where = "stdout"
	}
	fmt.Fprintf(os.Stderr, "exported %d hosts, %d settings and %s to %s\n", len(td.Hosts), len(td.Settings), agents, where)
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "skipped %d settings of this machine: %s\n", len(skipped), strings.Join(skipped, ", "))
	}
	return nil
}
