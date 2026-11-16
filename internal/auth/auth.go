// Package auth 实现与官方 ZCode 客户端一致的 OAuth 登录流程，
// 以及换 token 后的 billing plan 初始化。
//
// 官方客户端真实配置（逆向自 ZCode 3.18.2 app.asar，服务端校验，不可随意改）：
//   - zai（全球）   : chat.z.ai/api/oauth/authorize，浏览器回调换 code
//   - zai_cn（中国）: zcode.z.ai/api/v1/oauth/cli/init 发起 + cli/poll 轮询取 token
//     （3.18.2 起新增，登录页在 zchat.z.ai，不再跳 bigmodel.cn）
//   - bigmodel（旧）: bigmodel.cn/login，仅保留给旧账号重新授权
//   - tokenUrl     : https://zcode.z.ai/api/v1/oauth/token（zai 渠道回调换 token 用）
//   - redirectUri  : https://zcode.z.ai/app/oauth/login?redirect=zcode://oauth/callback
//     （服务端中转页：既完成 flow 关联又弹回 zcode:// 协议）
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
	CliInitPath       = "/api/v1/oauth/cli/init"
	callbackURLPrefix = "zcode://"
	oauthTimeout      = 15 * time.Second

	// ClientAppVersion 对齐官方桌面客户端版本号（redirect_uri 的 app_version 参数，
	// 服务端按版本区分客户端行为，保持与官方一致最稳妥）。
	ClientAppVersion = "3.18.2"

	// callbackRedirectURI 官方 3.18.2 起的 OAuth 落地协议地址（包在中转页参数里）。
	callbackRedirectURI = "zcode://oauth/callback"
)

// 服务端地址（var 仅为测试可覆盖，运行时即官方生产地址）。
var (
	TokenURL         = "https://zcode.z.ai/api/v1/oauth/token"
	BusinessLoginURL = "https://api.z.ai/api/auth/z/login"
	ZCodeBaseURL     = "https://zcode.z.ai"
)

// DesktopRedirectURI 官方客户端 buildDesktopOAuthRedirectUri 的复刻：
// zcode.z.ai 上的中转页，redirect 参数携带最终弹回的 zcode:// 协议地址。
// zai 授权、zai_cn 轮询 flow 的 redirect_uri 都用它。
//
// ⚠ 参数顺序必须与官方 URLSearchParams 插入序一致（redirect → app_version）：
// 服务端对 redirect_uri 做精确匹配，Go url.Values.Encode() 的字母序会改变
// 参数顺序导致「授权失败，授权过程出现错误」（zchat.z.ai 实测）。手写查询串。
func DesktopRedirectURI() string {
	return ZCodeBaseURL + "/app/oauth/login?redirect=" + url.QueryEscape(callbackRedirectURI) +
		"&app_version=" + url.QueryEscape(ClientAppVersion)
}

// replaceQueryParam 在查询串中原位替换指定 key 的值，其余参数保持原顺序
// （复刻 URLSearchParams.set 的语义；key 不存在时追加到末尾）。
func replaceQueryParam(rawQuery, key, value string) string {
	pairs := strings.Split(rawQuery, "&")
	prefix := key + "="
	for i, p := range pairs {
		if p == key || strings.HasPrefix(p, prefix) {
			pairs[i] = prefix + url.QueryEscape(value)
			return strings.Join(pairs, "&")
		}
	}
	return strings.Join(append(pairs, prefix+url.QueryEscape(value)), "&")
}

// cliRedirectURI zai_cn 轮询 flow 的 redirect_uri：中转页 + provider 参数。
// provider 必须排在 redirect、app_version 之后（官方 searchParams.set 插入序，
// 服务端精确匹配 redirect_uri，顺序错会导致「授权失败」）。
func cliRedirectURI(p Provider) string {
	redirect := DesktopRedirectURI()
	if p.ID == ProviderZaiCN.ID {
		redirect += "&provider=" + url.QueryEscape(p.ID)
	}
	return redirect
}

