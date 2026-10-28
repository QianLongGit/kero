package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"kero/internal/git"
	"kero/internal/proto"
)

func TestHostPath(t *testing.T) {
	home := proto.Home()
	abs := filepath.Join(home, "x")
	for in, want := range map[[2]string]string{
		{"", ""}:             home,
		{"~", ""}:            home,
		{"~/code", ""}:       filepath.Join(home, "code"),
		{abs, "/elsewhere"}:  abs,
		{"src/a.go", abs}:    filepath.Join(abs, "src", "a.go"),
		{"src/a.go", ""}:     filepath.Join(home, "src", "a.go"),
		{"../y", abs}:        filepath.Join(home, "y"),
		{"lib", "~/project"}: filepath.Join(home, "project", "lib"),
	} {
		if got := hostPath(in[0], in[1]); got != want {
			t.Errorf("hostPath(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestFSListAndStat(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"b-dir", "A-dir", ".hidden", "repo/.git"} {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hi"), 0o644)
	l, err := fsList(proto.FSListParams{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range l.Entries {
		names = append(names, e.Name)
		if e.Name == "repo" && !e.Repo {
			t.Error("repo is not a repository")
		}
	}
	if got := strings.Join(names, ","); got != "A-dir,b-dir,repo,file.txt" {
		t.Errorf("listed %s", got)
	}
	if l.Parent != filepath.Dir(dir) || l.Dir != dir {
		t.Errorf("dir %q, parent %q", l.Dir, l.Parent)
	}
	if l, _ := fsList(proto.FSListParams{Dir: dir, Hidden: true}); len(l.Entries) != 5 {
		t.Errorf("with hidden: %d entries", len(l.Entries))
	}
	if _, err := fsList(proto.FSListParams{Dir: filepath.Join(dir, "none")}); err == nil {
		t.Error("listed a directory that is none")
	}
	st := fsStat(proto.FSStatParams{Path: "file.txt", Base: dir})
	if !st.Exists || st.Dir || st.Size != 2 || st.Path != filepath.Join(dir, "file.txt") {
		t.Errorf("stat %+v", st)
	}
	if st := fsStat(proto.FSStatParams{Path: "nope", Base: dir}); st.Exists {
		t.Errorf("stat of nothing %+v", st)
	}
	if r, err := fsRead(proto.FSReadParams{Path: filepath.Join(dir, "file.txt")}); err != nil || string(r.Data) != "hi" {
		t.Errorf("read %q, %v", r.Data, err)
	}
	if _, err := fsRead(proto.FSReadParams{Path: filepath.Join(dir, "file.txt"), Max: 1}); err == nil {
		t.Error("read past Max")
	}
	if _, err := fsRead(proto.FSReadParams{Path: dir}); err == nil {
		t.Error("read a directory")
	}
}

// fsWrite writes what fsRead read back: whole, under the hash of what was
// read, so a file changed meanwhile on the host is a conflict, not an
// overwrite.
func TestFSWrite(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	os.WriteFile(file, []byte("one"), 0o600)

	// Round trip: the content goes back whole, keeping the file's mode.
	if _, err := fsWrite(proto.FSWriteParams{Path: file, Content: []byte("two"), Hash: git.Hash([]byte("one"))}); err != nil {
		t.Fatal(err)
	}
	if r, err := fsRead(proto.FSReadParams{Path: file}); err != nil || string(r.Data) != "two" {
		t.Errorf("read %q, %v", r.Data, err)
	}
	if st, _ := os.Stat(file); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("the mode is %v", st.Mode().Perm())
	}

	// A stale hash conflicts, and the file keeps what it has.
	if _, err := fsWrite(proto.FSWriteParams{Path: file, Content: []byte("three"), Hash: git.Hash([]byte("one"))}); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Errorf("wrote over a change: %v", err)
	}
	if r, _ := fsRead(proto.FSReadParams{Path: file}); string(r.Data) != "two" {
		t.Errorf("after the conflict, the file holds %q", r.Data)
	}

	// An empty hash forces, whatever the file holds.
	if _, err := fsWrite(proto.FSWriteParams{Path: file, Content: []byte("three")}); err != nil {
		t.Fatal(err)
	}

	// A file that is none takes only the empty hash: one deleted since it
	// was read conflicts as one changed does.
	fresh := filepath.Join(dir, "new.txt")
	if _, err := fsWrite(proto.FSWriteParams{Path: fresh, Content: []byte("n"), Hash: git.Hash([]byte("n"))}); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Errorf("made a file under a hash: %v", err)
	}
	if _, err := fsWrite(proto.FSWriteParams{Path: fresh, Content: []byte("n")}); err != nil {
		t.Fatal(err)
	}
	if r, err := fsRead(proto.FSReadParams{Path: fresh}); err != nil || string(r.Data) != "n" {
		t.Errorf("read %q, %v", r.Data, err)
	}
	if st, _ := os.Stat(fresh); runtime.GOOS != "windows" && st.Mode().Perm() != 0o644 {
		t.Errorf("a new file's mode is %v", st.Mode().Perm())
	}

	// A file past what fs.read reads is refused.
	if _, err := fsWrite(proto.FSWriteParams{Path: file, Content: make([]byte, maxRead+1)}); err == nil {
		t.Error("wrote past the limit")
	}

	// A directory for the file that is none.
	if _, err := fsWrite(proto.FSWriteParams{Path: filepath.Join(dir, "none", "x"), Content: []byte("x")}); err == nil {
		t.Error("wrote into a directory that is none")
	}

	// A link the rename would break, to write its target instead: neither.
	link := filepath.Join(dir, "link.txt")
	os.Symlink(file, link)
	if _, err := fsWrite(proto.FSWriteParams{Path: link, Content: []byte("x")}); err == nil {
		t.Error("wrote a link")
	}
}

