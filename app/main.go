package main

import (
	"embed"
	goruntime "runtime"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"zcas/internal/buildinfo"
)

//go:embed all:frontend/dist
var assets embed.FS

// appMenu 构建应用菜单栏。
// 注意：Wails 原生 role 菜单（AppMenu/EditMenu）的文案硬编码为英文（About/Quit/Undo…），
// 为保持菜单栏语言统一，自定义菜单也用英文。EditMenu 必须保留，否则 macOS 输入框 Cmd+C/V 失效。
//   - macOS：应用菜单自带 About 面板（版本号取自 Info.plist）；Help 放检查更新。
//   - Windows/Linux：Help 里提供检查更新 + About（弹窗由前端实现）。
// 菜单点击通过事件通知前端打开对应弹窗。
func appMenu(a *App) *menu.Menu {
	m := menu.NewMenu()
	if goruntime.GOOS == "darwin" {
		m.Append(menu.AppMenu())
		m.Append(menu.EditMenu())
	}
	help := m.AddSubmenu("Help")
	help.AddText("Check for Updates…", nil, func(*menu.CallbackData) {
		wruntime.EventsEmit(a.ctx, "menu:check-update")
	})
	if goruntime.GOOS != "darwin" {
		help.AddText("About "+buildinfo.Current.DisplayName, nil, func(*menu.CallbackData) {
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
		Menu:             appMenu(app),
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
