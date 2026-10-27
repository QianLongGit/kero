package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Install carries the server build of Kero to a host through this machine:
// it asks the host its platform, downloads its archive and the install
// script from the releases into a directory of its own, sends the archive
// to a file of the host's and the script to sh's standard input, and the
// host installs from what it was sent, offline — no curl of GitHub runs
// there, and nothing of the machine's stays behind.
func TestInstallRelay(t *testing.T) {
	const name = "kero-server-2.1.0-linux-amd64.tar.gz"
	archive := []byte("the server build of Kero: kero, and the library it carries")
	script := "#!/bin/sh\n# install-server.sh, of the release\n"
	sum := sha256.Sum256(archive)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2.1.0/" + name:
			w.Write(archive)
		case "/v2.1.0/" + name + ".sha256":
			io.WriteString(w, hex.EncodeToString(sum[:])+"  "+name+"\n")
		case "/v2.1.0/install-server.sh":
			io.WriteString(w, script)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("KERO_RELEASES", srv.URL)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KERO_SSH", exe)
	t.Setenv("KERO_TEST_SHELL", "sh")
	log := filepath.Join(t.TempDir(), "ssh.log")
	t.Setenv("KERO_TEST_SSH_LOG", log)
	// Where the machine keeps what it downloads, and the host what it is
	// sent: neither may stay behind. The host tells what it took of the
	// archive and the script in files of its own.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("KERO_TEST_ARCHIVE", filepath.Join(tmp, "sent-archive"))
	t.Setenv("KERO_TEST_SCRIPT", filepath.Join(tmp, "sent-script"))

	var out bytes.Buffer
	if err := Install(context.Background(), Addr{Scheme: "ssh", Host: "devbox"}, "2.1.0", true, &out); err != nil {
		t.Fatalf("%v: %s", err, &out)
	}
	b, _ := os.ReadFile(log)
	cmds := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(cmds) != 3 {
		t.Fatalf("ran %d commands: %s", len(cmds), b)
	}
	for _, c := range cmds {
		if strings.Contains(c, "curl") || strings.Contains(c, "wget") || strings.Contains(c, "github.com") {
			t.Errorf("%s: the host installs from what it is sent, not the web", c)
		}
	}
	if !strings.Contains(cmds[0], "uname -s -m") {
		t.Errorf("the host's platform: %s", cmds[0])
	}
	if !strings.Contains(cmds[1], "cat >") || !strings.Contains(cmds[1], name) {
		t.Errorf("the archive's sending: %s", cmds[1])
	}
	if !strings.Contains(cmds[2], "sh -s -- ") || !strings.Contains(cmds[2], "rm -rf") {
		t.Errorf("the offline install: %s", cmds[2])
	}
	// What was sent arrived whole.
	if b, _ := os.ReadFile(filepath.Join(tmp, "sent-archive")); !bytes.Equal(b, archive) {
		t.Errorf("the host got %q of the archive", b)
	}
	if b, _ := os.ReadFile(filepath.Join(tmp, "sent-script")); string(b) != script {
		t.Errorf("the host got %q of the install script", b)
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "kero-install-*")); len(left) > 0 {
		t.Errorf("%v stayed behind", left)
	}
}

// A version that is none, and a host the connection cannot reach, end an
// install before anything runs there.
func TestInstallFails(t *testing.T) {
	if err := Install(context.Background(), Addr{Scheme: "ssh", Host: "devbox"}, "2.1", true, io.Discard); err == nil || !strings.Contains(err.Error(), "not a version") {
		t.Errorf("a version that is none: %v", err)
	}
	if err := Install(context.Background(), Addr{Scheme: "http", Host: "box"}, "2.1.0", true, io.Discard); err == nil || !strings.Contains(err.Error(), "over SSH or WSL") {
		t.Errorf("an address Kero cannot install through: %v", err)
	}
}

// install-server.sh installs from an archive already on the machine, as
// Install relays one: it downloads nothing, takes the version off the
// archive's name, and refuses an archive of another platform.
func TestInstallScriptOffline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh of Windows takes no Unix paths")
	}
	if os.Getuid() == 0 {
		t.Skip("root: the install script refuses to run as root")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	script, err := filepath.Abs("../../scripts/install-server.sh")
	if err != nil {
		t.Fatal(err)
	}
	// A server build of the test's own platform: a kero that answers its
	// version, packed as the releases pack it.
	home, stage := t.TempDir(), t.TempDir()
	name := fmt.Sprintf("kero-server-9.9.9-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archive := filepath.Join(stage, name)
	os.WriteFile(filepath.Join(stage, "kero"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	tarCmd := exec.Command("tar", "-czf", archive, "-C", stage, "kero")
	if out, err := tarCmd.CombinedOutput(); err != nil {
		t.Fatalf("tar: %v: %s", err, out)
	}
	// The releases are nowhere: an install that reached for them would end
	// at once.
	run := func(args ...string) string {
		cmd := exec.Command(sh, append([]string{script}, args...)...)
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + t.TempDir(), "KERO_RELEASES=http://127.0.0.1:1"}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		if err != nil {
			t.Fatalf("%v: %s", err, &out)
		}
		return out.String()
	}
	out := run(archive, "--no-service")
	lib := filepath.Join(home, ".local", "lib", "kero")
	if _, err := os.Stat(filepath.Join(lib, "kero")); err != nil {
		t.Errorf("no kero in %s: %s", lib, out)
	}
	if _, err := os.Stat(filepath.Join(lib, ".kero-server")); err != nil {
		t.Error("the install is not one that updates itself")
	}
	link := filepath.Join(home, ".local", "bin", "kero")
	if got, err := os.Readlink(link); err != nil || got != filepath.Join(lib, "kero") {
		t.Errorf("kero links to %q, %v", got, err)
	}
	if !strings.Contains(out, "Kero 9.9.9 is installed") {
		t.Errorf("installed nothing of the archive's version: %s", out)
	}
	if !strings.Contains(out, "from "+archive) {
		t.Errorf("said nothing of installing from the file: %s", out)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Error("took the archive it was given away")
	}
	// An archive not of the host's platform is none to install.
	wrong := filepath.Join(stage, fmt.Sprintf("kero-server-9.9.9-%s-unknown.tar.gz", runtime.GOOS))
	os.Link(archive, wrong)
	cmd := exec.Command(sh, script, wrong, "--no-service")
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + t.TempDir()}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Run(); err == nil || !strings.Contains(buf.String(), "is not an archive of the server build") {
		t.Errorf("an archive of another platform: %v: %s", err, &buf)
	}
}
