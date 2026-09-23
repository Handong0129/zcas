// Package quota 查询 ZCode 账号额度（billing API，含 429 退避 + 401 兜底重试）。
package quota

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"zcas/internal/config"
	"zcas/internal/paths"
	"zcas/internal/platform"
	"zcas/internal/store"
	"zcas/internal/zcrypto"
)

// BillingBalanceURL 官方客户端实际使用的额度接口（新版已合并 plans+balances 到这一个端点；
// billing/current 已被服务端 WAF 拦截）。逆向自 macOS ZCode 3.14.0 app.asar。
const BillingBalanceURL = "https://zcode.z.ai/api/v1/zcode-plan/billing/balance"

var httpClient = &http.Client{Timeout: 15 * time.Second}

type PlanTier struct {
	Label string `json:"label"`
	Tier  string `json:"tier"`
}

type Item struct {
	Name        string   `json:"name"`
	Total       *float64 `json:"total"`
	Used        *float64 `json:"used"`
	Remaining   *float64 `json:"remaining"`
	PercentUsed *float64 `json:"percentUsed"`
	Unit        string   `json:"unit"`
	PeriodEnd   any      `json:"periodEnd,omitempty"`
}

type Overview struct {
	Total       *float64  `json:"total"`
	Used        *float64  `json:"used"`
	Remaining   *float64  `json:"remaining"`
	PercentUsed *float64  `json:"percentUsed"`
	IsEmpty     bool      `json:"isEmpty"`
	PlanTier    *PlanTier        `json:"planTier,omitempty"`
	Items       []Item           `json:"items"`
	CodingPlan  *CodingPlanUsage `json:"codingPlan,omitempty"`
	RefreshedAt int64     `json:"refreshedAt"`
}

func buildBillingURL(base string, cfg *config.Config) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	q.Set("app_version", appVersion(cfg))
	u.RawQuery = q.Encode()
	return u.String()
}

// appVersion 与本机安装的 ZCode 客户端版本保持一致，读不到时用配置兜底。
func appVersion(cfg *config.Config) string {
	if v := platform.ZCodeVersion(); v != "" {
		return v
	}
	return cfg.BillingAppVersion
}

// zcodeHeaders 复刻官方客户端 buildZCodeSourceHeaders 的请求头。
// 缺少这些头服务端会返回 400 parameter error（实测确认）。
func zcodeHeaders(cfg *config.Config) http.Header {
	ver := appVersion(cfg)
	plat := cfg.BillingPlatform
	if plat == "" {
		plat = runtime.GOOS + "-" + runtime.GOARCH
	}
	h := http.Header{}
	h.Set("User-Agent", "ZCode/"+ver)
	h.Set("HTTP-Referer", "https://zcode.z.ai")
	h.Set("X-Title", "Z Code@electron")
	h.Set("X-ZCode-App-Version", ver)
	h.Set("X-Platform", plat)
	h.Set("X-Client-Language", "zh-CN")
	h.Set("X-Client-Timezone", "Asia/Shanghai")
	if runtime.GOOS == "darwin" {
		h.Set("X-Os-Category", "macos")
	}
	if v := platform.OSVersion(); v != "" {
		h.Set("X-Os-Version", v)
	}
	if mid := deviceMid(); mid != "" {
		h.Set("X-Device-Mid", mid)
	}
	return h
}

// deviceMid 与官方客户端共用 ~/.zcode/v2/telemetry-state.json 里的设备标识。
func deviceMid() string {
	b, err := os.ReadFile(filepath.Join(paths.ZcodeV2Dir(), "telemetry-state.json"))
	if err != nil {
		return ""
	}
	var s struct {
		DeviceMid string `json:"deviceMid"`
	}
	if json.Unmarshal(b, &s) == nil {
		return s.DeviceMid
	}
	return ""
}

