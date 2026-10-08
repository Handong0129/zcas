// Package session 账号登录会话（oauth access_token）健康探测与保活。
//
// 背景（逆向官方客户端 + 实测确认）：
//   - zcode JWT（zcodejwttoken）与 account-provider api-key 是长效凭证，
//     会话过期后编码/推理仍可用；失效的只有 oauth access_token（服务端会话表控制，
//     JWT 本体无 exp 声明），表现为 ZCode 客户端「套餐查询失败」。
//   - 服务端会话过期时 bigmodel/chat.z.ai 的用量接口返回 HTTP 200 + 空 body
//     （不是 401！），伪造 token 才返回 code:401；这是过期判定的关键特征。
//   - 官方客户端没有任何刷新/保活实现，唯一恢复手段是重新 OAuth 授权。
//
// 保活原理：对库内所有账号（含未激活的）周期性发起一次带鉴权的只读请求。
// 若服务端按「闲置」过期，探测即可续命；若按绝对时长过期，探测无害。
package session

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"zcas/internal/fsutil"
	"zcas/internal/paths"
	"zcas/internal/store"
	"zcas/internal/zcrypto"
)

// Status 会话健康状态。
type Status string

const (
	StatusAlive   Status = "alive"   // 会话有效
	StatusExpired Status = "expired" // 会话已过期（需重新授权）
	StatusUnknown Status = "unknown" // 无法判定（无 oauth token / 网络失败）
)

// Report 单账号的最近一次探测结果。
type Report struct {
	ID        string `json:"id"`
	Status    Status `json:"status"`
	CheckedAt int64  `json:"checkedAt"` // 毫秒
}

var httpClient = &http.Client{Timeout: 12 * time.Second}

// usagePath 用量探测端点（bigmodel.cn 与 chat.z.ai 同路径，复刻官方客户端）。
const usagePath = "/api/monitor/usage/quota/limit"

func host(provider string) string {
	if provider == "zai" {
		return "https://chat.z.ai"
	}
	return "https://bigmodel.cn"
}

// CheckSnapshot 探测一个账号快照的 oauth 会话状态。
// 无 oauth access_token（老快照/仅抓取）或请求失败时返回 unknown，不误报过期。
func CheckSnapshot(id string, snap *store.Snapshot, secret string) Report {
	rep := Report{ID: id, Status: StatusUnknown, CheckedAt: time.Now().UnixMilli()}
	provider := "zai"
	if raw, ok := snap.CredentialFields["oauth:active_provider"]; ok && raw != "" {
		if v := plain(raw, secret); v != "" {
			provider = v
		}
	}
	tok := plain(snap.CredentialFields["oauth:"+provider+":access_token"], secret)
	if tok == "" {
		return rep
	}
	req, err := http.NewRequest(http.MethodGet, host(provider)+usagePath, nil)
	if err != nil {
		return rep
	}
	// 与官方客户端一致：Authorization 裸 token，不带 Bearer 前缀
	req.Header.Set("Authorization", tok)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return rep
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return rep // 网关/WAF 异常等，不判定
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		// 实测特征：会话过期时 200 + 空 body
		rep.Status = StatusExpired
		return rep
	}
	var env struct {
		Code int `json:"code"`
	}
	if json.Unmarshal(body, &env) != nil {
		return rep
	}
	switch {
	case env.Code == 200:
		rep.Status = StatusAlive
	case env.Code == 401 || env.Code == 403:
		rep.Status = StatusExpired
	default:
		rep.Status = StatusUnknown
	}
	return rep
}

func plain(v, secret string) string {
	if v == "" {
		return ""
	}
	if !zcrypto.IsEncrypted(v) {
		return v
	}
	out, err := zcrypto.Decrypt(v, secret)
	if err != nil {
		return ""
	}
	return out
}

// ===== 健康状态持久化（~/.zcas/health.json）=====

// LoadAll 读取全部账号的最近探测结果（文件缺失/损坏返回空 map）。
func LoadAll() map[string]Report {
	out := map[string]Report{}
	b, err := os.ReadFile(paths.HealthFile())
	if err != nil {
		return out
	}
	_ = json.Unmarshal(b, &out)
	return out
}

// SaveAll 原子写回健康状态。
func SaveAll(all map[string]Report) error {
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(paths.HealthFile(), b, 0o600)
}

// Forget 删除账号时清理其健康记录。
func Forget(id string) {
	all := LoadAll()
	if _, ok := all[id]; !ok {
		return
	}
	delete(all, id)
	_ = SaveAll(all)
}

// Sweep 探测所有账号（并发），返回最新全量报告。
// maxAge>0 时只探测距上次探测超过该时长的账号，其余沿用缓存（保活巡检用）。
func Sweep(maxAge time.Duration) map[string]Report {
	secret := zcrypto.DefaultSecret()
	all := LoadAll()
	metas, err := store.List()
	if err != nil {
		return all
	}
	type job struct {
		id   string
		snap *store.Snapshot
	}
	var jobs []job
	for _, m := range metas {
		if maxAge > 0 {
			if rep, ok := all[m.ID]; ok && time.Since(time.UnixMilli(rep.CheckedAt)) < maxAge {
				continue
			}
		}
		if _, snap, err := store.Load(m.ID); err == nil {
			jobs = append(jobs, job{m.ID, snap})
		}
	}
	if len(jobs) == 0 {
		return all
	}
	results := make(chan Report, len(jobs))
	for _, j := range jobs {
		go func(id string, snap *store.Snapshot) {
			results <- CheckSnapshot(id, snap, secret)
		}(j.id, j.snap)
	}
	for range jobs {
		rep := <-results
		all[rep.ID] = rep
	}
	_ = SaveAll(all)
	return all
}

// FormatStatus 状态的中文文案（提示信息用）。
func FormatStatus(s Status) string {
	switch s {
	case StatusAlive:
		return "会话正常"
	case StatusExpired:
		return "会话过期"
	default:
		return "未知"
	}
}
