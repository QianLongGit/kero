package main

// A remote host's file opens through a cache of kero's own: it comes
// down once by fs.read, any editor's edit to the copy here writes back
// by fs.write after a second of quiet, under the hash of what was read,
// and a file the host changed meanwhile asks before anything overwrites
// it. This file is the whole of that; the opens that use it are routed
// through materialize in files.go, file_pane.go and openwith.go.

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"kero/internal/client"
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

// cacheRoot is where the cache keeps the files it brings down: a folder
// of kero's own, one of each host's under it.
func cacheRoot() string { return filepath.Join(proto.Dir(), "files") }

// cachePath is where a host's file comes to this machine: a folder of
// the host's own, by its name, and in it the file by its path with its
// own name kept after the hash — apps tell a file by its name's end.
func cachePath(hostName, remotePath string) string {
	host := fmt.Sprintf("%x", sha256.Sum256([]byte(hostName)))[:16]
	path := fmt.Sprintf("%x", sha256.Sum256([]byte(remotePath)))[:16]
	return filepath.Join(cacheRoot(), host, path+"-"+cacheName(remotePath))
}

// cacheIndexPath is where the cache's mappings live past the app: panes
// of its files hold local paths, which a new app could not read back.
func cacheIndexPath() string { return filepath.Join(cacheRoot(), "index.json") }

// cacheIndexEntry is one mapping of the cache's index, as the file holds
// it: the file's local path — the map's key — is not of it.
type cacheIndexEntry struct {
	Host   string `json:"host"`
	Remote string `json:"remote"`
}

