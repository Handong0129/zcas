//go:build windows

package tray

import (
	"fmt"
	"strings"
	"sync"

	"github.com/energye/systray"
)

// windowsController 基于 energye/systray：其 Windows 实现自带独立窗口消息循环，
// 可在 goroutine 中运行，不与 Wails 主线程冲突。
type windowsController struct {
	rebuildMu sync.Mutex // 串行化全量重建（Update 可能从多个 goroutine 触发）
}

// New 返回当前平台的托盘控制器。
func New() Controller { return &windowsController{} }

func (w *windowsController) Start(icon []byte, tooltip string, menu []Item) {
	go systray.Run(func() {
		systray.SetIcon(icon)
		systray.SetTooltip(tooltip)
		w.rebuild(menu)
	}, func() {})
}

func (w *windowsController) Update(menu []Item) { w.rebuild(menu) }

func (w *windowsController) SetDockVisible(bool) {} // Windows 无 Dock 概念

func (w *windowsController) Quit() { systray.Quit() }

// rebuild 全量重建菜单（ResetMenu 清空后按模型重加）。
// energye/systray 没有菜单即将展开回调，状态新鲜度靠 Update 的调用方（事件驱动）保证。
func (w *windowsController) rebuild(menu []Item) {
	w.rebuildMu.Lock()
	defer w.rebuildMu.Unlock()
	onClick := OnClick
	systray.ResetMenu()
	addAll(menu, nil, onClick)
}

func addAll(items []Item, parent *systray.MenuItem, onClick func(string)) {
	for _, it := range items {
		if it.Separator {
			systray.AddSeparator()
			continue
		}
		title := it.Title
		if it.Bar != nil {
			// Windows 原生菜单不支持富文本/自绘行（hbmpItem 图标位实测不稳），
			// 用彩色 emoji 方块当进度条：Win10/11 菜单按彩色渲染。
			// 口径与主界面一致：显示「已用 xx%」，填充段数 = 已用占比，
			// 配色阈值同主界面 pct-*（<60% 绿、<85% 黄、其余红）。
			pct := *it.Bar
			filled := int(pct/10 + 0.5)
			if filled < 0 {
				filled = 0
			}
			if filled > 10 {
				filled = 10
			}
			seg := "🟥"
			if pct < 60 {
				seg = "🟩"
			} else if pct < 85 {
				seg = "🟨"
			}
			title = fmt.Sprintf("%s  已用 %.0f%% %s%s", it.Title, pct,
				strings.Repeat(seg, filled), strings.Repeat("⬜", 10-filled))
			if it.Checked {
				title += " 🟢" // 当前账号标记（与 macOS 名字旁绿点同一视觉语言）
			}
		}
		var mi *systray.MenuItem
		if parent == nil {
			mi = systray.AddMenuItem(title, it.Tooltip)
		} else {
			mi = parent.AddSubMenuItem(title, it.Tooltip)
		}
		if it.Disabled {
			mi.Disable()
		}
		if it.Checked && it.Bar == nil {
			mi.Check()
		}
		if it.Action != "" {
			action := it.Action
			mi.Click(func() {
				if onClick != nil {
					onClick(action)
				}
			})
		}
		if len(it.Children) > 0 {
			addAll(it.Children, mi, onClick)
		}
	}
}
