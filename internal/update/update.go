// Package update 版本信息与更新检查（GitHub Releases）。
package update

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"zcas/internal/buildinfo"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

// Result 一次更新检查的结果。
type Result struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	HasUpdate bool   `json:"hasUpdate"`
	URL       string `json:"url"` // 最新版本的发布页
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// Check 查询 GitHub 最新 release 并与当前版本比较。
func Check() (*Result, error) {
	cfg := buildinfo.Current
	req, err := http.NewRequest(http.MethodGet, cfg.LatestReleaseAPI(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "zcas/"+cfg.Version) // GitHub API 要求 UA

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("网络请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("仓库还没有发布版本")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub 返回 %s", resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	r := &Result{
		Current: cfg.Version,
		Latest:  strings.TrimPrefix(rel.TagName, "v"),
		URL:     rel.HTMLURL,
	}
	if r.URL == "" {
		r.URL = cfg.ReleasesURL()
	}
	r.HasUpdate = compareVersions(cfg.Version, r.Latest) < 0
	return r, nil
}

// compareVersions 三段数字版本比较：a<b -1，相等 0，a>b 1。
func compareVersions(a, b string) int {
	pa, pb := splitVer(a), splitVer(b)
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// splitVer 解析 "v0.3.0" / "0.3.0-rc1" → [0 3 0]；非数字段按 0 处理。
func splitVer(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	for i, p := range strings.Split(v, ".") {
		if i >= len(out) {
			break
		}
		out[i], _ = strconv.Atoi(p)
	}
	return out
}
