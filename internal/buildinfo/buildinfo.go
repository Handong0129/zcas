// Package buildinfo 内嵌工程全局配置（project.json），是版本号、应用名、仓库地址
// 等信息的唯一来源：改配置只改同目录的 project.json。
//
// 读取方式：
//   - Go 代码：直接用本包的 Current（go:embed 编译期内嵌，发布二进制自带）
//   - shell/Makefile/CI：scripts/config.py <key>（如 version、author.name）
//   - wails.json / release.yml 等无法运行时读配置的文件：scripts/sync-config.sh 派生
package buildinfo

import (
	_ "embed"
	"encoding/json"
	"strings"
)

//go:embed project.json
var raw []byte

type Author struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Info 工程全局配置，字段与 project.json 一一对应。
type Info struct {
	Name          string `json:"name"`          // 二进制/CLI 命令名
	DisplayName   string `json:"displayName"`   // 应用显示名（窗口标题、菜单、安装后名称）
	Version       string `json:"version"`       // 语义化版本，发版由 scripts/release.sh 递增
	Description   string `json:"description"`   // 一句话简介（桌面入口 Comment 等）
	Repository    string `json:"repository"`    // GitHub 仓库主页
	PkgIdentifier string `json:"pkgIdentifier"` // macOS pkg 安装器标识
	Copyright     string `json:"copyright"`
	Author        Author `json:"author"`
}

// Current 编译期内嵌的配置。配置非法直接 panic——配置错误应在启动时暴露，而不是静默降级。
var Current = func() Info {
	var info Info
	if err := json.Unmarshal(raw, &info); err != nil {
		panic("buildinfo: project.json 非法: " + err.Error())
	}
	return info
}()

// RepoPath GitHub 仓库的 owner/repo 形式（API 路径用）。
func (i Info) RepoPath() string {
	p := strings.TrimPrefix(i.Repository, "https://github.com/")
	return strings.TrimSuffix(p, ".git")
}

// ReleasesURL Release 列表页。
func (i Info) ReleasesURL() string { return i.Repository + "/releases" }

// LatestReleaseAPI 最新 Release 的 API 端点。
func (i Info) LatestReleaseAPI() string {
	return "https://api.github.com/repos/" + i.RepoPath() + "/releases/latest"
}
