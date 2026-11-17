// Package tray 系统状态栏（托盘）抽象层。
//
// Wails v2 没有原生托盘；这里按 OS 分实现：
//   - darwin：自带 objc NSStatusItem shim（tray_darwin.go/.m）——第三方 systray 库
//     （energye/fyne/getlantern 系）在 macOS 上都要抢占 NSApplication delegate 或
//     独占主线程事件循环，与 Wails 的 NSApp 循环冲突（会顶掉 OnUrlOpen 等 delegate
//     回调）。自建 status item 只往主队列派发 UI 操作，不动 delegate，可完美共存。
//   - windows：energye/systray（getlantern 维护版），自带独立消息循环，可在 goroutine 跑。
//   - 其余平台（Linux 等）：no-op（暂无环境验证，保留接口）。
package tray

import "sync/atomic"

// Item 托盘菜单项声明。Action 非空表示可点击（点击时以 Action 回调）；
// Action 为空的项按纯展示处理（配合 Disabled 做状态行/标题行）。
// Bar 非空时（0-100）在标题后渲染进度条：darwin 用绿色文字块，其他平台用 Unicode 块。
type Item struct {
	Title     string   `json:"title"`
	Tooltip   string   `json:"tooltip,omitempty"`
	Action    string   `json:"action,omitempty"`
	Disabled  bool     `json:"disabled,omitempty"`
	Checked   bool     `json:"checked,omitempty"`
	Separator bool     `json:"separator,omitempty"`
	Bar       *float64 `json:"bar,omitempty"`
	Children  []Item   `json:"children,omitempty"`
}

// Controller 托盘控制器：Start 建立（含初始菜单），Update 全量重建菜单，Quit 移除托盘图标。
// SetDockVisible 控制应用是否出现在 Dock/任务栏（托盘态隐藏，恢复窗口时显示；
// 仅 darwin 有意义，其他平台 no-op——Windows 无 Dock 概念，从托盘退出即可）。
// Update 可从任意 goroutine 调用；实现内部负责切回 UI 线程。
type Controller interface {
	Start(icon []byte, tooltip string, menu []Item)
	Update(menu []Item)
	SetDockVisible(visible bool)
	Quit()
}

// OnClick 菜单项点击回调（参数为 Item.Action）。
var OnClick func(action string)

// OnOpen 菜单即将展开回调（darwin=menuWillOpen / windows=SetOnRClick，均在弹出前同步调用）：
// 可在此重建菜单，让每次点开都是最新状态。必须快速返回（只读内存级数据），不能等网络。
var OnOpen func()

// OnDockIconClick 应用已在运行时点击 Dock 图标的回调（目前仅 darwin 支持，
// 经 applicationShouldHandleReopen 动态补挂实现）：托盘态下应恢复主窗口。
var OnDockIconClick func()

// appQuitRequested 「应用级退出请求」标志（darwin：Dock 退出 / Cmd+Q 触发，
// 见 tray_darwin.m 的 swizzle 说明）。红叉窗口关闭不会置位。
var appQuitRequested atomic.Bool

// ConsumeAppQuitRequest 读取并清除「应用级退出请求」：Wails 在 macOS 上把红叉关闭
// 和应用退出都走 OnBeforeClose，调用方据此区分「真退出」和「按偏好关闭」。
func ConsumeAppQuitRequest() bool { return appQuitRequested.Swap(false) }