// fsDelete takes a file, and a folder with all that is in it; no empty
// path, which would be the home itself.
func TestFSDelete(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	sub := filepath.Join(dir, "sub")
	inner := filepath.Join(sub, "b.txt")
	os.WriteFile(file, []byte("a"), 0o600)
	os.MkdirAll(sub, 0o755)
	os.WriteFile(inner, []byte("b"), 0o600)

	if err := fsDelete(proto.FSDeleteParams{Path: file}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("the file is still there: %v", err)
	}
	if err := fsDelete(proto.FSDeleteParams{Path: sub}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inner); !os.IsNotExist(err) {
		t.Error("a folder's files outlived it")
	}
	if err := fsDelete(proto.FSDeleteParams{Path: filepath.Join(dir, "none")}); err != nil {
		t.Errorf("what is not there is no error: %v", err)
	}
	if err := fsDelete(proto.FSDeleteParams{Path: "  "}); err == nil {
		t.Error("an empty path would be the home, whole")
	}
}

// The method answers over the connection, as a client calls it.
func TestFSWriteCall(t *testing.T) {
	d := testDaemon(t)
	file := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(file, []byte("one"), 0o644)
	raw, err := json.Marshal(proto.FSWriteParams{Path: file, Content: []byte("two"), Hash: git.Hash([]byte("one"))})
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.call(nil, "fs.write", raw)
	hash, ok := res.(string)
	if err != nil || !ok || hash != git.Hash([]byte("two")) {
		t.Errorf("call gave %v, %v", res, err)
	}
	if r, _ := fsRead(proto.FSReadParams{Path: file}); string(r.Data) != "two" {
		t.Errorf("the file holds %q", r.Data)
	}
}

func TestPasteFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("XDG_RUNTIME_DIR", "")
	if runtime.GOOS == "windows" {
		t.Setenv("TMP", tmp)
		t.Setenv("TEMP", tmp)
	}
	r, err := pasteFile(proto.PasteFileParams{Name: "../../etc/shot one.png", Data: []byte("png")})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := pasteDir()
	if !strings.HasPrefix(r.Path, dir) || filepath.Base(r.Path) != "shot one.png" {
		t.Errorf("pasted at %s, not in %s", r.Path, dir)
	}
	if b, _ := os.ReadFile(r.Path); string(b) != "png" {
		t.Errorf("pasted %q", b)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Dir(filepath.Dir(r.Path))); st.Mode().Perm() != 0o700 {
			t.Errorf("the paste directory is %v", st.Mode().Perm())
		}
	}
	// Two of one name stay apart.
	r2, _ := pasteFile(proto.PasteFileParams{Name: "shot one.png", Data: []byte("2")})
	if r2.Path == r.Path {
		t.Error("a second paste of a name took the first's place")
	}
	if _, err := pasteFile(proto.PasteFileParams{Name: "big", Data: make([]byte, maxPaste+1)}); err == nil {
		t.Error("pasted a file past the limit")
	}
	if r, _ := pasteFile(proto.PasteFileParams{Name: "", Data: []byte("x")}); filepath.Base(r.Path) != "pasted" {
		t.Errorf("a file of no name pasted as %s", r.Path)
	}
}

// A paste directory someone else could reach is made private, or refused
// when it is not the user's to make so.
func TestPasteDirPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permissions are Unix's")
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("XDG_RUNTIME_DIR", "")
	base := filepath.Join(tmp, fmt.Sprintf("kero-%d", os.Getuid()))
	os.Mkdir(base, 0o777)
	os.Chmod(base, 0o777)
	if _, err := pasteDir(); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(base); st.Mode().Perm() != 0o700 {
		t.Errorf("the base is %v", st.Mode().Perm())
	}
	// A link where the directory goes, as another user could plant.
	os.RemoveAll(base)
	os.Symlink(t.TempDir(), base)
	if _, err := pasteDir(); err == nil {
		t.Error("pasted through a link")
	}
}
