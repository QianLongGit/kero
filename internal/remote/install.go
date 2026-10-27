package remote

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"unicode/utf16"

	"kero/internal/shell"
	"kero/internal/update"
)

// Install installs the server build of Kero, of version ("" the latest), on
// a host reached over SSH or WSL, through this machine, whose network the
// host may lack: the machine downloads the host's archive and the release's
// install script into a directory of its own, sends the archive to a file
// of the host's over the connection and the script to sh's standard input,
// and the host installs from the file, with no network of its own. What the
// machine downloaded goes, however the install ends; what the host says
// goes to out. Batch: it asks nothing, as from the app.
//
// A host is of Windows when a connection found it so (Windows), or its sh
// runs on one, as Git's does: it installs with PowerShell from the web
// still, until the relay carries Windows' flow too.
func Install(ctx context.Context, a Addr, version string, batch bool, out io.Writer) error {
	if version != "" {
		version = strings.TrimPrefix(version, "v")
		if !update.ValidVersion(version) {
			return errors.New(version + " is not a version of Kero")
		}
	}
	// run runs a program of sh on the host, as the connection reaches it:
	// over SSH, or through wsl.exe, which runs sh itself, not through the
	// distribution's shell.
	var run func(prog string) *exec.Cmd
	switch a.Scheme {
	case "ssh":
		if Windows(a) {
			return runInstall(windowsInstall(a, version, batch), out, out)
		}
		run = func(prog string) *exec.Cmd {
			cmd := exec.Command(SSHCommand(), append(installSSH(a, batch), "--", a.Host, "sh -c "+shellQuote(prog))...)
			cmd.Env = sshEnv()
			return cmd
		}
	case "wsl":
		wa := []string{}
		if a.Host != "" {
			wa = append(wa, "-d", a.Host)
		}
		run = func(prog string) *exec.Cmd {
			return exec.Command("wsl.exe", append(wa, "--exec", "sh", "-c", prog)...)
		}
	default:
		return errors.New("Kero installs itself on a host over SSH or WSL: install it there with the script of its releases")
	}
	goos, goarch, windows, err := hostPlatform(run, out)
	if err != nil {
		return err
	}
	if windows {
		return runInstall(windowsInstall(a, version, batch), out, out)
	}
	return relayInstall(ctx, run, out, version, goos, goarch)
}

// windowsInstall is the command that installs Kero on a host of Windows
// over SSH: PowerShell runs the release's install script, which it
// downloads from the web, as the relay carries sh's flow only. It may ask
// to trust the host, or for a password, as FindShell does.
func windowsInstall(a Addr, version string, batch bool) *exec.Cmd {
	cmd := exec.Command(SSHCommand(), append(installSSH(a, batch), "--", a.Host, encodedPowerShell(powershellInstall(version)))...)
	cmd.Env = sshEnv()
	hideWindow(cmd)
	return cmd
}

// hostPlatform tells the platform the host names for itself, as the server
// build's archives name theirs: Linux on x86_64 is linux/amd64. A kernel of
// Windows', whose SSH server may run Git's sh, tells Windows, whose install
// is PowerShell's.
func hostPlatform(run func(string) *exec.Cmd, out io.Writer) (goos, goarch string, windows bool, err error) {
	cmd := run(`uname -s -m`)
	hideWindow(cmd)
	var stdout bytes.Buffer
	if err := runInstall(cmd, &stdout, out); err != nil {
		return "", "", false, err
	}
	f := strings.Fields(stdout.String())
	if len(f) != 2 {
		return "", "", false, fmt.Errorf("%s: the host names no platform of Kero's", strings.TrimSpace(stdout.String()))
	}
	switch {
	case f[0] == "Linux":
		goos = "linux"
	case f[0] == "Darwin":
		goos = "darwin"
	case strings.HasPrefix(f[0], "MINGW"), strings.HasPrefix(f[0], "MSYS"), strings.HasPrefix(f[0], "CYGWIN"):
		return "", "", true, nil
	default:
		return "", "", false, fmt.Errorf("Kero is not made for %s", f[0])
	}
	switch f[1] {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	default:
		return "", "", false, fmt.Errorf("Kero is not made for %s", f[1])
	}
	return goos, goarch, false, nil
}

