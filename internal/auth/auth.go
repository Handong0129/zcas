// Package auth 实现与官方 ZCode 客户端一致的 Authorization Code OAuth 流程，
// 以及换 token 后的 billing plan 初始化。
//
// 官方客户端真实配置（提取自 app.asar，服务端校验，不可随意改）：
//   - authorizeUrl : https://chat.z.ai/api/oauth/authorize
//   - tokenUrl     : https://zcode.z.ai/api/v1/oauth/token
//   - redirectUri  : zcode://zai-auth/callback（自定义协议回调）
//   - appId        : client_P8X5CMWmlaRO9gyO-KSqtg
//
// 与原 JS 实现的关键差异：AddAccount 直接用 tokenSet 构造账号切片入库，
// 不再"写入 v2 → capture → 恢复"，添加账号全程不触碰当前登录态。
package auth

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"zcas/internal/config"
	"zcas/internal/fingerprint"
	"zcas/internal/fsutil"
	"zcas/internal/quota"
	"zcas/internal/store"
	"zcas/internal/zcrypto"
)

const (
	TokenURL           = "https://zcode.z.ai/api/v1/oauth/token"
	BusinessLoginURL   = "https://api.z.ai/api/auth/z/login"
	oauthTimeout       = 15 * time.Second
	callbackURLPrefix  = "zcode://"
)

// Provider 登录渠道配置（逆向自 ZCode 客户端 app.asar 的两个 OAuth provider）。
// 全球用户走 z.ai，中国用户走 BigModel，两者 token 端点相同，其余配置不同。
type Provider struct {
	ID           string // oauth:<id>:* 凭据前缀
	DisplayName  string
	AuthorizeURL string
	AppID        string
	RedirectURI  string
	Slots        []string // 该渠道对应的 config provider 槽位
}

var (
	ProviderZai = Provider{
		ID:           "zai",
		DisplayName:  "z.ai（全球）",
		AuthorizeURL: "https://chat.z.ai/api/oauth/authorize",
		AppID:        "client_P8X5CMWmlaRO9gyO-KSqtg",
		RedirectURI:  "zcode://zai-auth/callback",
		Slots:        []string{"builtin:zai-start-plan", "builtin:zai-coding-plan", "builtin:zai"},
	}
	ProviderBigModel = Provider{
		ID:           "bigmodel",
		DisplayName:  "BigModel（中国）",
		AuthorizeURL: "https://bigmodel.cn/login",
		AppID:        "zcode",
		RedirectURI:  "zcode://oauth/callback",
		Slots:        []string{"builtin:bigmodel-coding-plan", "builtin:bigmodel-start-plan"},
	}
)

// ParseProvider 解析用户输入的渠道（zai/全球/global 或 bigmodel/中国/cn）。
func ParseProvider(s string) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "zai", "global", "全球":
		return ProviderZai, nil
	case "bigmodel", "cn", "china", "中国":
		return ProviderBigModel, nil
	}
	return Provider{}, fmt.Errorf("未知渠道 %q（可选 zai / bigmodel）", s)
}

var httpClient = &http.Client{Timeout: oauthTimeout}

type TokenSet struct {
	Token          string         // zcode JWT（调 API 用，含 billing 权限）
	ZaiAccessToken string         // zai oauth access_token
	RefreshToken   string         // zai refresh_token
	User           map[string]any // 用户信息（email/name/avatar...）
}

func NewState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func BuildAuthorizeURL(p Provider, state string) string {
	q := url.Values{}
	if p.ID == ProviderBigModel.ID {
		// bigmodel 登录页的参数形式与 z.ai 不同（逆向自官方客户端）
		q.Set("redirect", p.RedirectURI)
		q.Set("appId", p.AppID)
		q.Set("state", state)
	} else {
		q.Set("response_type", "code")
		q.Set("client_id", p.AppID)
		q.Set("redirect_uri", p.RedirectURI)
		q.Set("state", state)
	}
	return p.AuthorizeURL + "?" + q.Encode()
}

