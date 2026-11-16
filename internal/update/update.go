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
	tag, url, err := fetchFromAPI(cfg)
	if err != nil {
		// API 失败（匿名限流 403——每 IP 每小时 60 次，共享代理出口极易耗尽——
		// 或网络不可达）时回退网页版：releases/latest 会 302 跳到最新 tag，
		// 不占 API 配额；仓库没发过版则网页同样 404，语义不变。
		tag, url, err = fetchFromPage(cfg)
		if err != nil {
			return nil, err
		}
	}

	r := &Result{
		Current: cfg.Version,
		Latest:  strings.TrimPrefix(tag, "v"),
		URL:     url,
	}
	if r.URL == "" {
		r.URL = cfg.ReleasesURL()
	}
	r.HasUpdate = compareVersions(cfg.Version, r.Latest) < 0
	return r, nil
}

// fetchFromAPI 走 api.github.com 拿最新 release 的 tag 与发布页地址。
func fetchFromAPI(cfg buildinfo.Info) (tag, url string, err error) {
	req, err := http.NewRequest(http.MethodGet, cfg.LatestReleaseAPI(), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "zcas/"+cfg.Version) // GitHub API 要求 UA

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("网络请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", "", fmt.Errorf("仓库还没有发布版本")
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GitHub 返回 %s", resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", "", fmt.Errorf("解析响应失败: %w", err)
	}
	return rel.TagName, rel.HTMLURL, nil
}

// fetchFromPage 从 releases/latest 页面的 302 跳转地址提取最新 tag。
func fetchFromPage(cfg buildinfo.Info) (tag, url string, err error) {
	client := *httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse // 不跟随跳转，直接读 Location
	}
	resp, err := client.Get(cfg.Repository + "/releases/latest")
	if err != nil {
		return "", "", fmt.Errorf("网络请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", "", fmt.Errorf("仓库还没有发布版本")
	}
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("GitHub 返回 %s", resp.Status)
	}
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if loc == "" || i < 0 {
		return "", "", fmt.Errorf("无法从跳转地址解析版本号: %s", loc)
	}
	return loc[i+len("/tag/"):], cfg.ReleasesURL(), nil
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
