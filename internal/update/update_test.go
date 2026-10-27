package update

import (
	"context"
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The releases are the fork's, where this Kero's versions live: an install
// that reaches for one finds it there, not in the upstream it came from.
func TestRepo(t *testing.T) {
	if Repo != "QianLongGit/kero" {
		t.Errorf("Repo is %q, want QianLongGit/kero", Repo)
	}
	for c, want := range map[string]string{
		InstallScript("2.1.0"): "https://github.com/QianLongGit/kero/releases/download/v2.1.0/install-server.sh",
		InstallScript(""):      "https://github.com/QianLongGit/kero/releases/latest/download/install-server.sh",
		InstallScriptPS(""):    "https://github.com/QianLongGit/kero/releases/latest/download/install-server.ps1",
	} {
		if c != want {
			t.Errorf("script at %s, want %s", c, want)
		}
	}
}

// Download writes a release's file to a directory of the caller's, as the
// machine that relays an install to a host with no network of its own, and
// Checksum checks what it wrote against the release's .sha256, as the
// install script checks what it downloads.
func TestDownloadAndChecksum(t *testing.T) {
	const name = "kero-server-2.1.0-linux-amd64.tar.gz"
	archive := []byte("the archive: kero, and the library it carries")
	sum := sha256.Sum256(archive)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + name:
			w.Write(archive)
		case "/" + name + ".sha256":
			io.WriteString(w, hex.EncodeToString(sum[:])+"  "+name+"\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	path, err := Download(t.Context(), srv.URL+"/"+name, dir, name)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, archive) {
		t.Errorf("wrote %q, want %q", b, archive)
	}
	if err := Checksum(t.Context(), path, srv.URL+"/"+name+".sha256"); err != nil {
		t.Errorf("Checksum: %v", err)
	}
	// A checksum of anything else, as a download gone wrong, fails.
	os.WriteFile(filepath.Join(dir, "other"), []byte("another file"), 0o644)
	if err := Checksum(t.Context(), filepath.Join(dir, "other"), srv.URL+"/"+name+".sha256"); err == nil {
		t.Error("a checksum of another file passes")
	}
	// A file the release does not have, or a checksum that is none, is no
	// download of it.
	if _, err := Download(t.Context(), srv.URL+"/none", dir, "none"); err == nil {
		t.Error("downloaded a file the release does not have")
	}
	if err := Checksum(t.Context(), path, srv.URL+"/"+name); err == nil {
		t.Error("took the archive itself for its checksum")
	}
}

// The key Kero checks is the one mygo.json signs the app's updates with.
func TestPublicKeyIsTheApps(t *testing.T) {
	b, err := os.ReadFile("../../mygo.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Updates struct {
			PublicKey string `json:"publicKey"`
		} `json:"updates"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Updates.PublicKey != PublicKey {
		t.Errorf("update.PublicKey is %q, mygo.json's %q", PublicKey, cfg.Updates.PublicKey)
	}
}

func TestVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	archive := []byte("an archive")
	sig, err := Sign(archive, "2.1.0", "linux", "amd64", base64.StdEncoding.EncodeToString(priv))
	if err != nil {
		t.Fatal(err)
	}
	// Signed, but not with Kero's key.
	if err := Verify(archive, "2.1.0", "linux", "amd64", sig); err == nil {
		t.Error("an archive another key signed passes")
	}
	// The signature is of the archive as that version for that platform.
	msg := message(archive, "2.1.0", "linux", "amd64")
	s, _ := base64.StdEncoding.DecodeString(sig)
	if !ed25519.Verify(pub, msg, s) {
		t.Fatal("Sign signs something else than message")
	}
	for _, other := range [][]byte{message(archive, "2.0.0", "linux", "amd64"), message(archive, "2.1.0", "linux", "arm64"), message([]byte("x"), "2.1.0", "linux", "amd64")} {
		if ed25519.Verify(pub, other, s) {
			t.Errorf("the signature passes for %s", other)
		}
	}
	if err := Verify(archive, "2.1.0", "linux", "amd64", "not base64!"); err == nil {
		t.Error("a bad signature passes")
	}
	if _, err := Sign(archive, "2.1.0", "linux", "amd64", "short"); err == nil {
		t.Error("signed with a bad key")
	}
}

func TestVersions(t *testing.T) {
	for v, ok := range map[string]bool{"2.1.0": true, "2.1.0-beta.1": true, "10.0.12": true, "2.1": false, "../2.1.0": false, "2.1.0/x": false, "v2.1.0": false, "": false} {
		if ValidVersion(v) != ok {
			t.Errorf("ValidVersion(%q) = %v", v, !ok)
		}
	}
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"2.1.0", "2.1.0", 0}, {"2.0.11", "2.1.0", -1}, {"2.10.0", "2.9.9", 1}, {"2.1.0-beta", "2.1.0", -1},
		{"2.1.0", "2.1.0-beta", 1}, {"v3.0.0", "2.99.0", 1}, {"2.1.0-a", "2.1.0-b", -1},
	} {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%s, %s) = %d", c.a, c.b, got)
		}
	}
}

// Only an install of install-server.sh updates itself, and never back to
// an older Kero unless asked.
func TestInstallRefuses(t *testing.T) {
	dir := t.TempDir()
	if err := Install(t.Context(), "2.1.0", dir, "2.0.0", false); err == nil {
		t.Error("updated a directory without the marker")
	}
	os.WriteFile(filepath.Join(dir, Marker), nil, 0o644)
	if err := Install(t.Context(), "2.0.0", dir, "2.1.0", false); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("went back to an older Kero: %v", err)
	}
	if err := Install(t.Context(), "../../x", dir, "2.1.0", true); err == nil {
		t.Error("took a version that is none")
	}
	if err := Install(t.Context(), "2.1.0", dir, "2.1.0", false); err != nil {
		t.Errorf("the version installed: %v", err)
	}
}

func tarball(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipball(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0o755)
		w, _ := zw.CreateHeader(h)
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

func TestReplace(t *testing.T) {
	kero := exe(runtime.GOOS)
	dir := filepath.Join(t.TempDir(), "kero")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, kero), []byte("old"), 0o755)
	os.WriteFile(filepath.Join(dir, "stale.so"), []byte("old"), 0o644)
	if err := Replace(tarball(t, map[string]string{kero: "new", "libghostty-vt.so": "lib", Marker: ""}), dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, kero)); string(b) != "new" {
		t.Errorf("kero is %q", b)
	}
	// Windows' archives are zips.
	if err := Replace(zipball(t, map[string]string{kero: "zipped", Marker: ""}), dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, kero)); string(b) != "zipped" {
		t.Errorf("kero is %q", b)
	}
	if olds, _ := filepath.Glob(filepath.Join(dir, "*"+oldSuffix)); len(olds) > 0 {
		t.Errorf("%v stayed", olds)
	}
	if err := Replace(tarball(t, map[string]string{kero: "new", "libghostty-vt.so": "lib", Marker: ""}), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.so")); err == nil {
		t.Error("a file of the old version stayed")
	}
	for _, leftover := range []string{dir + ".new", dir + ".old"} {
		if _, err := os.Stat(leftover); err == nil {
			t.Errorf("%s stayed", leftover)
		}
	}
	// An archive without kero, or with a path out of it, changes nothing.
	for _, bad := range []map[string]string{{"other": "x"}, {kero: "x"}, {kero: "x", Marker: "", "../escape": "x"}} {
		if err := Replace(tarball(t, bad), dir); err == nil {
			t.Errorf("replaced with %v", bad)
		}
		if err := Replace(zipball(t, bad), dir); err == nil {
			t.Errorf("replaced with a zip of %v", bad)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, kero)); string(b) != "new" {
			t.Errorf("a bad archive changed kero to %q", b)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); err == nil {
		t.Error("a file got out of the archive")
	}
}

// A download whose route dies mid-body — as the release CDN's does on
// flaky networks — comes back on a later try, not as an error the user
// has to act on.
func TestDownloadRetries(t *testing.T) {
	cut := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cut++
		if cut <= 2 {
			w.Header().Set("Content-Length", "100")
			panic(http.ErrAbortHandler)
		}
		w.Write([]byte("the whole file"))
	}))
	defer srv.Close()
	path, err := Download(context.Background(), srv.URL, t.TempDir(), "f")
	if err != nil {
		t.Fatalf("Download after cuts: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "the whole file" || cut != 3 {
		t.Errorf("read %q after %d tries", b, cut)
	}
}
