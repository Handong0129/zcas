package main

import (
	"embed"
	goruntime "runtime"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/menu/keys"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"zcas/internal/buildinfo"
	"zcas/internal/platform"
)

//go:embed all:frontend/dist
var assets embed.FS

// trayIcon 托盘图标（复用应用图标；macOS/Linux 用 PNG）。
//
//go:embed build/appicon.png
var trayIcon []byte

// trayIconICO Windows 托盘图标：energye/systray 的 Windows 实现经
// LoadImage(IMAGE_ICON) 加载，只认 ICO 内容——传 PNG 会加载失败，
// 表现为托盘空白图标（Windows 实测）。多尺寸（16/32/48/256）保证托盘小图清晰。
//
//go:embed build/windows/icon.ico
var trayIconICO []byte

// appMenu 构建应用菜单栏。
//   - macOS 应用菜单（第一个菜单）自建而不用原生 AppMenu role：role 会带
//     Hide/Hide Others/Show All 等对本工具无用的系统项，只要 About 和 Quit。
//   - 工具菜单不叫 "Edit"：macOS 会给名为 Edit 的菜单自动追加
//     「自动填充/听写/表情符号」等系统项，改名即消失。
//
// 菜单点击通过事件通知前端（menu:add-account / menu:refresh / menu:settings / menu:about）。
func appMenu(a *App) *menu.Menu {
	m := menu.NewMenu()
	if goruntime.GOOS == "darwin" {
		app := m.AddSubmenu(buildinfo.Current.DisplayName)
		app.AddText("About "+buildinfo.Current.DisplayName, nil, func(*menu.CallbackData) {
			wruntime.EventsEmit(a.ctx, "menu:about")
		})
		app.AddSeparator()
		app.AddText("Quit "+buildinfo.Current.DisplayName, keys.CmdOrCtrl("q"), func(*menu.CallbackData) {
			go a.doQuit()
		})
	}
	tools := m.AddSubmenu("Tools")
	tools.AddText("Add Account…", keys.CmdOrCtrl("n"), func(*menu.CallbackData) {
		wruntime.EventsEmit(a.ctx, "menu:add-account")
	})
	tools.AddText("Launch ZCode", nil, func(*menu.CallbackData) {
		go func() { _ = platform.LaunchZCode() }()
	})
	tools.AddText("Refresh", keys.CmdOrCtrl("r"), func(*menu.CallbackData) {
		wruntime.EventsEmit(a.ctx, "menu:refresh")
	})
	settings := m.AddSubmenu("Settings")
	// 不挂 ⌘,/Ctrl+, 快捷键：Windows 显示内部键名 "OEMComma"；
	// 标题不用 "Preferences…"：macOS 13+ 会把它自动改名为系统统一的 "Settings…"（Settings ▸ Settings 的由来）
	settings.AddText("Options…", nil, func(*menu.CallbackData) {
		wruntime.EventsEmit(a.ctx, "menu:settings")
	})
	if goruntime.GOOS != "darwin" {
		// Windows 顶层菜单必须下拉，macOS 的 About 已并入应用菜单
		about := m.AddSubmenu("About")
		about.AddText("About "+buildinfo.Current.DisplayName, nil, func(*menu.CallbackData) {
			wruntime.EventsEmit(a.ctx, "menu:about")
		})
	}
	return m
}
func main() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:     buildinfo.Current.DisplayName,
		Width:     980,
		Height:    700,
		MinWidth:  860,
		MinHeight: 600,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 13, G: 17, B: 23, A: 1},
		OnStartup:        app.startup,
		OnBeforeClose:    app.beforeClose,
		Menu:             appMenu(app),
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               singleInstanceID,
			OnSecondInstanceLaunch: app.onSecondInstance,
		},
		Mac: &mac.Options{
			Appearance: mac.NSAppearanceNameDarkAqua,
			OnUrlOpen:  app.handleOpenURL,
		},
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		println("Error:", err.Error())
	}
}
