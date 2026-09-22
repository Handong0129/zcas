// Package config 管理可热更新的配置项。
//
// 所有逆向自 ZCode 客户端的"会变的常量"（billing 路由参数等）都放在这里，
// 官方客户端升级导致参数失效时，改 ~/.zcas/config.json 即可恢复，不用等发版。
package config

import (
	"encoding/json"
	"os"

	"zcas/internal/paths"
)

type Config struct {
	// BillingPlatform billing 接口的 platform 路由参数。
	// macOS M 系列默认 darwin-arm64；若服务端拒绝可改回 win32-x64。
	BillingPlatform string `json:"billingPlatform"`
	// BillingAppVersion billing 接口的 app_version 参数兜底值
	//（优先自动读取本机安装的 ZCode 客户端版本，这里只是兑底）。
	BillingAppVersion string `json:"billingAppVersion"`
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