func fetchBilling(rawURL, token string, cfg *config.Config) (any, error) {
	retryDelays := []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond, 4 * time.Second}
	var lastErr error
	for attempt := 0; attempt <= len(retryDelays); attempt++ {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header = zcodeHeaders(cfg)
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var data any
			if err := json.Unmarshal(body, &data); err != nil {
				return nil, fmt.Errorf("额度接口返回非 JSON: %w", err)
			}
			return data, nil
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < len(retryDelays) {
			lastErr = errors.New("服务端限流，正在重试")
			time.Sleep(retryDelays[attempt])
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("Token 已过期或无效（HTTP %d）", resp.StatusCode)
		}
		return nil, fmt.Errorf("额度接口 HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	return nil, lastErr
}

// HasAnyPlan 检查是否已有 plan（billing 初始化就绪探测）。
func HasAnyPlan(zcodeJWT string, cfg *config.Config) bool {
	if zcodeJWT == "" {
		return false
	}
	data, err := fetchBilling(buildBillingURL(BillingBalanceURL, cfg), zcodeJWT, cfg)
	if err != nil {
		return false
	}
	cur := unwrap(data)
	plans, _ := cur["plans"].([]any)
	return len(plans) > 0
}

// RawJSON 用候选 token 依次请求 billing 接口，返回首个成功响应的原始 JSON（排查用）。
func RawJSON(tokens []string, cfg *config.Config) ([]byte, error) {
	if len(tokens) == 0 {
		return nil, errors.New("未找到可用于查询额度的 token")
	}
	var lastErr error
	for _, token := range tokens {
		data, err := fetchBilling(buildBillingURL(BillingBalanceURL, cfg), token, cfg)
		if err == nil {
			return json.MarshalIndent(data, "", "  ")
		}
		lastErr = err
	}
	return nil, lastErr
}

// ForTokens 依次尝试候选 token 查询额度；全部 401/403 时延迟 1.5s 用首个 token 兜底重试
// （服务端首次激活有缓存延迟，避免误报过期）。
func ForTokens(tokens []string, cfg *config.Config) (*Overview, error) {
	if len(tokens) == 0 {
		return nil, errors.New("未找到可用于查询额度的 token")
	}
	var lastErr error
	authFails := 0
	for _, token := range tokens {
		ov, err := queryOne(token, cfg)
		if err == nil {
			return ov, nil
		}
		lastErr = err
		if strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "HTTP 403") {
			authFails++
		}
	}
	if authFails > 0 && authFails == len(tokens) {
		time.Sleep(1500 * time.Millisecond)
		if ov, err := queryOne(tokens[0], cfg); err == nil {
			return ov, nil
		}
		return nil, errors.New("该账号 Token 已过期，请删除后重新登录")
	}
	return nil, lastErr
}

func queryOne(token string, cfg *config.Config) (*Overview, error) {
	// 新版端点一次返回 plans + balances
	data, err := fetchBilling(buildBillingURL(BillingBalanceURL, cfg), token, cfg)
	if err != nil {
		return nil, err
	}
	ov := normalize(data)
	ov.RefreshedAt = time.Now().UnixMilli()
	return ov, nil
}

// ===== token 候选 =====

// CurrentTokens 从当前登录态读取候选 token（zcodejwttoken 优先，
// oauth access_token 是 chat.z.ai 的，查 billing 会 401，仅作兜底）。
func CurrentTokens(secret string) ([]string, error) {
	cred, cfg, err := store.ReadLive()
	if err != nil {
		return nil, err
	}
	return candidateTokens(cred, cfg, secret), nil
}

// CredMapFromSnapshot 把快照凭据字段转成 map[string]any（EnrichCodingPlan 入参）。
func CredMapFromSnapshot(snap *store.Snapshot) map[string]any {
	cred := map[string]any{}
	for k, v := range snap.CredentialFields {
		cred[k] = v
	}
	return cred
}

