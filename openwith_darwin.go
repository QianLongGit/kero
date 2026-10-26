//go:build darwin

package main

// The apps the system says open a file, asked of LaunchServices through
// purego — the list the Finder's Open With menu shows, the system's own
// choice first. LaunchServices lives as a framework under CoreServices,
// whose binary the disk no longer holds on its own: dlopen finds it in
// the system's cache all the same. Its CF values come from
// CoreFoundation, both bound once and shared.
//
// CF's memory rules hold throughout: what a Copy or a Create returns is
// ours to release, and what a Get returns stays the owner's — the array
// holds the app URLs, which only borrowing reads.

import (
	"bytes"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/ebitengine/purego"
	"github.com/egoist/mygo/ui"

	"kero/locales"
)

// AppEntry is an app that opens a file, as the menu shows it: by its
// name, at its bundle's path.
type AppEntry struct{ Name, Path string }

// cfUTF8 is kCFStringEncodingUTF8, and cfPOSIXStyle
// kCFURLPOSIXPathStyle, as CoreFoundation's own constants spell them.
const (
	cfUTF8       = 0x08000100
	cfPOSIXStyle = 0
)

// ls is LaunchServices as bound on the first ask, shared by every one
// after; lsErr says why that binding failed, told on every ask.
var (
	lsOnce sync.Once
	lsErr  error
	ls     struct {
		copyApps  func(url uintptr, roles uint32) uintptr // LSCopyApplicationURLsForURL
		urlOfPath func(alloc, b *byte, n int64, isDir bool) uintptr
		arrayLen  func(a uintptr) int64
		arrayAt   func(a uintptr, i int64) uintptr
		urlPath   func(u uintptr, style int32) uintptr
		strToC    func(s uintptr, b *byte, n int64, enc uint32) bool
		release   func(v uintptr)
	}
)

// lsBind opens CoreFoundation and LaunchServices and binds the few
// calls the enumeration needs, once.
func lsBind() {
	cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		lsErr = fmt.Errorf("open with: load CoreFoundation: %w", err)
		return
	}
	lsl, err := purego.Dlopen("/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/LaunchServices", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		lsErr = fmt.Errorf("open with: load LaunchServices: %w", err)
		return
	}
	for _, b := range []struct {
		lib  uintptr
		name string
		into any
	}{
		{lsl, "LSCopyApplicationURLsForURL", &ls.copyApps},
		{cf, "CFURLCreateFromFileSystemRepresentation", &ls.urlOfPath},
		{cf, "CFArrayGetCount", &ls.arrayLen},
		{cf, "CFArrayGetValueAtIndex", &ls.arrayAt},
		{cf, "CFURLCopyFileSystemPath", &ls.urlPath},
		{cf, "CFStringGetCString", &ls.strToC},
		{cf, "CFRelease", &ls.release},
	} {
		if _, err := purego.Dlsym(b.lib, b.name); err != nil {
			lsErr = fmt.Errorf("open with: bind %s: %w", b.name, err)
			return
		}
		purego.RegisterLibFunc(b.into, b.lib, b.name)
	}
}

// applicationsForPath lists the apps LaunchServices says open path, the
// one it would choose itself first. No app claiming the file is an
// empty list, not a failure; a failure is the binding's, told as it is.
func applicationsForPath(path string) ([]AppEntry, error) {
	lsOnce.Do(lsBind)
	if lsErr != nil {
		return nil, lsErr
	}
	// The path goes in as raw bytes, NUL-ended and alive for the call,
	// as the Quick Look bridge passes its own: a CFURL of the file
	// system reads them as they are.
	b := append([]byte(path), 0)
	url := ls.urlOfPath(nil, &b[0], int64(len(path)), false)
	runtime.KeepAlive(b)
	if url == 0 {
		return nil, fmt.Errorf("open with: %s cannot be a file URL", path)
	}
	// kLSRolesAll, every role an app may take with the file: what shows
	// in the Finder's menu names the viewers as the editors.
	arr := ls.copyApps(url, 0xFFFFFFFF)
	ls.release(url)
	if arr == 0 {
		return nil, nil
	}
	apps := make([]AppEntry, 0, ls.arrayLen(arr))
	buf := make([]byte, 4096)
	for i, n := int64(0), ls.arrayLen(arr); i < n; i++ {
		// The app URL is the array's to keep; the path of it is a copy
		// of ours to release. A bundle names itself in its last
		// component, which the .app ends.
		s := ls.urlPath(ls.arrayAt(arr, i), cfPOSIXStyle)
		if s == 0 {
			continue
		}
		var p string
		if ls.strToC(s, &buf[0], int64(len(buf)), cfUTF8) {
			p = string(buf[:bytes.IndexByte(buf, 0)])
		}
		ls.release(s)
		if p == "" {
			continue
		}
		apps = append(apps, AppEntry{Name: strings.TrimSuffix(filepath.Base(p), ".app"), Path: p})
	}
	ls.release(arr)
	return apps, nil
}

// chooseAppMenu adds the submenu of the apps the system says open the
// file, which choosing one runs — macOS's own Open With, of the tree.
func (a *App) chooseAppMenu(m *ui.Menu, path string) {
	m.Submenu(locales.FilesChooseApp(), func(m *ui.Menu) {
		apps, err := applicationsForPath(path)
		switch {
		case err != nil:
			m.Item(err.Error()).Disabled(true)
		case len(apps) == 0:
			m.Item(locales.FilesNoApps()).Disabled(true)
		default:
			for _, app := range apps {
				if m.Item(app.Name).Chosen() {
					a.runOpenWith(openWithCommand(app.Path, path))
				}
			}
		}
	})
}
