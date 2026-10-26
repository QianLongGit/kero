package main

// A remote host's file opens through a cache of kero's own: it comes
// down once by fs.read, any editor's edit to the copy here writes back
// by fs.write after a second of quiet, under the hash of what was read,
// and a file the host changed meanwhile asks before anything overwrites
// it. This file is the whole of that; the opens that use it are routed
// through materialize in files.go, file_pane.go and openwith.go.

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kero/internal/git"
	"kero/internal/proto"

	"kero/locales"
)

const (
	// cachePoll is how often the watcher asks the disk what became of
	// the files it maps: by path, which survives the atomic saves of
	// editors that replace a file's inode whole.
	cachePoll = 500 * time.Millisecond
	// cacheQuiet is how long a file must keep still after its last
	// change before its edit writes back: editors write in bursts, and
	// one upload of a settled file beats a race through the burst.
	cacheQuiet = time.Second
	// cacheMax is the most a cached file may hold, the fs.read cap: a
	// file past it never comes down.
	cacheMax = int64(32 << 20)
)

// errCacheOffline says an upload could not leave: the host it belongs to
// is not connected. It is told of no one; the write waits for the host.
var errCacheOffline = errors.New("the host is not connected")

// filesCache maps the local path of each file it brought down to what
// it is of the host: the host, the path there, and the hash of the
// content last read or written — the lock the write back holds. It is
// the app's own, not a window's: no frame builds from it.
type filesCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
}

// cacheEntry is one file of the cache, and what its watcher follows:
// the file's mtime and size as last seen, when it last changed, and
// whether an upload of it is under way.
type cacheEntry struct {
	host   string // the host's name, "" never: local files are not cached
	remote string // the path on the host
	hash   string // sha256 hex of the content last read or written
	mod    time.Time
	size   int64
	// changed is when the file last changed, zero while it is as it was
	// written; uploading keeps a second upload from queueing behind one.
	changed   time.Time
	uploading bool
}

// cachePath is where a host's file comes to this machine: a folder of
// the host's own, by its name, and in it the file by its path with its
// own name kept after the hash — apps tell a file by its name's end.
func cachePath(hostName, remotePath string) string {
	host := fmt.Sprintf("%x", sha256.Sum256([]byte(hostName)))[:16]
	path := fmt.Sprintf("%x", sha256.Sum256([]byte(remotePath)))[:16]
	return filepath.Join(proto.Dir(), "files", host, path+"-"+cacheName(remotePath))
}

// cacheName is a remote path's own name, as either kind of host spells
// it, cut from its front when a host allows a longer one than this
// machine's, its extension kept.
func cacheName(remotePath string) string {
	name := proto.DirName(remotePath)
	if ext := filepath.Ext(name); len(name) > 80 {
		name = name[:max(80-len(ext), 1)] + ext
	}
	if name == "" {
		name = "file"
	}
	return name
}

// writeCache puts a file's bytes at its cached path whole, through a
// temporary file renamed over it, so no reader — the watcher among
// them — sees half of one.
func writeCache(local string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(local), ".cache-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, 0o600)
	}
	if err == nil {
		err = os.Rename(name, local)
	}
	if err != nil {
		os.Remove(name)
	}
	return err
}

// cacheCall calls a method of the host a cached file came from, with
// the connection as it is now: a host not connected answers offline,
// to be tried again once it is.
func (a *App) cacheCall(hostName, method string, params, out any) error {
	h := a.hostNamed(hostName)
	if h == nil || h.cl == nil {
		return errCacheOffline
	}
	return h.cl.Call(method, params, out)
}

// cacheLocal tells whether a path is one of the cache's own files.
func (a *App) cacheLocal(path string) bool {
	a.cache.mu.Lock()
	defer a.cache.mu.Unlock()
	_, ok := a.cache.entries[path]
	return ok
}