// ParseCallbackInput 解析用户粘贴的内容：完整 zcode:// 回调 URL 或裸 code。
func ParseCallbackInput(input string) (code, state string, err error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", errors.New("输入为空")
	}
	if strings.HasPrefix(input, callbackURLPrefix) || strings.HasPrefix(input, "http") {
		u, err := url.Parse(input)
		if err != nil {
			return "", "", fmt.Errorf("无法解析回调链接: %w", err)
		}
		// 官方客户端 parseCallbackParams: authCode ?? code（bigmodel 渠道用 authCode）
		code = u.Query().Get("code")
		if code == "" {
			code = u.Query().Get("authCode")
		}
		state = u.Query().Get("state")
		if code == "" {
			return "", "", errors.New("回调链接里没有 code/authCode 参数")
		}
		return code, state, nil
	}
	// 裸 code（也容忍 "code=xxx&state=yyy" 形式）
	if strings.Contains(input, "code=") {
		if vals, err := url.ParseQuery(input); err == nil {
			c := vals.Get("code")
			if c == "" {
				c = vals.Get("authCode")
			}
			return c, vals.Get("state"), nil
		}
	}
	return input, "", nil
}

// ExchangeCode 用授权码换 token 集合（逆向自官方客户端 exchangeToken）。
// 两渠道 POST 同一端点，响应结构相同（data.<provider>.access_token）。
func ExchangeCode(p Provider, code, state string) (*TokenSet, error) {
	body, _ := json.Marshal(map[string]string{
		"provider":     p.ID,
		"code":         code,
		"redirect_uri": p.RedirectURI,
		"state":        state,
	})
	resp, err := httpClient.Post(TokenURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("token 交换请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var result struct {
		Code int               `json:"code"`
		Msg  string            `json:"msg"`
		Data map[string]any    `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("token 响应非 JSON（HTTP %d）: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("token 交换失败: %s", firstNonEmpty(result.Msg, fmt.Sprintf("HTTP %d", resp.StatusCode)))
	}
	d := result.Data
	token, _ := d["token"].(string)
	providerBlock, _ := d[p.ID].(map[string]any)
	accessToken, _ := providerBlock["access_token"].(string)
	refreshToken, _ := providerBlock["refresh_token"].(string)
	if token == "" || accessToken == "" {
		return nil, fmt.Errorf("token 响应缺少 data.token / data.%s.access_token", p.ID)
	}
	user, _ := d["user"].(map[string]any)
	return &TokenSet{
		Token:          token,
		ZaiAccessToken: accessToken,
		RefreshToken:   refreshToken,
		User:           user,
	}, nil
}

// AddAccount 用 tokenSet 直接构造账号切片入库（不触碰当前登录态）。
// 返回 (meta, created, billingReady, error)；同账号已存在时 created=false。
func AddAccount(p Provider, ts *TokenSet, label, note string, cfg *config.Config) (meta *store.Meta, created bool, billingReady bool, err error) {
	if ts == nil || ts.Token == "" {
		return nil, false, false, errors.New("缺少 token（zcode JWT）")
	}
	secret := zcrypto.DefaultSecret()

	// 构造 credentials 账号字段
	fields, err := credentialFields(p, ts, secret)
	if err != nil {
		return
	}

	// 构造 config provider 槽位：克隆本机已有 builtin 槽位的完整结构
	// （models/kind/baseURL 等），只替换 apiKey——否则客户端没有可用模型
	slots := buildProviderSlots(p, ts.Token)

	// 复用指纹逻辑生成与 capture 一致的账号 id
	credMap := map[string]any{}
	for k, v := range fields {
		credMap[k] = v
	}
	providerMap := map[string]any{}
	for id, raw := range slots {
		var slot map[string]any
		_ = json.Unmarshal(raw, &slot)
		providerMap[id] = slot
	}
	fp := fingerprint.Extract(credMap, map[string]any{"provider": providerMap}, secret)
	if fp == nil {
		return nil, false, false, errors.New("无法从 token 中提取账号指纹")
	}

	meta = &store.Meta{
		ID:           fp.EmailShortID,
		ShortID:      fp.ShortID,
		EmailShortID: fp.EmailShortID,
		UserID:       fp.UserID,
		Provider:     fp.Provider,
		Label:        firstNonEmpty(label, fp.Label),
		Email:        fp.Email,
		Phone:        fp.Phone,
		Name:         fp.Name,
		Avatar:       fp.Avatar,
		Note:         note,
		Source:       "oauth",
		CapturedAt:   time.Now().UnixMilli(),
	}
	if store.Exists(meta.ID) {
		return meta, false, false, nil
	}
	snap := &store.Snapshot{
		Version:          store.SnapshotVersion,
		CredentialFields: fields,
		ProviderSlots:    slots,
	}
	if err = store.Save(meta, snap); err != nil {
		return nil, false, false, err
	}

	// 触发服务端 billing plan 初始化（逆向自 ZaiBusinessTokenResolver；
	// 服务端异步处理，快速轮询没就绪也正常，首次查询时 quota 层会自动退避重试）
	billingReady = TriggerBusinessLogin(ts.ZaiAccessToken, ts.Token, cfg)
	return meta, true, billingReady, nil
}

// UpdateAccount 重新授权：用新 tokenSet 覆盖更新已有账号的快照（重新授权入口）。
// 名称/备注/抓取时间等元数据保留，只刷新登录态字段；身份不一致时拒绝（防串号）。
func UpdateAccount(p Provider, ts *TokenSet, id string, cfg *config.Config) (*store.Meta, error) {
	if ts == nil || ts.Token == "" {
		return nil, errors.New("缺少 token（zcode JWT）")
	}
	meta, oldSnap, err := store.Load(id)
	if err != nil {
		return nil, fmt.Errorf("目标账号不存在: %w", err)
	}
	secret := zcrypto.DefaultSecret()

	fields, err := credentialFields(p, ts, secret)
	if err != nil {
		return nil, err
	}

	// 槽位优先沿用该账号旧快照的结构（models/baseURL 等与首次登录时一致），
	// 只替换 apiKey；旧快照没有的槽位再用 live config 模板补
	fresh := buildProviderSlots(p, ts.Token)
	slots := map[string]json.RawMessage{}
	for _, slotID := range p.Slots {
		var slot map[string]any
		if raw, ok := oldSnap.ProviderSlots[slotID]; ok && len(raw) > 0 {
			if json.Unmarshal(raw, &slot) == nil && slot != nil {
				opts, _ := slot["options"].(map[string]any)
				if opts == nil {
					opts = map[string]any{}
				}
				opts["apiKey"] = ts.Token
				slot["enabled"] = true
				slot["options"] = opts
			}
		}
		if slot == nil {
			// buildProviderSlots 已写好 apiKey，反序列化复用
			var m map[string]any
			if raw, ok := fresh[slotID]; ok {
				_ = json.Unmarshal(raw, &m)
			}
			slot = m
		}
		if slot == nil {
			continue
		}
		raw, _ := json.Marshal(slot)
		slots[slotID] = raw
	}

	// 指纹与身份校验：新授权必须是同一个账号（按去重 id 或稳定 user_id 匹配）
	credMap := map[string]any{}
	for k, v := range fields {
		credMap[k] = v
	}
	providerMap := map[string]any{}
	for slotID, raw := range slots {
		var slot map[string]any
		_ = json.Unmarshal(raw, &slot)
		providerMap[slotID] = slot
	}
	fp := fingerprint.Extract(credMap, map[string]any{"provider": providerMap}, secret)
	if fp == nil {
		return nil, errors.New("无法从 token 中提取账号指纹")
	}
	if fp.EmailShortID != id && (meta.UserID == "" || fp.UserID != meta.UserID) {
		return nil, fmt.Errorf("授权的是账号「%s」，与目标账号「%s」不一致，请确认浏览器里登录的是目标账号", fp.Label, meta.Label)
	}

	// 备份旧快照再覆盖（与保鲜回写一致）
	sp, err := store.SnapPath(id)
	if err != nil {
		return nil, err
	}
	if b, err := json.Marshal(oldSnap); err == nil {
		_ = fsutil.WriteFileAtomic(sp+".bak", b, 0o600)
	}
	meta.UpdatedAt = time.Now().UnixMilli()
	snap := &store.Snapshot{
		Version:          store.SnapshotVersion,
		CredentialFields: fields,
		ProviderSlots:    slots,
	}
	if err := store.Save(meta, snap); err != nil {
		return nil, err
	}

	// 服务端 billing 初始化对已有账号无需阻塞等待，后台补一次即可
	go func() { _ = TriggerBusinessLogin(ts.ZaiAccessToken, ts.Token, cfg) }()
	return meta, nil
}

// ProviderFromSnapshot 从快照凭据推断登录渠道（重新授权时自动选渠道用）。
func ProviderFromSnapshot(snap *store.Snapshot, secret string) Provider {
	p, _ := ParseProvider("zai")
	if snap == nil {
		return p
	}
	raw := snap.CredentialFields["oauth:active_provider"]
	if raw == "" {
		return p
	}
	if !zcrypto.IsEncrypted(raw) {
		if v, err := ParseProvider(raw); err == nil {
			return v
		}
		return p
	}
	if plain, err := zcrypto.Decrypt(raw, secret); err == nil {
		if v, err := ParseProvider(plain); err == nil {
			return v
		}
	}
	return p
}

// credentialFields 构造 credentials.json 的账号字段（enc:v1 密文）。
func credentialFields(p Provider, ts *TokenSet, secret string) (map[string]string, error) {
	enc := func(s string) (string, error) {
		if s == "" {
			return "", nil
		}
		return zcrypto.Encrypt(s, secret)
	}
	fields := map[string]string{}
	set := func(k, plain string) error {
		v, err := enc(plain)
		if err != nil {
			return err
		}
		if v != "" {
			fields[k] = v
		}
		return nil
	}
	if err := set("oauth:active_provider", p.ID); err != nil {
		return nil, err
	}
	if err := set("oauth:"+p.ID+":access_token", ts.ZaiAccessToken); err != nil {
		return nil, err
	}
	if err := set("oauth:"+p.ID+":refresh_token", ts.RefreshToken); err != nil {
		return nil, err
	}
	if err := set("zcodejwttoken", ts.Token); err != nil {
		return nil, err
	}
	userInfo := officialUserProfile(ts.User, p.ID)
	uiJSON, _ := json.Marshal(userInfo)
	if err := set("oauth:"+p.ID+":user_info", string(uiJSON)); err != nil {
		return nil, err
	}
	return fields, nil
}

// TriggerBusinessLogin POST api.z.ai/api/auth/z/login 触发服务端初始化 billing plan，
// 然后轮询 billing/current 确认 plans 就绪。总耗时约 20s 内。
func TriggerBusinessLogin(zaiAccessToken, zcodeJWT string, cfg *config.Config) bool {
	if quota.HasAnyPlan(zcodeJWT, cfg) {
		return true
	}
	if zaiAccessToken != "" {
		for _, delay := range []time.Duration{0, 3 * time.Second, 10 * time.Second} {
			if delay > 0 {
				time.Sleep(delay)
			}
			body, _ := json.Marshal(map[string]string{"token": zaiAccessToken})
			resp, err := httpClient.Post(BusinessLoginURL, "application/json", bytes.NewReader(body))
			if err != nil {
				continue
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var r struct {
				Code    *int  `json:"code"`
				Success *bool `json:"success"`
			}
			if json.Unmarshal(raw, &r) == nil {
				ok := (r.Code != nil && (*r.Code == 0 || *r.Code == 200)) || (r.Success != nil && *r.Success)
				if ok && waitBillingReady(zcodeJWT, cfg) {
					return true
				}
			}
		}
	}
	return waitBillingReady(zcodeJWT, cfg)
}

func waitBillingReady(zcodeJWT string, cfg *config.Config) bool {
	for _, delay := range []time.Duration{time.Second, 3 * time.Second, 6 * time.Second} {
		time.Sleep(delay)
		if quota.HasAnyPlan(zcodeJWT, cfg) {
			return true
		}
	}
	return false
}

// buildProviderSlots 为新账号构造 zai 系 provider 槽位。
// 官方客户端的槽位包含 models/kind/baseURL 等完整结构，缺了会报「当前没有可用模型」。
// 策略：优先克隆 live config 里的同 id 槽位，其次克隆任意带 models 的 builtin 槽位
// （zai/bigmodel 的模型与 endpoint 同构），都没有才退回最小槽位。
func buildProviderSlots(p Provider, token string) map[string]json.RawMessage {
	slots := map[string]json.RawMessage{}
	var liveProviders map[string]any
	if _, cfg, err := store.ReadLive(); err == nil {
		liveProviders, _ = cfg["provider"].(map[string]any)
	}

	sortedIDs := func() []string {
		ids := make([]string, 0, len(liveProviders))
		for id := range liveProviders {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return ids
	}
	pickTemplate := func(sameFamily bool) map[string]any {
		for _, id := range sortedIDs() {
			if !strings.HasPrefix(id, "builtin:") {
				continue
			}
			if sameFamily && !strings.Contains(id, p.ID) {
				continue
			}
			slot, _ := liveProviders[id].(map[string]any)
			if slot == nil {
				continue
			}
			if _, ok := slot["models"].(map[string]any); ok {
				return slot
			}
		}
		return nil
	}
	template := pickTemplate(true)
	if template == nil {
		template = pickTemplate(false)
	}

	clone := func(src map[string]any) map[string]any {
		b, _ := json.Marshal(src)
		out := map[string]any{}
		_ = json.Unmarshal(b, &out)
		return out
	}

	for _, id := range p.Slots {
		var slot map[string]any
		if ls, ok := liveProviders[id].(map[string]any); ok && ls != nil {
			slot = clone(ls)
		} else if template != nil {
			slot = clone(template)
		} else {
			slot = map[string]any{}
		}
		slot["enabled"] = true
		opts, _ := slot["options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		opts["apiKey"] = token
		slot["options"] = opts
		raw, _ := json.Marshal(slot)
		slots[id] = raw
	}
	return slots
}

// officialUserProfile 生成与官方客户端一致的 user_info 结构。
// 客户端 restoreCachedSession 校验 {id, username, displayName, avatarUrl?, rawProfile?}，
// 且 zai 渠道保存的是 rawProfile 本体（saveUserProfile 逆向结论）。
func officialUserProfile(user map[string]any, providerID string) map[string]any {
	if providerID == ProviderZai.ID {
		if rp, ok := user["rawProfile"].(map[string]any); ok && rp != nil {
			return ensureProfileKeys(rp, user)
		}
	}
	return ensureProfileKeys(user, user)
}

// ensureProfileKeys 补齐客户端校验要求的 id/username/displayName 字段。
func ensureProfileKeys(profile, fallback map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range profile {
		out[k] = v
	}
	pick := func(keys ...string) string {
		for _, src := range []map[string]any{profile, fallback} {
			for _, k := range keys {
				if s, ok := src[k].(string); ok && s != "" {
					return s
				}
			}
		}
		return ""
	}
	if out["id"] == nil {
		out["id"] = pick("id", "user_id", "userId", "sub", "customerNumber")
	}
	if out["username"] == nil {
		out["username"] = pick("username", "name", "nickName", "displayName", "email")
	}
	if out["displayName"] == nil {
		out["displayName"] = pick("displayName", "name", "username", "nickName", "email")
	}
	if out["avatarUrl"] == nil {
		if v := pick("avatarUrl", "avatar", "avatar_url", "picture"); v != "" {
			out["avatarUrl"] = v
		}
	}
	return out
}

func normalizeUserInfo(user map[string]any) map[string]any {
	rp, _ := user["rawProfile"].(map[string]any)
	g := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := user[k].(string); ok && s != "" {
				return s
			}
		}
		for _, k := range keys {
			if s, ok := rp[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
	return map[string]any{
		"email":   g("email", "mail"),
		"phone":   g("phone", "mobile", "phoneNumber", "phone_number"),
		"name":    g("name", "username", "nickName", "displayName"),
		"avatar":  g("avatar", "avatarUrl", "picture"),
		"user_id": g("user_id", "userId", "id", "customerNumber", "sub"),
	}
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
