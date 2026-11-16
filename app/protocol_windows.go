//go:build windows

package main

import (
	"os"

	"zcas/internal/platform"
)

// Windows 的 zcode:// 协议接管：OAuth 发起时把 HKCU 的 scheme 打开方式指向本程序，
// 浏览器回调会以命令行参数启动 zcas.exe；已在运行时经单实例锁把参数转发给首实例
// （App.onSecondInstance 解析 zcode:// 参数后走 handleOpenURL，与 macOS open-url 同路）。
// 完成/取消后归还给官方 ZCode.exe（找不到时删除用户级注册，回落系统级）。

// selfBundlePath 返回当前 exe 路径。
func selfBundlePath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// urlHandlerStash 接管前的原注册命令：归还时写回。
// Windows 版 ZCode 是按用户注册的（HKCU，与我们的接管同址），直接读 HKCU 会读到
// 我们自己写的命令，必须先暂存。
var urlHandlerStash string

// setDefaultURLHandler 把 scheme 的用户级打开方式指向 target exe（接管）。
func setDefaultURLHandler(scheme, target string) {
	if prev := platform.GetURLHandlerCommand(scheme); prev != "" && prev != `"`+target+`" "%1"` {
		urlHandlerStash = prev
	}
	if err := platform.SetURLHandler(scheme, target); err != nil {
		// 接管失败必须留痕：浏览器回调将无法唤起本程序（用户侧表现为
		// "认证通过但 zcas 没反应"），oauth-debug.log 是唯一线索
		oauthLog("协议接管失败: scheme=%s target=%s err=%v", scheme, target, err)
	}
}

// restoreURLSchemeToZCode 把 zcode:// 归还：优先写回接管前的原命令；
// 没有暂存（如进程重启丢了）再找 ZCode.exe；都找不到就删用户级键回落系统默认。
func restoreURLSchemeToZCode() {
	if urlHandlerStash != "" {
		_ = platform.SetURLHandlerCommand("zcode", urlHandlerStash)
		urlHandlerStash = ""
		return
	}
	if exe, err := platform.ZCodeExePath(); err == nil {
		_ = platform.SetURLHandler("zcode", exe)
		return
	}
	platform.DeleteURLHandler("zcode")
}