// materialize brings a file of a host to this machine, as the file the
// user opens: a remote host's downloads once into the cache, from where
// its edits write back; a local host's file is its own path already.
// It blocks on the host's answer, so it runs on a goroutine of its
// caller's, never the UI thread.
func (a *App) materialize(hostName, remotePath string) (string, error) {
	if hostName == "" {
		return remotePath, nil
	}
	local := cachePath(hostName, remotePath)
	a.cache.mu.Lock()
	cached := a.cache.entries[local] != nil
	a.cache.mu.Unlock()
	if cached {
		if _, err := os.Stat(local); err == nil {
			a.startUploadWatcher()
			return local, nil
		}
		// The copy went — the cache emptied, cleaned: read it down again.
	}
	var st proto.FSStat
	if err := a.cacheCall(hostName, "fs.stat", proto.FSStatParams{Path: remotePath}, &st); err != nil {
		return "", err
	}
	if !st.Exists || st.Dir {
		return "", errFilesGone
	}
	if st.Size > cacheMax {
		// fs.read reads at most cacheMax: a file past it is said here,
		// rather than by a download that could not come whole.
		return "", fmt.Errorf("%s is larger than %d MB", remotePath, cacheMax>>20)
	}
	var r proto.FSRead
	if err := a.cacheCall(hostName, "fs.read", proto.FSReadParams{Path: remotePath, Max: cacheMax}, &r); err != nil {
		return "", err
	}
	if err := writeCache(local, r.Data); err != nil {
		return "", err
	}
	fi, err := os.Stat(local)
	if err != nil {
		return "", err
	}
	a.cache.mu.Lock()
	if a.cache.entries == nil {
		a.cache.entries = map[string]*cacheEntry{}
	}
	// What the watcher first sees is the file as it landed: the fetch
	// itself is no edit.
	a.cache.entries[local] = &cacheEntry{host: hostName, remote: remotePath, hash: git.Hash(r.Data),
		mod: fi.ModTime(), size: fi.Size()}
	a.cache.mu.Unlock()
	a.startUploadWatcher()
	return local, nil
}

// withLocalFile opens a file of a host as this machine's own: a local
// host's at once on its path, a remote host's brought to the cache
// first, its edits from there written back. open runs on the UI thread;
// a file that will not come down is told of, not opened.
func (a *App) withLocalFile(h *host, path string, open func(local string)) {
	if !h.remote() {
		open(path)
		return
	}
	go func() {
		local, err := a.materialize(h.name, path)
		a.update(func() {
			if err != nil {
				a.toast(locales.FilesCantRead() + ": " + remoteError(err))
				return
			}
			open(local)
		})
	}()
}

// startUploadWatcher starts, once for the app, the watcher that carries
// the cache's edits back: it runs as long as the app does.
func (a *App) startUploadWatcher() {
	a.watchOnce.Do(func() {
		go func() {
			for range time.Tick(cachePoll) {
				a.watchCache()
			}
		}()
	})
}

// watchCache is one poll of the watcher: it asks the disk what became
// of each file the cache maps — by path, that an atomic save leaves the
// watcher following — drops the mappings whose file went, and uploads
// the edits that have kept still cacheQuiet since they last changed.
func (a *App) watchCache() {
	a.cache.mu.Lock()
	defer a.cache.mu.Unlock()
	now := time.Now()
	for local, e := range a.cache.entries {
		if e.uploading {
			continue
		}
		st, err := os.Stat(local)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			delete(a.cache.entries, local)
		case err != nil:
			// A file the disk will not speak of this poll: the next one.
		case st.ModTime() != e.mod || st.Size() != e.size:
			e.mod, e.size, e.changed = st.ModTime(), st.Size(), now
		case e.changed.IsZero() || now.Sub(e.changed) < cacheQuiet:
			// Still changing, or as it was written: nothing to do.
		default:
			e.uploading = true
			go a.uploadCache(local, e)
		}
	}
}

// uploadCache writes a cached file's edit back to its host, under the
// hash of the content last read or written: a file the host changed
// meanwhile is a conflict for the user to settle, not an overwrite.
func (a *App) uploadCache(local string, e *cacheEntry) {
	content, err := os.ReadFile(local)
	var sum string
	if err == nil {
		err = a.cacheCall(e.host, "fs.write", proto.FSWriteParams{Path: e.remote, Content: content, Hash: e.hash}, &sum)
	}
	a.cacheUploaded(local, e, sum, err)
}