// TokensFromSnapshot 从账号切片读取候选 token。
func TokensFromSnapshot(snap *store.Snapshot, secret string) []string {
	cred := CredMapFromSnapshot(snap)
	providers := map[string]any{}
	for id, raw := range snap.ProviderSlots {
		var slot map[string]any
		if err := json.Unmarshal(raw, &slot); err == nil {
			providers[id] = slot
		}
	}
	return candidateTokens(cred, map[string]any{"provider": providers}, secret)
}

func candidateTokens(cred, cfg map[string]any, secret string) []string {
	activeProvider := "zai"
	if raw, ok := cred["oauth:active_provider"].(string); ok && raw != "" {
		if v := safeDecrypt(raw, secret); v != "" {
			activeProvider = v
		}
	}
	var tokens []string
	add := func(v string) {
		plain := safeDecrypt(v, secret)
		if len(plain) > 20 && !contains(tokens, plain) {
			tokens = append(tokens, plain)
		}
	}
	strv := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	add(strv(cred, "zcodejwttoken"))
	add(strv(cred, "oauth:"+activeProvider+":access_token"))
	add(strv(cred, "oauth:zai:access_token"))
	add(strv(cred, "oauth:bigmodel:access_token"))
	if providers, ok := cfg["provider"].(map[string]any); ok {
		// 稳定顺序，避免 map 遍历随机性
		ids := make([]string, 0, len(providers))
		for id := range providers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			slot, _ := providers[id].(map[string]any)
			opts, _ := slot["options"].(map[string]any)
			if apiKey, ok := opts["apiKey"].(string); ok {
				add(apiKey)
			}
		}
	}
	return tokens
}

func safeDecrypt(v, secret string) string {
	if v == "" {
		return ""
	}
	if !zcrypto.IsEncrypted(v) {
		return v
	}
	plain, err := zcrypto.Decrypt(v, secret)
	if err != nil {
		return ""
	}
	return plain
}

// ===== 响应归一化（复刻 JS 版 normalizeQuota 逻辑）=====

func normalize(data any) *Overview {
	unwrapped := unwrap(data)
	pool := map[string]float64{}
	flattenNumbers(unwrapped, "", pool)

	total := sumNumbers(pool, "total_units")
	if total == nil {
		total = firstNumber(pool, "total", "totalQuota", "totalCredits", "quotaTotal", "amountTotal", "creditTotal")
	}
	used := sumNumbers(pool, "used_units")
	if used == nil {
		used = firstNumber(pool, "used", "usedQuota", "usedCredits", "quotaUsed", "amountUsed", "consumed", "totalUsed")
	}
	remaining := sumNumbers(pool, "remaining_units")
	if remaining == nil {
		remaining = firstNumber(pool, "remaining", "remain", "balance", "available", "availableQuota", "left", "quotaRemaining")
	}
	if total == nil && used != nil && remaining != nil {
		t := *used + *remaining
		total = &t
	}
	if used == nil && total != nil && remaining != nil {
		u := math.Max(0, *total-*remaining)
		used = &u
	}
	if remaining == nil && total != nil && used != nil {
		r := math.Max(0, *total-*used)
		remaining = &r
	}

	ov := &Overview{Total: total, Used: used, Remaining: remaining, PlanTier: extractPlanTier(unwrapped)}
	if total != nil && *total > 0 && used != nil {
		p := clamp(*used / *total * 100, 0, 100)
		ov.PercentUsed = &p
	}
	plans, plansOk := unwrapped["plans"].([]any)
	balances, balOk := unwrapped["balances"].([]any)
	ov.IsEmpty = plansOk && balOk && len(plans) == 0 && len(balances) == 0
	ov.Items = normalizeItems(unwrapped)
	return ov
}

