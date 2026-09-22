//go:build darwin

package main

/*
#cgo LDFLAGS: -framework Foundation -framework AppKit
#include <stdlib.h>
void zcasSetDefaultURLHandler(const char* scheme, const char* bundlePath);
*/
import "C"

import (
	"os"
	"strings"
	"unsafe"
)

// setDefaultURLHandler 通过 NSWorkspace 设置 URL scheme 的默认 handler（macOS 12+）。
func setDefaultURLHandler(scheme, bundlePath string) {
	cs := C.CString(scheme)
	cb := C.CString(bundlePath)
	defer C.free(unsafe.Pointer(cs))
	defer C.free(unsafe.Pointer(cb))
	C.zcasSetDefaultURLHandler(cs, cb)
}

// selfBundlePath 返回当前 .app 包路径；非 .app 运行（go run / wails dev）时返回空串。
func selfBundlePath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if i := strings.Index(exe, ".app/"); i >= 0 {
		return exe[:i+len(".app")]
	}
	return ""
}
