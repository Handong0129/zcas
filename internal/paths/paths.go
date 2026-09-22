// Package paths 集中管理所有路径常量。
package paths

import (
	"os"
	"path/filepath"
)

func HomeDir() string {
	home, _ := os.UserHomeDir()
	return home
}

// ZcodeV2Dir ZCode 客户端数据目录（macOS/Linux/Windows 均为 ~/.zcode/v2）。
func ZcodeV2Dir() string { return filepath.Join(HomeDir(), ".zcode", "v2") }

// CredentialsFile 加密的 OAuth token 文件。
func CredentialsFile() string { return filepath.Join(ZcodeV2Dir(), "credentials.json") }

// ConfigFile provider 配置（含明文 JWT apiKey）文件。
func ConfigFile() string { return filepath.Join(ZcodeV2Dir(), "config.json") }

// DataDir 工具自身数据目录：ZCAS_DATA_DIR 环境变量优先，否则 ~/.zcas。
func DataDir() string {
	if v := os.Getenv("ZCAS_DATA_DIR"); v != "" {
		return v
	}
	return filepath.Join(HomeDir(), ".zcas")
}

// StoreDir 账号快照目录。
func StoreDir() string { return filepath.Join(DataDir(), "accounts") }

// BackupDir 切换前登录态备份（回滚用）。
func BackupDir() string { return filepath.Join(DataDir(), ".last") }

// ConfigPath 工具配置文件（billing 参数等可热更新项）。
func ConfigPath() string { return filepath.Join(DataDir(), "config.json") }
