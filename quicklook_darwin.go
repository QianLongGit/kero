//go:build darwin

package main

// Quick Look, macOS's spacebar preview, on the file chosen in the Files
// view. The panel is Apple's own window; this drives it from Go through
// purego — objc_msgSend for the calls, and a small NSObject data source
// class, made once, that hands the panel the one URL it previews.
//
// Everything here runs on the app's main thread, where mygo runs its
// event loop and the Files view calls this: AppKit allows no other, and
// the panel's own keys (Esc to close) ride that loop for free.

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// ql is the Quick Look machinery, bound on the first preview and shared
// by every one after. The panel and its data source are singletons of
// the process; the URL is held by us rather than an autorelease pool,
// for the panel may ask for it after the pool of the frame that set it
// has drained.
var (
	qlOnce sync.Once
	qlErr  error // why the one-time binding failed, told on every preview
	ql     struct {
		send              uintptr // objc_msgSend
		NSString, NSURL   uintptr
		panel             uintptr // the shared QLPreviewPanel
		url               uintptr // what the data source serves, retained
		sel               map[string]uintptr
		name              []byte // bytes the runtime holds the class by
		typesCount        []byte // and its methods' type encodings
		typesItem         []byte
		impCount, impItem uintptr // the data source's two IMPs, kept for their life
	}
)

// qlCount and qlItem answer the panel's two questions: one item, the URL
// of the last preview, read fresh each time. Plain funcs on this
// package, capturing nothing of the UI — the IMP table holds them for
// the life of the process.
func qlCount(self, cmd, panel uintptr) uintptr { return 1 }

func qlItem(self, cmd, panel, index uintptr) uintptr { return ql.url }

// qlMsgSend sends a message of integers and objects. A Go pointer goes
// in as uintptr(unsafe.Pointer(…)) right in the call: uintptrescapes
// keeps what it points at alive and still for the call, or Objective-C
// would read bytes the GC had moved.
//
//go:uintptrescapes
func qlMsgSend(recv, sel uintptr, args ...uintptr) uintptr {
	r, _, _ := purego.SyscallN(ql.send, append([]uintptr{recv, sel}, args...)...)
	return r
}

// qlCStr copies s to NUL-ended bytes, as the runtime's C strings are.
// A *byte anchors its bytes for as long as it is held, so the call it
// goes to reads them fine.
func qlCStr(s string) *byte {
	b := append([]byte(s), 0)
	return &b[0]
}

