package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"kero/internal/settings"
)

// importCmd merges what another computer's kero export wrote — hosts by
// name, settings one by one, the agents rules — after showing what it
// will do here and asking, unless --yes says not to. The question goes
// to whatever stdin is: a terminal answers it, a pipe may carry a yes,
// and anything else — EOF, a no — leaves the configuration alone.
func importCmd(args []string) error {
	f, err := parse(args, nil, map[string]string{"y": "yes", "yes": "yes"})
	if err != nil {
		return err
	}
	if len(f.pos) != 1 {
		return usageErr("kero import <file> [--yes]")
	}
	b, err := os.ReadFile(f.pos[0])
	if err != nil {
		return err
	}
	td, err := settings.ParseTransfer(b)
	if err != nil {
		return err
	}
	sum, err := settings.PreviewImport(td)
	if err != nil {
		return err
	}
	if from := strings.TrimSpace(td.ExportedFrom + " " + td.AppVersion); from != "" {
		fmt.Printf("exported from %s\n", from)
	}
	fmt.Printf("hosts to merge: %d (%d new, %d replaced)\n", len(td.Hosts), sum.HostsAdded, sum.HostsReplaced)
	if len(sum.Settings) > 0 {
		fmt.Printf("settings to apply: %d\n", len(sum.Settings))
	}
	if len(sum.Skipped) > 0 {
		fmt.Printf("settings kept here (local to the exporter): %s\n", strings.Join(sum.Skipped, ", "))
	}
	if sum.AgentsToml {
		fmt.Println("agents.toml to replace")
	}
	if !f.set["yes"] {
		fmt.Print("Proceed? [y/N] ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return errors.New("no answer; kero import <file> --yes imports without asking")
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			return errors.New("declined; nothing changed")
		}
	}
	if sum, err = settings.Import(td); err != nil {
		return err
	}
	fmt.Printf("imported %d hosts (%d new, %d replaced) and %d settings\n",
		sum.HostsAdded+sum.HostsReplaced, sum.HostsAdded, sum.HostsReplaced, len(sum.Settings))
	if sum.AgentsToml {
		fmt.Println("agents.toml replaced")
	}
	return nil
}