func unwrap(data any) map[string]any {
	cur := data
	for i := 0; i < 4; i++ {
		m, ok := cur.(map[string]any)
		if !ok {
			return map[string]any{}
		}
		if inner, ok := m["data"]; ok {
			cur = inner
			continue
		}
		if inner, ok := m["result"]; ok {
			cur = inner
			continue
		}
		return m
	}
	if m, ok := cur.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func flattenNumbers(obj map[string]any, prefix string, out map[string]float64) {
	for k, v := range obj {
		flattenValue(v, joinPath(prefix, k), out)
	}
}

// flattenValue 递归展开 map 和数组（数组下标作为路径段，与 JS 版 Object.entries 行为一致）。
func flattenValue(v any, path string, out map[string]float64) {
	if n, ok := toNumber(v); ok {
		out[path] = n
		return
	}
	switch child := v.(type) {
	case map[string]any:
		for k, v2 := range child {
			flattenValue(v2, joinPath(path, k), out)
		}
	case []any:
		for i, v2 := range child {
			flattenValue(v2, joinPath(path, strconv.Itoa(i)), out)
		}
	}
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func leafName(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}

func firstNumber(pool map[string]float64, keys ...string) *float64 {
	// 稳定顺序
	paths := make([]string, 0, len(pool))
	for p := range pool {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		for _, k := range keys {
			if leafName(p) == k {
				v := pool[p]
				return &v
			}
		}
	}
	return nil
}

func sumNumbers(pool map[string]float64, keys ...string) *float64 {
	total := 0.0
	count := 0
	for p, v := range pool {
		for _, k := range keys {
			if leafName(p) == k {
				total += v
				count++
			}
		}
	}
	if count == 0 {
		return nil
	}
	return &total
}

// extractPlanTier 从 plans 数组提取付费等级（复刻 ZCode 客户端逻辑，Max 优先避免模糊匹配）。
func extractPlanTier(current map[string]any) *PlanTier {
	plans, _ := current["plans"].([]any)
	match := func(p any, kw string) bool {
		m, _ := p.(map[string]any)
		id := strings.ToLower(strv(m["plan_id"]))
		name := strings.ToLower(strv(m["name"]))
		return strings.Contains(id, kw) || strings.Contains(name, kw)
	}
	active := func(p any) bool {
		m, _ := p.(map[string]any)
		return strings.EqualFold(strv(m["status"]), "active")
	}
	for _, rule := range []struct {
		keywords []string
		tier     PlanTier
	}{
		{[]string{"max"}, PlanTier{"Max", "max"}},
		{[]string{"pro"}, PlanTier{"Pro", "pro"}},
		{[]string{"lite"}, PlanTier{"Lite", "lite"}},
		{[]string{"start-plan", "start plan"}, PlanTier{"Start Plan", "start"}},
	} {
		for _, p := range plans {
			if !active(p) {
				continue
			}
			for _, kw := range rule.keywords {
				if match(p, kw) {
					t := rule.tier
					return &t
				}
			}
		}
	}
	return nil
}

func normalizeItems(data map[string]any) []Item {
	balances, _ := data["balances"].([]any)
	items := make([]Item, 0, len(balances))
	for _, b := range balances {
		m, _ := b.(map[string]any)
		if m == nil {
			continue
		}
		total, hasT := toNumber(m["total_units"])
		used, hasU := toNumber(m["used_units"])
		remaining, hasR := toNumber(m["remaining_units"])
		if !hasR {
			remaining, hasR = toNumber(m["available_units"])
		}
		item := Item{
			Name: firstNonEmpty(strv(m["show_name"]), strv(m["name"]), strv(m["entitlement_id"]), strv(m["plan_id"]), "未知模型"),
			Unit: firstNonEmpty(strv(m["unit_type"]), strv(m["meter"]), "quota"),
		}
		if hasT {
			item.Total = &total
		}
		if hasU {
			item.Used = &used
		}
		if hasR {
			item.Remaining = &remaining
		}
		if hasT && total > 0 && hasU {
			p := clamp(used/total*100, 0, 100)
			item.PercentUsed = &p
		}
		if pe := firstNonEmpty(strv(m["period_end"]), strv(m["expires_at"])); pe != "" {
			item.PeriodEnd = pe
		}
		items = append(items, item)
	}
	return items
}

func toNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case string:
		if strings.TrimSpace(n) == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(strings.ReplaceAll(n, ",", ""), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func clamp(v, min, max float64) float64 { return math.Min(max, math.Max(min, v)) }

func strv(v any) string { s, _ := v.(string); return s }

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// ===== Coding Plan 用量（bigmodel / z.ai 自有 coding-plan 计费，独立于 zcode-plan）=====

// CodingPlanUsagePath GLM Coding Plan 用量查询路径（bigmodel.cn 与 chat.z.ai 同路径）。
const CodingPlanUsagePath = "/api/monitor/usage/quota/limit"

type CodingPlanLimit struct {
	Name      string  `json:"name"`      // 每5小时 / 每周
	Limit     float64 `json:"limit"`     // 限额总量（接口字段 usage，命名有误导性）
	Used      float64 `json:"used"`      // 已用（接口字段 currentValue）
	Remaining float64 `json:"remaining"`
	Percent   int     `json:"percent"`   // 已用百分比
	NextReset int64   `json:"nextReset,omitempty"`
}

type CodingPlanUsage struct {
	Provider    string            `json:"provider"`              // bigmodel / zai
	Level       string            `json:"level"`                 // lite / pro / max
	ProductName string            `json:"productName,omitempty"` // 如 GLM Coding Lite（来自订阅接口）
	ValidPeriod string            `json:"validPeriod,omitempty"`
	Limits      []CodingPlanLimit `json:"limits"`
}

func codingPlanHost(provider string) string {
	if provider == "zai" {
		return "https://chat.z.ai"
	}
	return "https://bigmodel.cn"
}

// FetchCodingPlanUsage 查询 coding-plan 用量。
// 鉴权与官方客户端一致：Authorization 头直接放裸 token（createBigModelLoginAuthHeaders）。
// account-provider api-key 与 oauth access_token 均可（实测确认）。
func FetchCodingPlanUsage(provider, token string) (*CodingPlanUsage, error) {
	if token == "" {
		return nil, errors.New("缺少 coding-plan token")
	}
	req, err := http.NewRequest(http.MethodGet, codingPlanHost(provider)+CodingPlanUsagePath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coding-plan 用量接口 HTTP %d", resp.StatusCode)
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Level  string `json:"level"`
			Limits []struct {
				Unit         int     `json:"unit"`
				Number       int     `json:"number"`
				Usage        float64 `json:"usage"`
				CurrentValue float64 `json:"currentValue"`
				Remaining    float64 `json:"remaining"`
				Percentage   int     `json:"percentage"`
				NextReset    int64   `json:"nextResetTime"`
			} `json:"limits"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Code != 200 {
		return nil, fmt.Errorf("coding-plan 用量接口返回异常: %s", truncate(string(body), 120))
	}
	if env.Data.Level == "" && len(env.Data.Limits) == 0 {
		return nil, errors.New("该账号无 coding-plan 用量数据")
	}
	u := &CodingPlanUsage{Provider: provider, Level: env.Data.Level}
	for _, l := range env.Data.Limits {
		u.Limits = append(u.Limits, CodingPlanLimit{
			Name:      limitWindowName(l.Unit, l.Number),
			Limit:     l.Usage,
			Used:      l.CurrentValue,
			Remaining: l.Remaining,
			Percent:   l.Percentage,
			NextReset: l.NextReset,
		})
	}
	return u, nil
}

// limitWindowName 限额周期文案（实测：unit 3=小时、6=周；未知值按原始编号兜底）。
func limitWindowName(unit, number int) string {
	name := map[int]string{3: "小时", 6: "周"}[unit]
	if name == "" {
		return fmt.Sprintf("每%d(u%d)", number, unit)
	}
	if number == 1 {
		return "每" + name
	}
	return fmt.Sprintf("每%d%s", number, name)
}

// FetchBigModelSubscription 查询 BigModel 侧 coding-plan 订阅（产品名/有效期）。
func FetchBigModelSubscription(accessToken string) (productName, validPeriod string, err error) {
	if accessToken == "" {
		return "", "", errors.New("缺少 bigmodel access token")
	}
	req, err := http.NewRequest(http.MethodGet, "https://bigmodel.cn/api/biz/subscription/list", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var env struct {
		Code int `json:"code"`
		Data []struct {
			ProductName string `json:"productName"`
			Status      string `json:"status"`
			Valid       string `json:"valid"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Code != 200 {
		return "", "", fmt.Errorf("订阅查询失败: %s", truncate(string(body), 120))
	}
	for _, s := range env.Data {
		if strings.EqualFold(s.Status, "VALID") && strings.Contains(strings.ToLower(s.ProductName), "coding") {
			return s.ProductName, s.Valid, nil
		}
	}
	return "", "", errors.New("无有效 coding-plan 订阅")
}

// EnrichCodingPlan 若凭据含 coding-plan 相关 token（bigmodel / zai），
// 查询用量并挂到 Overview.CodingPlan；任何失败都静默（附加信息，不影响主流程）。
func EnrichCodingPlan(ov *Overview, cred map[string]any, secret string) {
	if ov == nil || len(cred) == 0 {
		return
	}
	decrypt := func(k string) string {
		s, _ := cred[k].(string)
		return safeDecrypt(s, secret)
	}
	// accountID 本账号在该渠道的 user id（user_info.id）。
	// coding-plan api-key 内嵌账号 id，但客户端切换账号后不清残留，
	// 必须按此过滤，否则无订阅账号会显示成别人的订阅（实测复现）。
	accountID := func(provider string) string {
		var ui struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(decrypt("oauth:"+provider+":user_info")), &ui) == nil {
			return ui.ID
		}
		return ""
	}
	// 1) 客户端物化的 coding-plan api-key（account-provider:* 凭据）
	// 排序遍历保证确定性（map 随机序会导致同账号多次刷新展示不同账号的数据）。
	keys := make([]string, 0, len(cred))
	for k := range cred {
		if strings.HasPrefix(k, "account-provider:coding-plan:") && strings.HasSuffix(k, ":api-key") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		provider := "bigmodel"
		if strings.Contains(k, ":zai-") {
			provider = "zai"
		}
		// key 形如 …:account:<渠道slug>:account:<账号id>:api-key；
		// 内嵌账号 id 存在时必须与本账号匹配（本账号 id 未知也跳过，不冒险展示）。
		if embedded := embeddedAccountID(k); embedded != "" && accountID(provider) != embedded {
			continue
		}
		s, _ := cred[k].(string)
		if u, err := FetchCodingPlanUsage(provider, safeDecrypt(s, secret)); err == nil {
			ov.CodingPlan = u
			enrichSubscription(u, decrypt("oauth:bigmodel:access_token"))
			return
		}
	}
	// 2) oauth access_token 兜底（实测 bigmodel token 可直接查用量）
	for _, p := range []string{"bigmodel", "zai"} {
		token := decrypt("oauth:" + p + ":access_token")
		if token == "" {
			continue
		}
		if u, err := FetchCodingPlanUsage(p, token); err == nil {
			ov.CodingPlan = u
			if p == "bigmodel" {
				enrichSubscription(u, token)
			}
			return
		}
	}
}

// embeddedAccountID 提取 coding-plan api-key 里内嵌的账号 id
// （account-provider:coding-plan:account:<渠道slug>:account:<账号id>:api-key），没有则返回空。
func embeddedAccountID(key string) string {
	const sep = ":account:"
	i := strings.LastIndex(key, sep)
	if i < 0 {
		return ""
	}
	return strings.TrimSuffix(key[i+len(sep):], ":api-key")
}

// enrichSubscription bigmodel 侧补充订阅产品名与有效期（可选，失败静默）。
func enrichSubscription(u *CodingPlanUsage, accessToken string) {
	if u.Provider != "bigmodel" {
		return
	}
	name, period, err := FetchBigModelSubscription(accessToken)
	if err == nil {
		u.ProductName = name
		u.ValidPeriod = period
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