// Provider 登录渠道配置（逆向自 ZCode 3.18.2 客户端 app.asar 的 OAuth provider）。
// 全球用户走 zai；中国用户 3.18.2 起走 zai_cn（zchat 轮询式），
// bigmodel 为旧版中国渠道，仅保留给存量账号重新授权。
type Provider struct {
	ID           string // oauth:<id>:* 凭据前缀
	DisplayName  string
	AuthorizeURL string
	AppID        string
	RedirectURI  string
	Slots        []string // 该渠道对应的 config provider 槽位
	// Polling 该渠道是否走「服务端 init + 轮询」流程（zai_cn）。
	Polling bool
}

var (
	ProviderZai = Provider{
		ID:           "zai",
		DisplayName:  "z.ai（全球）",
		AuthorizeURL: "https://chat.z.ai/api/oauth/authorize",
		AppID:        "client_P8X5CMWmlaRO9gyO-KSqtg",
		RedirectURI:  DesktopRedirectURI(),
		Slots:        []string{"builtin:zai-start-plan", "builtin:zai-coding-plan", "builtin:zai"},
	}
	ProviderZaiCN = Provider{
		ID:           "zai_cn",
		DisplayName:  "Z.ai（中国）",
		AuthorizeURL: ZCodeBaseURL + CliInitPath, // 仅标记用途；实际授权页由 init 返回
		AppID:        "client_P8X5CMWmlaRO9gyO-KSqtg",
		RedirectURI:  DesktopRedirectURI(),
		Slots:        []string{"builtin:bigmodel-coding-plan", "builtin:bigmodel-start-plan"},
		Polling:      true,
	}
	// ProviderBigModel 旧版中国渠道（3.18.2 前的登录页），存量账号重新授权仍可用。
	ProviderBigModel = Provider{
		ID:           "bigmodel",
		DisplayName:  "BigModel（旧中国渠道）",
		AuthorizeURL: "https://bigmodel.cn/login",
		AppID:        "zcode",
		RedirectURI:  "zcode://oauth/callback",
		Slots:        []string{"builtin:bigmodel-coding-plan", "builtin:bigmodel-start-plan"},
	}
)

// ParseProvider 解析用户输入的渠道（zai/全球、zai_cn/中国 或 bigmodel/旧中国）。
func ParseProvider(s string) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "zai", "global", "全球":
		return ProviderZai, nil
	case "zai_cn", "zaicn", "zchat", "cn", "china", "中国":
		return ProviderZaiCN, nil
	case "bigmodel":
		return ProviderBigModel, nil
	}
	return Provider{}, fmt.Errorf("未知渠道 %q（可选 zai / zai_cn / bigmodel）", s)
}

var httpClient = &http.Client{Timeout: oauthTimeout}

// LinkedBigModel zai_cn 登录时服务端返回的 BigModel 关联凭据
// （oauth:linked_bigmodel，官方 client 存 {userId, customerId, accessToken}）。
type LinkedBigModel struct {
	UserID      string `json:"userId"`
	CustomerID  string `json:"customerId"`
	AccessToken string `json:"accessToken"`
}

type TokenSet struct {
	Token          string          // zcode JWT（调 API 用，含 billing 权限）
	ZaiAccessToken string          // zai oauth access_token（zai 渠道为 business token）
	RefreshToken   string          // refresh_token（zai_cn 渠道没有）
	User           map[string]any  // 用户信息（email/name/avatar...）
	LinkedBigModel *LinkedBigModel // zai_cn 渠道的 BigModel 关联凭据
}

