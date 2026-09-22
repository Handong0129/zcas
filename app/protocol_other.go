//go:build !darwin

package main

// 非 macOS 平台的协议注册占位实现。
// Windows 的协议注册在注册表（HKCU\Software\Classes\zcode），
// Linux 在 .desktop 的 x-scheme-handler，均暂未实现——
// 这两个平台上 OAuth 登录完成后浏览器不会自动跳回，
// 走界面上已有的"复制回调链接手动粘贴"兜底路径即可。

// selfBundlePath 仅 macOS 需要（协议注册指向 .app）。
func selfBundlePath() string { return "" }

// setDefaultURLHandler 仅 macOS 实现（LSSetDefaultHandlerForURLScheme）。
func setDefaultURLHandler(_, _ string) {}
