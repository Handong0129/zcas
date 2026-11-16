// Package config 管理可热更新的配置项。
//
// 所有逆向自 ZCode 客户端的"会变的常量"（billing 路由参数等）都放在这里，
// 官方客户端升级导致参数失效时，改 ~/.zcas/config.json 即可恢复，不用等发版。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"zcas/internal/paths"
)

type Config struct {
	// BillingPlatform billing 接口的 platform 路由参数。
	// macOS M 系列默认 darwin-arm64；若服务端拒绝可改回 win32-x64。
	BillingPlatform string `json:"billingPlatform"`
	// BillingAppVersion billing 接口的 app_version 参数兜底值
	//（优先自动读取本机安装的 ZCode 客户端版本，这里只是兑底）。
	BillingAppVersion string `json:"billingAppVersion"`
	// AutoLaunchZCode 启动 zcas 时，若已登录且 ZCode 未运行，则后台自动拉起 ZCode（默认开）。
	// 只想查额度/管理账号、不希望被自动拉起 ZCode 时，在 ~/.zcas/config.json 里设
	// "autoLaunchZCode": false 即可关闭（用指针区分「未设置=默认开」和「显式关」）。
	AutoLaunchZCode *bool `json:"autoLaunchZCode"`
	// CloseAction 点窗口关闭键时的行为："tray"=最小化到托盘、"quit"=直接退出。
	// 未设置时首次关闭会询问并按用户选择写入（空值即「询问」）。
	CloseAction string `json:"closeAction"`
}

// CloseAction 常量。
const (
	CloseToTray = "tray"
	CloseQuit   = "quit"
)

// ValidCloseAction 报告 v 是否为合法的关闭行为值。
func ValidCloseAction(v string) bool {
	return v == "" || v == CloseToTray || v == CloseQuit
}

// ShouldAutoLaunch 报告是否应在启动时自动拉起 ZCode；未显式配置时默认开。
func (c *Config) ShouldAutoLaunch() bool {
	return c.AutoLaunchZCode == nil || *c.AutoLaunchZCode
}

func defaults() *Config {
	return &Config{
		BillingPlatform:   "darwin-arm64",
		BillingAppVersion: "3.14.0",
	}
}

// Load 读取 ~/.zcas/config.json 覆盖默认值，环境变量优先级最高。
func Load() *Config {
	cfg := defaults()
	if b, err := os.ReadFile(paths.ConfigPath()); err == nil {
		_ = json.Unmarshal(b, cfg) // 部分字段失败也用默认值兜底
	}
	if v := os.Getenv("ZCAS_BILLING_PLATFORM"); v != "" {
		cfg.BillingPlatform = v
	}
	if v := os.Getenv("ZCAS_BILLING_APP_VERSION"); v != "" {
		cfg.BillingAppVersion = v
	}
	if cfg.BillingPlatform == "" {
		cfg.BillingPlatform = "darwin-arm64"
	}
	if cfg.BillingAppVersion == "" {
		cfg.BillingAppVersion = "4.1.10"
	}
	return cfg
}

// SetCloseAction 把关闭行为偏好写回 ~/.zcas/config.json。
// 按原文读成 map 合并后再写，避免结构体序列化丢掉文件里的其他未知字段。
func SetCloseAction(v string) error {
	if !ValidCloseAction(v) {
		return fmt.Errorf("非法的关闭行为: %q", v)
	}
	path := paths.ConfigPath()
	raw := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &raw)
	}
	raw["closeAction"] = v
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