// NewState 生成 OAuth state（对齐官方客户端：32 字节随机数的 hex）。
func NewState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func BuildAuthorizeURL(p Provider, state string) string {
	if p.Polling {
		// zai_cn 的授权页由服务端 init 返回（含服务端生成的 state），见 StartCliFlow
		return p.AuthorizeURL
	}
	if p.ID == ProviderBigModel.ID {
		// bigmodel 登录页的参数形式与 z.ai 不同（逆向自官方客户端）
		q := url.Values{}
		q.Set("redirect", p.RedirectURI)
		q.Set("appId", p.AppID)
		q.Set("state", state)
		return p.AuthorizeURL + "?" + q.Encode()
	}
	// zai：官方 3.18.2 的 buildAuthorizeUrl，参数保持官方插入序
	// （redirect_uri → response_type → client_id → state），理由同 DesktopRedirectURI
	return p.AuthorizeURL + "?" + strings.Join([]string{
		"redirect_uri=" + url.QueryEscape(p.RedirectURI),
		"response_type=code",
		"client_id=" + url.QueryEscape(p.AppID),
		"state=" + url.QueryEscape(state),
	}, "&")
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
// zai/bigmodel 渠道 POST 同一端点，响应结构相同（data.<provider>.access_token）。
// zai 渠道与官方一致：把 zai access_token 经 z/login 换成 business token 再入库。
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
		Code int            `json:"code"`
		Msg  string         `json:"msg"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("token 响应非 JSON（HTTP %d）: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if result.Code != 0 {
		return nil, fmt.Errorf("token 交换失败: %s", firstNonEmpty(result.Msg, fmt.Sprintf("HTTP %d", resp.StatusCode)))
	}
	d := result.Data
	token, _ := d["token"].(string)
	providerBlock, _ := d["zai"].(map[string]any)
	accessToken, _ := providerBlock["access_token"].(string)
	refreshToken, _ := providerBlock["refresh_token"].(string)
	if token == "" || accessToken == "" {
		return nil, fmt.Errorf("token 响应缺少 data.token / data.zai.access_token")
	}
	user, _ := d["user"].(map[string]any)
	if userID, _ := user["user_id"].(string); userID == "" {
		return nil, errors.New("token 响应缺少 data.user.user_id")
	}
	ts := &TokenSet{
		Token:          token,
		RefreshToken:   refreshToken,
		User:           user,
		LinkedBigModel: parseLinkedBigModel(d["bigmodel"], user),
	}
	// zai 渠道与官方一致：oauth:zai:access_token 存 business token（z/login 换发）。
	// 换发失败退回原始 oauth token（客户端此处会硬失败，工具侧宽松处理便于排查）。
	ts.ZaiAccessToken = accessToken
	if p.ID == ProviderZai.ID {
		if biz, err := ResolveBusinessToken(accessToken); err == nil && biz != "" {
			ts.ZaiAccessToken = biz
		}
	}
	return ts, nil
}

// ResolveBusinessToken 用 zai oauth token 换 business token
// （官方 ZaiBusinessTokenResolver：POST z/login {token} → data.access_token）。
func ResolveBusinessToken(oauthToken string) (string, error) {
	if oauthToken == "" {
		return "", errors.New("缺少 zai oauth token")
	}
	body, _ := json.Marshal(map[string]string{"token": oauthToken})
	resp, err := httpClient.Post(BusinessLoginURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("business token 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var result struct {
		Code    *int           `json:"code"`
		Success *bool          `json:"success"`
		Msg     string         `json:"msg"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("business token 响应非 JSON（HTTP %d）: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	ok := (result.Code != nil && (*result.Code == 0 || *result.Code == 200)) ||
		(result.Success != nil && *result.Success)
	if !ok {
		return "", fmt.Errorf("business token 换发失败: %s", firstNonEmpty(result.Msg, fmt.Sprintf("HTTP %d", resp.StatusCode)))
	}
	token, _ := result.Data["access_token"].(string)
	if token == "" {
		token, _ = result.Data["accessToken"].(string)
	}
	if token == "" {
		return "", errors.New("business token 响应缺少 data.access_token")
	}
	return token, nil
}

// parseLinkedBigModel 解析响应里的 bigmodel 关联块（readLinkedBigModel 复刻）。
// 缺失返回 nil；结构不完整也返回 nil（工具侧宽松，不阻断登录）。
func parseLinkedBigModel(v any, user map[string]any) *LinkedBigModel {
	block, _ := v.(map[string]any)
	if block == nil {
		return nil
	}
	customerID, _ := block["customer_id"].(string)
	accessToken, _ := block["access_token"].(string)
	if customerID == "" || accessToken == "" {
		return nil
	}
	userID, _ := user["user_id"].(string)
	return &LinkedBigModel{UserID: userID, CustomerID: customerID, AccessToken: accessToken}
}

// ===== zai_cn 轮询式登录（ZCode 3.18.2 新流程） =====
//
// 官方 startOAuthWithPolling 复刻：
//  1. 本地生成 32 字节随机 pollToken；
//  2. POST zcode.z.ai/api/v1/oauth/cli/init（Bearer pollToken，body {"provider":"zai_cn"}）
//     → {flow_id, authorize_url, expires_at, poll_interval_sec}；
//  3. authorize_url 的 redirect_uri 参数替换为 zcode.z.ai 中转页（附 provider=zai_cn），
//     浏览器打开后用户在 zchat.z.ai 完成登录，中转页把 flow 标记完成；
//  4. 轮询 GET cli/poll/<flow_id>（Bearer pollToken）直到 status=ready，
//     token 集合直接在响应里（无授权码、无 refresh token）。

// CliFlow 一次 zai_cn 轮询登录会话。
type CliFlow struct {
	Provider     string // 发起渠道（"zai_cn"）
	FlowID       string
	State        string // 服务端在 authorize_url 里生成的 state
	AuthorizeURL string // 最终给浏览器打开的授权页（已替换 redirect_uri）
	PollURL      string
	PollToken    string
	ExpiresAt    time.Time
	Interval     time.Duration // 服务端建议的轮询间隔
}

// FlowStatus 轮询状态。
const (
	FlowPending = "pending"
	FlowReady   = "ready"
	FlowFailed  = "failed"
)

// StartCliFlow 发起 zai_cn 轮询登录（步骤 1-3）。
func StartCliFlow(p Provider) (*CliFlow, error) {
	if !p.Polling {
		return nil, fmt.Errorf("渠道 %s 不支持轮询登录", p.ID)
	}
	pollToken, err := NewState() // 同规格：32 字节 hex
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{"provider": p.ID})
	req, err := http.NewRequest(http.MethodPost, ZCodeBaseURL+CliInitPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+pollToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OAuth flow 初始化请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			FlowID          string `json:"flow_id"`
			AuthorizeURL    string `json:"authorize_url"`
			ExpiresAt       int64  `json:"expires_at"`
			PollIntervalSec int64  `json:"poll_interval_sec"`
			ServerTime      int64  `json:"server_time"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("OAuth flow 初始化响应非 JSON（HTTP %d）: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if result.Code != 0 || result.Data.FlowID == "" || result.Data.AuthorizeURL == "" {
		return nil, fmt.Errorf("OAuth flow 初始化失败: %s", firstNonEmpty(result.Msg, fmt.Sprintf("HTTP %d", resp.StatusCode)))
	}
	u, err := url.Parse(result.Data.AuthorizeURL)
	if err != nil || u.Scheme != "https" {
		return nil, fmt.Errorf("OAuth flow 返回的授权页无效: %s", truncate(result.Data.AuthorizeURL, 120))
	}
	// 与官方一致：redirect_uri 替换为中转页，zai_cn 在 wrapper 末尾追加 provider 参数
	u.RawQuery = replaceQueryParam(u.RawQuery, "redirect_uri", cliRedirectURI(p))

	interval := time.Duration(result.Data.PollIntervalSec) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &CliFlow{
		Provider:     p.ID,
		FlowID:       result.Data.FlowID,
		State:        u.Query().Get("state"),
		AuthorizeURL: u.String(),
		PollURL:      ZCodeBaseURL + "/api/v1/oauth/cli/poll/" + url.PathEscape(result.Data.FlowID),
		PollToken:    pollToken,
		ExpiresAt:    time.Unix(result.Data.ExpiresAt, 0),
		Interval:     interval,
	}, nil
}

// Poll 查询一次 flow 状态。status=ready 时返回 token 集合。
// 服务端 4xx（除 408/429）按官方语义视为 flow 失败返回错误。
func (f *CliFlow) Poll() (status string, ts *TokenSet, err error) {
	if time.Now().After(f.ExpiresAt) {
		return FlowFailed, nil, errors.New("OAuth flow 已过期，请重新发起登录")
	}
	req, err := http.NewRequest(http.MethodGet, f.PollURL, nil)
	if err != nil {
		return FlowPending, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.PollToken)
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return FlowPending, nil, fmt.Errorf("OAuth flow 查询失败: %w", err) // 网络抖动，下轮重试
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 && resp.StatusCode != 429 {
		return FlowFailed, nil, fmt.Errorf("OAuth flow 已失效（HTTP %d）: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var result struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Status string         `json:"status"`
			Token  string         `json:"token"`
			User   map[string]any `json:"user"`
			Zai    struct {
				AccessToken string `json:"access_token"`
			} `json:"zai"`
			BigModel map[string]any `json:"bigmodel"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Code != 0 {
		return FlowPending, nil, fmt.Errorf("OAuth flow 查询响应异常: %s", truncate(string(raw), 120))
	}
	switch result.Data.Status {
	case FlowPending:
		return FlowPending, nil, nil
	case FlowFailed:
		return FlowFailed, nil, errors.New("浏览器里未完成授权或授权被拒绝，请重新发起登录")
	case FlowReady:
	default:
		return FlowPending, nil, fmt.Errorf("OAuth flow 查询响应无效: %s", truncate(string(raw), 120))
	}
	userID, _ := result.Data.User["user_id"].(string)
	if result.Data.Token == "" || result.Data.Zai.AccessToken == "" || userID == "" {
		return FlowFailed, nil, fmt.Errorf("OAuth flow 响应缺少 token / zai.access_token / user.user_id: %s", truncate(string(raw), 160))
	}
	return FlowReady, &TokenSet{
		Token:          result.Data.Token,
		ZaiAccessToken: result.Data.Zai.AccessToken, // zai_cn 不经 z/login，直接存原始 token
		User:           result.Data.User,
		LinkedBigModel: parseLinkedBigModel(result.Data.BigModel, result.Data.User),
	}, nil
}

// AddAccount 用 tokenSet 直接构造账号切片入库（不触碰当前登录态）。
// 返回 (meta, created, error)；同账号已存在时 created=false。
//
// billing plan 初始化（服务端异步激活）不阻塞入库：快照保存成功即返回，
// 初始化在后台协程补做——服务端本来就要异步处理，快轮询没就绪也正常，
// 后续额度查询时 quota 层自带退避重试，无需让调用方干等 10-20 秒。
func AddAccount(p Provider, ts *TokenSet, label, note string, cfg *config.Config) (meta *store.Meta, created bool, err error) {
	if ts == nil || ts.Token == "" {
		return nil, false, errors.New("缺少 token（zcode JWT）")
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
		return nil, false, errors.New("无法从 token 中提取账号指纹")
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
		return meta, false, nil
	}
	snap := &store.Snapshot{
		Version:          store.SnapshotVersion,
		CredentialFields: fields,
		ProviderSlots:    slots,
	}
	if err = store.Save(meta, snap); err != nil {
		return nil, false, err
	}

	// 服务端 billing 初始化后台补做（不阻塞调用方）：
	// zai_cn（轮询渠道）不经 z/login，直接等 billing 就绪。
	go func() {
		if p.Polling {
			_ = waitBillingReady(ts.Token, cfg)
			return
		}
		_ = TriggerBusinessLogin(ts.ZaiAccessToken, ts.Token, cfg)
	}()
	return meta, true, nil
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
	go func() {
		if p.Polling {
			_ = waitBillingReady(ts.Token, cfg)
			return
		}
		_ = TriggerBusinessLogin(ts.ZaiAccessToken, ts.Token, cfg)
	}()
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
// zai_cn 渠道额外写 oauth:linked_bigmodel（BigModel 关联凭据，官方 commitSession 同款）。
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
	if ts.LinkedBigModel != nil {
		lb, _ := json.Marshal(ts.LinkedBigModel)
		if err := set("oauth:linked_bigmodel", string(lb)); err != nil {
			return nil, err
		}
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
// 客户端 parseStoredUserProfile 先按 {id, username, displayName} 解析，
// zai/zai_cn 渠道再回退解析裸 user 对象（saveUserProfile 存 rawProfile 本体）；
// ensureProfileKeys 补齐规范字段后两种解析路径都能命中。
func officialUserProfile(user map[string]any, providerID string) map[string]any {
	if providerID == ProviderZai.ID || providerID == ProviderZaiCN.ID {
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