// relayInstall carries the install of Kero through this machine, whose
// network the host lacks: the archive of version for the host's platform
// and the release's install script, downloaded here and checked as the
// script checks them; the archive written to a file of the host's own over
// the connection; the script, run from this machine's copy on sh's standard
// input, installing from that file, offline. The directory of the downloads
// is the machine's own, and goes however the install ends.
func relayInstall(ctx context.Context, run func(string) *exec.Cmd, out io.Writer, version, goos, goarch string) error {
	if version == "" {
		v, err := update.Latest(ctx)
		if err != nil {
			return fmt.Errorf("finding Kero's latest release: %w", err)
		}
		version = v
	}
	dir, err := os.MkdirTemp("", "kero-install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	name := update.Asset(version, goos, goarch)
	archive, err := update.Download(ctx, update.URL(version, name), dir, name)
	if err != nil {
		return fmt.Errorf("downloading Kero %s for %s/%s: %w", version, goos, goarch, err)
	}
	if err := update.Checksum(ctx, archive, update.URL(version, name+".sha256")); err != nil {
		return err
	}
	script, err := update.Download(ctx, update.InstallScript(version), dir, "install-server.sh")
	if err != nil {
		return fmt.Errorf("downloading the install script of Kero %s: %w", version, err)
	}
	f, err := sendFile(run, archive, name, out)
	if err != nil {
		return err
	}
	return installArchive(run, script, f, out)
}

// sendFile writes the file at path to the host, as sh reads the connection
// into a file of a directory it makes itself, and tells the path it wrote.
// Should the writing fail, the host cleans its directory up.
func sendFile(run func(string) *exec.Cmd, path, name string, out io.Writer) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	cmd := run(`d=$(mktemp -d) || exit 1; f="$d/` + name + `"; cat >"$f" || { rm -rf "$d"; exit 1; }; printf %s "$f"`)
	hideWindow(cmd)
	cmd.Stdin = f
	var stdout bytes.Buffer
	if err := runInstall(cmd, &stdout, out); err != nil {
		return "", err
	}
	remote := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(remote, "/") {
		if remote == "" {
			return "", errors.New("the host wrote no file of Kero's")
		}
		return "", fmt.Errorf("%s: the host wrote Kero somewhere else", remote)
	}
	return remote, nil
}

// installArchive runs the install script, from this machine's copy of it
// on sh's standard input, installing the archive the host holds at f,
// offline, and taking the file of it with the directory that held it
// however the install ends.
func installArchive(run func(string) *exec.Cmd, script, f string, out io.Writer) error {
	s, err := os.Open(script)
	if err != nil {
		return err
	}
	defer s.Close()
	cmd := run(`f=` + shellQuote(f) + `; rc=0; sh -s -- "$f" || rc=$?; rm -rf "${f%/*}"; exit $rc`)
	hideWindow(cmd)
	cmd.Stdin = s
	return runInstall(cmd, out, out)
}

// runInstall runs a command of the host's install, its standard output to
// stdout and the rest of what it says to out, and tells how it ended: what
// it said last, as ssh's "Permission denied", is why it failed.
func runInstall(cmd *exec.Cmd, stdout, out io.Writer) error {
	var stderr tail
	cmd.Stdout, cmd.Stderr = stdout, io.MultiWriter(&stderr, out)
	err := cmd.Run()
	if err != nil && stderr.last() != "" {
		return errors.New(stderr.last())
	}
	return err
}

// powershellInstall is the script of PowerShell that installs Kero on
// Windows: install-server.ps1, which it runs from memory, as no policy of
// PowerShell's on scripts then stops it.
func powershellInstall(version string) string {
	args := ""
	if version != "" {
		args = " -Version " + shell.PSQuote(version)
	}
	return `$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue'; ` +
		`[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12; ` +
		`$s = (New-Object Net.WebClient).DownloadString(` + shell.PSQuote(update.InstallScriptPS(version)) + `); ` +
		`& ([scriptblock]::Create($s))` + args
}

// encodedPowerShell is a command, for cmd and PowerShell alike, that runs
// a script of PowerShell: encoded, as neither shell then reads any of it.
func encodedPowerShell(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return "powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand " + base64.StdEncoding.EncodeToString(b)
}

// installSSH are the options of ssh for an install, which, but in batch,
// may ask to trust the host, or for a password.
func installSSH(a Addr, batch bool) []string {
	sa := []string{"-T", "-o", "ConnectTimeout=15"}
	if batch {
		sa = append(sa, "-o", "BatchMode=yes")
	}
	if a.Port != "" {
		sa = append(sa, "-p", a.Port)
	}
	if a.User != "" {
		sa = append(sa, "-l", a.User)
	}
	return sa
}

// FindShell finds whether a host reached over SSH runs commands with a
// shell of Windows, as Windows then reports, for Install: it runs
// sh there, which they have not. A connection finds it too. It may ask as
// an install does, and fails as it would.
func FindShell(a Addr, batch bool) error {
	if a.Scheme != "ssh" || Windows(a) {
		return nil
	}
	cmd := exec.Command(SSHCommand(), append(installSSH(a, batch), "--", a.Host, "sh -c 'exit 0'")...)
	cmd.Env = sshEnv()
	hideWindow(cmd)
	var stderr tail
	cmd.Stdin, cmd.Stderr = os.Stdin, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if sh := windowsShell(ee.ExitCode(), stderr.all()); sh != "" {
			shells.Store(a.String(), sh)
			return nil
		}
	}
	if err != nil && stderr.last() != "" {
		return errors.New(stderr.last())
	}
	return err
}
