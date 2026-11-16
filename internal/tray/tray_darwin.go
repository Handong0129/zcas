//go:build darwin

package tray

/*
#cgo darwin CFLAGS: -DDARWIN -x objective-c -fobjc-arc
#cgo darwin LDFLAGS: -framework Cocoa

#include <stdbool.h>
#include <stdlib.h>

void tray_create(const char* icon, int iconLen, bool template, const char* tooltip, const char* menuJSON);
void tray_set_menu(const char* menuJSON);
void tray_set_dock_visible(bool visible);
void tray_destroy(void);
*/
import "C"

import (
	"encoding/json"
	"sync"
	"unsafe"
)

// darwinController 基于 NSStatusItem 的托盘实现。
// UI 操作全部经 objc 侧 dispatch_async 到主队列执行，与 Wails 的 NSApp 主线程循环共存；
// 不注册 NSApplicationDelegate（避开与 Wails 的 delegate 冲突）。
type darwinController struct{}

var menuMu sync.Mutex // 序列化 menu JSON 的构造（回调与 Update 可能并发）

// New 返回当前平台的托盘控制器。
func New() Controller { return &darwinController{} }

func (darwinController) Start(icon []byte, tooltip string, menu []Item) {
	// 图标是 PNG 二进制（含 NUL），必须按指针+长度传，不能走 CString
	var iconPtr unsafe.Pointer
	if len(icon) > 0 {
		iconPtr = unsafe.Pointer(&icon[0])
	}
	cTooltip := C.CString(tooltip)
	defer C.free(unsafe.Pointer(cTooltip))
	cMenu := C.CString(menuJSON(menu))
	defer C.free(unsafe.Pointer(cMenu))
	C.tray_create((*C.char)(iconPtr), C.int(len(icon)), true, cTooltip, cMenu)
}

func (darwinController) Update(menu []Item) {
	cMenu := C.CString(menuJSON(menu))
	defer C.free(unsafe.Pointer(cMenu))
	C.tray_set_menu(cMenu)
}

func (darwinController) Quit() { C.tray_destroy() }

// SetDockVisible 切换 NSApplication activation policy：托盘态隐藏 Dock 图标，
// 恢复窗口时切回 Regular（图标重新出现）。
func (darwinController) SetDockVisible(visible bool) { C.tray_set_dock_visible(C.bool(visible)) }

func menuJSON(menu []Item) string {
	menuMu.Lock()
	defer menuMu.Unlock()
	b, err := json.Marshal(menu)
	if err != nil {
		return "[]"
	}
	return string(b)
}

//export trayItemClicked
func trayItemClicked(action *C.char) {
	if OnClick == nil {
		return
	}
	a := C.GoString(action)
	go OnClick(a) // 回调可能触发网络/切换等耗时操作，别占住主线程
}

//export trayMenuWillOpen
func trayMenuWillOpen() {
	// 必须同步执行：menuWillOpen 返回后菜单才展开，异步重建会错过本次展示
	if OnOpen != nil {
		OnOpen()
	}
}

//export trayAppQuitRequested
func trayAppQuitRequested() { appQuitRequested.Store(true) }

//export trayDockIconClicked
func trayDockIconClicked() {
	if OnDockIconClick == nil {
		return
	}
	go OnDockIconClick() // Go 侧会做窗口恢复/事件派发，别占住 AppKit 主线程
}