// cacheName is a remote path's own name, as either kind of host spells
// it, cut from its front when a host allows a longer one than this
// machine's, its extension kept and no rune left half.
func cacheName(remotePath string) string {
	name := proto.DirName(remotePath)
	if ext := filepath.Ext(name); len(name) > 80 {
		cut := max(80-len(ext), 1)
		for cut > 1 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = name[:cut] + ext
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

// cacheHost is a host's connection as the cache writes by it, read on
// the UI thread that owns the hosts, never from a goroutine: nil when
// the host is none of the app's or not connected.
func (a *App) cacheHost(hostName string) *client.Client {
	if h := a.hostNamed(hostName); h != nil {
		return h.cl
	}
	return nil
}

// cacheLocal tells whether a path is one of the cache's own files.
func (a *App) cacheLocal(path string) bool {
	a.cache.mu.Lock()
	defer a.cache.mu.Unlock()
	_, ok := a.cache.entries[path]
	return ok
}

// saveCacheIndexLocked writes the cache's mappings to its index, called
// with the cache's lock held: what the file says and the map holds
// cannot come apart. Best-effort and whole — writeCache's temporary
// rename: a cache that cannot save its index still works for the app it
// is in, a restart only forgets it.
func (a *App) saveCacheIndexLocked() {
	index := make(map[string]cacheIndexEntry, len(a.cache.entries))
	for local, e := range a.cache.entries {
		index[local] = cacheIndexEntry{Host: e.host, Remote: e.remote}
	}
	if b, err := json.Marshal(index); err == nil {
		writeCache(cacheIndexPath(), b)
	}
}

// loadCacheIndex reads the cache's mappings back as the app starts: a
// pane of a cached file, saved with its local path, is read through the
// cache again, and its edits still write back. The hash is not the
// index's to hold: each file is read again, and the watcher carries only
// what changes from here on. A missing or corrupt index, or a file gone
// from the disk, leaves its mapping out — nothing of it syncs.
func (a *App) loadCacheIndex() {
	b, err := os.ReadFile(cacheIndexPath())
	if err != nil {
		return
	}
	var index map[string]cacheIndexEntry
	if json.Unmarshal(b, &index) != nil {
		return
	}
	entries := map[string]*cacheEntry{}
	for local, ix := range index {
		if ix.Host == "" || ix.Remote == "" {
			continue
		}
		data, err := os.ReadFile(local)
		if err != nil {
			continue
		}
		fi, err := os.Stat(local)
		if err != nil {
			continue
		}
		entries[local] = &cacheEntry{host: ix.Host, remote: ix.Remote,
			hash: git.Hash(data), mod: fi.ModTime(), size: fi.Size()}
	}
	if len(entries) == 0 {
		return
	}
	a.cache.entries = entries
	a.startUploadWatcher()
}

// materialize brings a file of a host to this machine, as the file the
// user opens: a remote host's downloads once into the cache, from where
// its edits write back; a local host's file is its own path already.
// cl is the host's connection as the caller's thread, the UI's, read
// it; this runs on a goroutine of its caller's, never the UI thread,
// and reads nothing of the app's own but the cache.
func (a *App) materialize(cl *client.Client, hostName, remotePath string) (string, error) {
	if hostName == "" {
		return remotePath, nil
	}
	if cl == nil {
		return "", errCacheOffline
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
	if err := cl.Call("fs.stat", proto.FSStatParams{Path: remotePath}, &st); err != nil {
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
	if err := cl.Call("fs.read", proto.FSReadParams{Path: remotePath, Max: cacheMax}, &r); err != nil {
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
	a.saveCacheIndexLocked()
	a.cache.mu.Unlock()
	a.startUploadWatcher()
	return local, nil
}

// withLocalFile opens a file of a host as this machine's own: a local
// host's at once on its path, a remote host's brought to the cache
// first, its edits from there written back. The connection it comes by
// is read here, on the caller's UI thread; open runs there too, and a
// file that will not come down is told of, not opened.
func (a *App) withLocalFile(h *host, path string, open func(local string)) {
	if !h.remote() {
		open(path)
		return
	}
	cl, name := h.cl, h.name
	go func() {
		local, err := a.materialize(cl, name, path)
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

// cacheUpload is a cached file's edit leaving, as the watcher queued it:
// what the write needs, snapshotted under the cache's lock, and the
// record it flew from, kept only to tell whether what answers is still
// that one — a re-materialize may have put another in its place, with
// the same host and path and so no value to tell them apart.
type cacheUpload struct {
	local  string
	host   string
	remote string
	hash   string
	of     *cacheEntry
}

// watchCache is one poll of the watcher: it asks the disk what became
// of each file the cache maps — by path, that an atomic save leaves the
// watcher following — drops the mappings whose file went, and queues
// the edits that have kept still cacheQuiet since they last changed.
// The queue hops to the UI thread, where a host's connection is read
// and the upload started; the watcher itself touches nothing of the
// app's but the cache.
func (a *App) watchCache() {
	var queue []cacheUpload
	dropped := false
	a.cache.mu.Lock()
	now := time.Now()
	for local, e := range a.cache.entries {
		if e.uploading {
			continue
		}
		st, err := os.Stat(local)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			delete(a.cache.entries, local)
			dropped = true
		case err != nil:
			// A file the disk will not speak of this poll: the next one.
		case st.ModTime() != e.mod || st.Size() != e.size:
			e.mod, e.size, e.changed = st.ModTime(), st.Size(), now
		case e.changed.IsZero() || now.Sub(e.changed) < cacheQuiet:
			// Still changing, or as it was written: nothing to do.
		default:
			e.uploading = true
			queue = append(queue, cacheUpload{local: local, host: e.host, remote: e.remote, hash: e.hash, of: e})
		}
	}
	if dropped {
		a.saveCacheIndexLocked()
	}
	a.cache.mu.Unlock()
	if len(queue) > 0 {
		a.update(func() {
			for _, u := range queue {
				a.sendUpload(u)
			}
		})
	}
}

// sendUpload starts a queued edit's write back, on the UI thread where
// the hosts live. A host not connected keeps its edit waiting, told of
// no one: it writes once the host is back.
func (a *App) sendUpload(u cacheUpload) {
	cl := a.cacheHost(u.host)
	if cl == nil {
		a.cacheUploaded(u, "", errCacheOffline)
		return
	}
	go a.uploadCache(cl, u)
}

// uploadCache writes a queued edit back to its host, under the hash of
// the content last read or written: a file the host changed meanwhile
// is a conflict for the user to settle, not an overwrite. A connection
// that ended under it is the host's own going, not a write refused:
// the edit waits for the host as one never sent does.
func (a *App) uploadCache(cl *client.Client, u cacheUpload) {
	content, err := os.ReadFile(u.local)
	var sum string
	if err == nil {
		if err = cl.Call("fs.write", proto.FSWriteParams{Path: u.remote, Content: content, Hash: u.hash}, &sum); err != nil {
			select {
			case <-cl.Done():
				err = errCacheOffline
			default:
			}
		}
	}
	a.cacheUploaded(u, sum, err)
}

// cacheUploaded takes what an upload of a cached file came to: the
// record's own state, under the lock, and what the user is told of a
// write that would not go, on the UI thread. A conflict writes nothing
// more until the user answers it. What came belongs to the record only
// while it is still the one the upload flew from — a re-materialize
// may have put another in its place, whose mapping is not this
// answer's to drop nor its hash this answer's to set.
func (a *App) cacheUploaded(u cacheUpload, sum string, err error) {
	var tell func()
	a.cache.mu.Lock()
	e := a.cache.entries[u.local]
	switch {
	case e != u.of:
		// The record is gone, or another took its place; the answer has
		// no one left to tell.
	case errors.Is(err, errCacheOffline):
		e.uploading = false
		e.changed = time.Now() // a second of quiet after the host is back
	case err == nil:
		e.uploading = false
		e.hash, e.changed = sum, time.Time{}
	case errors.Is(err, fs.ErrNotExist):
		// The file went as it uploaded: its record goes with it.
		delete(a.cache.entries, u.local)
		a.saveCacheIndexLocked()
	case strings.HasPrefix(err.Error(), "conflict:"):
		e.uploading = false
		e.changed = time.Time{}
		tell = func() { a.conflictAsk(u.local, e) }
	default:
		e.uploading = false
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
// or the host's own — an overwrite is never silent. The answers run as
// the dialog gives them, on the UI thread; their own writes fly on
// goroutines of their own.
func (a *App) conflictAsk(local string, e *cacheEntry) {
	a.confirm = &confirmation{
		open:   true,
		title:  locales.FilesConflictTitle(),
		detail: locales.FilesConflictDetail(e.remote),
		action: locales.FilesConflictOverwrite(),
		run:    func() { a.cacheOverwrite(local) },
		alt:    locales.FilesConflictRedownload(),
		altRun: func() { a.cacheRefetch(local) },
	}
}

// cacheOverwrite writes the cache's file over the host's, whatever the
// host holds: the user chose the edit here over the host's change. The
// dialog's answer runs it on the UI thread, where the host's connection
// is read; the write itself flies on a goroutine of its own.
func (a *App) cacheOverwrite(local string) {
	e := a.cacheRecord(local)
	if e == nil {
		return
	}
	cl, remote := a.cacheHost(e.host), e.remote
	if cl == nil {
		a.toast(locales.HostNotConnected(e.host))
		return
	}
	go func() {
		content, err := os.ReadFile(local)
		var sum string
		if err == nil {
			err = cl.Call("fs.write", proto.FSWriteParams{Path: remote, Content: content}, &sum)
		}
		if err != nil {
			a.update(func() { a.toast(locales.FilesCantWrite() + ": " + err.Error()) })
			return
		}
		a.cacheSet(local, sum, nil)
	}()
}

// cacheRefetch takes the host's file again over the cache's copy, the
// user having chosen it over the edit here, and resets the record to
// it, so the watcher does not take the fetch for an edit. As the
// overwrite answer, it runs on the UI thread and writes on a goroutine.
func (a *App) cacheRefetch(local string) {
	e := a.cacheRecord(local)
	if e == nil {
		return
	}
	cl, remote := a.cacheHost(e.host), e.remote
	if cl == nil {
		a.toast(locales.HostNotConnected(e.host))
		return
	}
	go func() {
		var r proto.FSRead
		if err := cl.Call("fs.read", proto.FSReadParams{Path: remote, Max: cacheMax}, &r); err != nil {
			a.update(func() { a.toast(locales.FilesCantRead() + ": " + err.Error()) })
			return
		}
		if err := writeCache(local, r.Data); err != nil {
			a.update(func() { a.toast(locales.FilesCantWrite() + ": " + err.Error()) })
			return
		}
		fi, _ := os.Stat(local)
		a.cacheSet(local, git.Hash(r.Data), fi)
	}()
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