// cacheUploaded takes what an upload of a cached file came to: the
// record's own state, under the lock, and what the user is told of a
// write that would not go, on the UI thread. A conflict writes nothing
// more until the user answers it.
func (a *App) cacheUploaded(local string, e *cacheEntry, sum string, err error) {
	var tell func()
	a.cache.mu.Lock()
	e.uploading = false
	_, live := a.cache.entries[local]
	switch {
	case errors.Is(err, errCacheOffline):
		e.changed = time.Now() // a second of quiet after the host is back
	case err == nil:
		e.hash, e.changed = sum, time.Time{}
	case errors.Is(err, fs.ErrNotExist):
		// The file went as it uploaded: its record goes with it.
		if a.cache.entries[local] == e {
			delete(a.cache.entries, local)
		}
	case !live:
		// The record is gone; the answer has no one left to tell.
	case strings.HasPrefix(err.Error(), "conflict:"):
		e.changed = time.Time{}
		tell = func() { a.conflictAsk(local, e) }
	default:
		e.changed = time.Time{}
		msg := err.Error()
		if strings.Contains(msg, "unknown method") {
			// The host's kerod predates fs.write: the files' own hint to
			// update it says so in the user's words.
			msg = remoteError(err)
		} else {
			msg = locales.FilesCantWrite() + ": " + msg
		}
		tell = func() { a.toast(msg) }
	}
	a.cache.mu.Unlock()
	if tell != nil {
		a.update(tell)
	}
}

// conflictAsk asks which of two changes to a file wins: the edit here,
// or the host's own — an overwrite is never silent.
func (a *App) conflictAsk(local string, e *cacheEntry) {
	a.confirm = &confirmation{
		open:   true,
		title:  locales.FilesConflictTitle(),
		detail: locales.FilesConflictDetail(e.remote),
		action: locales.FilesConflictOverwrite(),
		run:    func() { go a.cacheOverwrite(local) },
		alt:    locales.FilesConflictRedownload(),
		altRun: func() { go a.cacheRefetch(local) },
	}
}

// cacheOverwrite writes the cache's file over the host's, whatever the
// host holds: the user chose the edit here over the host's change.
func (a *App) cacheOverwrite(local string) {
	e := a.cacheRecord(local)
	if e == nil {
		return
	}
	content, err := os.ReadFile(local)
	var sum string
	if err == nil {
		err = a.cacheCall(e.host, "fs.write", proto.FSWriteParams{Path: e.remote, Content: content}, &sum)
	}
	if err != nil {
		a.update(func() { a.toast(locales.FilesCantWrite() + ": " + err.Error()) })
		return
	}
	a.cacheSet(local, sum, nil)
}

// cacheRefetch takes the host's file again over the cache's copy, the
// user having chosen it over the edit here, and resets the record to
// it, so the watcher does not take the fetch for an edit.
func (a *App) cacheRefetch(local string) {
	e := a.cacheRecord(local)
	if e == nil {
		return
	}
	var r proto.FSRead
	if err := a.cacheCall(e.host, "fs.read", proto.FSReadParams{Path: e.remote, Max: cacheMax}, &r); err != nil {
		a.update(func() { a.toast(locales.FilesCantRead() + ": " + err.Error()) })
		return
	}
	if err := writeCache(local, r.Data); err != nil {
		a.update(func() { a.toast(locales.FilesCantWrite() + ": " + err.Error()) })
		return
	}
	fi, _ := os.Stat(local)
	a.cacheSet(local, git.Hash(r.Data), fi)
}

// cacheRecord is the cache's record of a local file, taken under its
// lock, nil while the file is none of the cache's.
func (a *App) cacheRecord(local string) *cacheEntry {
	a.cache.mu.Lock()
	defer a.cache.mu.Unlock()
	return a.cache.entries[local]
}

// cacheSet takes what came of the host for a cached file into its
// record — the one that answers by the local path, which a refetch
// meanwhile may have made another — as its new hash, at rest.
func (a *App) cacheSet(local, sum string, fi os.FileInfo) {
	a.cache.mu.Lock()
	defer a.cache.mu.Unlock()
	e := a.cache.entries[local]
	if e == nil {
		return
	}
	e.hash, e.changed = sum, time.Time{}
	if fi != nil {
		e.mod, e.size = fi.ModTime(), fi.Size()
	}
}
