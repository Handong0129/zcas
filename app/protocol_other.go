//go:build !darwin && !windows

package main

// Linux 的协议注册占位实现（.desktop 的 x-scheme-handler，暂未实现）。
// OAuth 登录完成后浏览器不会自动跳回，走界面上已有的"复制回调链接手动粘贴"兜底路径。

// selfBundlePath 仅 macOS / Windows 需要。
func selfBundlePath() string { return "" }

// setDefaultURLHandler 仅 macOS / Windows 实现。
func setDefaultURLHandler(_, _ string) {}

// restoreURLSchemeToZCode 仅 macOS / Windows 实现。
func restoreURLSchemeToZCode() {}