// qlBind opens the frameworks, makes the data source class, and takes
// the shared panel — once, for a class of a name twice over cannot be
// allocated, and the panel is the process's one.
func qlBind() {
	libobjc := "/usr/lib/libobjc.A.dylib"
	libs := map[string]uintptr{libobjc: 0}
	for _, p := range []string{
		libobjc,
		// Quartz, where the panel lives, with what it needs under it.
		"/System/Library/Frameworks/Foundation.framework/Foundation",
		"/System/Library/Frameworks/AppKit.framework/AppKit",
		"/System/Library/Frameworks/Quartz.framework/Quartz",
	} {
		h, err := purego.Dlopen(p, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			qlErr = fmt.Errorf("quick look: load %s: %w", p, err)
			return
		}
		libs[p] = h
	}
	var (
		getClass    func(*byte) uintptr
		registerSel func(*byte) uintptr
		allocClass  func(super uintptr, name *byte, extra uint64) uintptr
		addMethod   func(cls, name, imp uintptr, types *byte) bool
		registerCls func(cls uintptr)
	)
	for _, b := range []struct {
		name string
		into any
	}{
		{"objc_getClass", &getClass},
		{"sel_registerName", &registerSel},
		{"objc_allocateClassPair", &allocClass},
		{"class_addMethod", &addMethod},
		{"objc_registerClassPair", &registerCls},
	} {
		purego.RegisterLibFunc(b.into, libs[libobjc], b.name)
	}
	var err error
	if ql.send, err = purego.Dlsym(libs[libobjc], "objc_msgSend"); err != nil {
		qlErr = fmt.Errorf("quick look: bind objc_msgSend: %w", err)
		return
	}

	ql.sel = map[string]uintptr{}
	for _, s := range []string{
		"alloc", "init", "retain", "release",
		"stringWithUTF8String:", "fileURLWithPath:",
		"sharedApplication", "sharedPreviewPanel", "setDataSource:",
		"reloadData", "makeKeyAndOrderFront:", "orderFrontRegardless",
		"numberOfPreviewItemsInPreviewPanel:", "previewPanel:previewItemAtIndex:",
	} {
		ql.sel[s] = registerSel(qlCStr(s))
	}
	nsApp, qlPanel, nsObject, nsString, nsURL :=
		getClass(qlCStr("NSApplication")), getClass(qlCStr("QLPreviewPanel")), getClass(qlCStr("NSObject")),
		getClass(qlCStr("NSString")), getClass(qlCStr("NSURL"))
	if nsApp == 0 || qlPanel == 0 || nsObject == 0 || nsString == 0 || nsURL == 0 {
		qlErr = errors.New("quick look: a class of Apple's was not found")
		return
	}
	ql.NSString, ql.NSURL = nsString, nsURL
	// The panel wants an application; in the app mygo has made one, and
	// in a bare process, as the tests run in, this makes it.
	qlMsgSend(nsApp, ql.sel["sharedApplication"])

	// The data source class: NSObject with the two methods the panel
	// asks by. Its name and the methods' type encodings are bytes held
	// in ql, for the runtime keeps them.
	ql.name, ql.typesCount, ql.typesItem =
		[]byte("KeroQLDataSource\x00"), []byte("q24@0:8@16\x00"), []byte("@32@0:8@16q24\x00")
	ql.impCount, ql.impItem = purego.NewCallback(qlCount), purego.NewCallback(qlItem)
	cls := allocClass(nsObject, &ql.name[0], 0)
	if cls == 0 {
		qlErr = errors.New("quick look: the data source class could not be made")
		return
	}
	if !addMethod(cls, ql.sel["numberOfPreviewItemsInPreviewPanel:"], ql.impCount, &ql.typesCount[0]) ||
		!addMethod(cls, ql.sel["previewPanel:previewItemAtIndex:"], ql.impItem, &ql.typesItem[0]) {
		qlErr = errors.New("quick look: the data source class could not take its methods")
		return
	}
	registerCls(cls)
	source := qlMsgSend(qlMsgSend(cls, ql.sel["alloc"]), ql.sel["init"])
	ql.panel = qlMsgSend(qlPanel, ql.sel["sharedPreviewPanel"])
	if source == 0 || ql.panel == 0 {
		qlErr = errors.New("quick look: the panel could not be made")
		return
	}
	qlMsgSend(ql.panel, ql.sel["setDataSource:"], source)
}

// quickLook previews a file in the Quick Look panel: the shared panel,
// re-pointed at the file and brought forward. The path is of this
// machine, which the Files view sees to. The panel owns the keyboard
// while it is up, and Esc hands it back.
func quickLook(path string) error {
	qlOnce.Do(qlBind)
	if qlErr != nil {
		return qlErr
	}
	// The path as an NSString: stringWithUTF8String: reads a C string, so
	// it takes bytes of ours, NUL-ended and alive for the call.
	b := append([]byte(path), 0)
	str := qlMsgSend(ql.NSString, ql.sel["stringWithUTF8String:"], uintptr(unsafe.Pointer(&b[0])))
	runtime.KeepAlive(b)
	url := qlMsgSend(ql.NSURL, ql.sel["fileURLWithPath:"], str)
	if str == 0 || url == 0 {
		return fmt.Errorf("quick look: %s cannot be a file URL", path)
	}
	// The URL is kept by us until the next preview lets it go, as above;
	// the panel holds its own to the item it shows.
	qlMsgSend(url, ql.sel["retain"])
	if old := ql.url; old != 0 {
		qlMsgSend(old, ql.sel["release"])
	}
	ql.url = url
	qlMsgSend(ql.panel, ql.sel["reloadData"])
	qlMsgSend(ql.panel, ql.sel["makeKeyAndOrderFront:"], 0)
	qlMsgSend(ql.panel, ql.sel["orderFrontRegardless"])
	return nil
}
