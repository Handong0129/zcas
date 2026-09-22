// Package fingerprint 从登录态中提取账号指纹（用于去重、命名、回写匹配）。
//
// 思路（与原 JS 实现一致，provider 已泛化，bigmodel/zai 均支持）：
//   - config.json 里启用中 provider 的 apiKey 是明文 JWT，payload 含 user_id，
//     是最稳定的账号唯一标识；
//   - credentials.json 的 user_info/access_token 是 enc:v1 加密，
//     解密后用于显示邮箱/头像/用户名。
package fingerprint

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"zcas/internal/zcrypto"
)

type Profile struct {
	ActiveProvider   string
	Email            string
	Phone            string
	Name             string
	Avatar           string
	CredentialUserID string
	CustomerID       string
	AccessUserID     string
	UserKey          string
}

type Fingerprint struct {
	UserID       string `json:"userId"`
	ShortID      string `json:"shortId"`
	EmailShortID string `json:"emailShortId"` // 去重 id：邮箱 hash > 手机号 hash > uid 前 8 位
	Provider     string `json:"provider"`
	Label        string `json:"label"`
	Email        string `json:"email,omitempty"`
	Phone        string `json:"phone,omitempty"`
	Name         string `json:"name,omitempty"`
	Avatar       string `json:"avatar,omitempty"`
	CustomerID   string `json:"customerId,omitempty"`
	UserKey      string `json:"userKey,omitempty"`
	Source       string `json:"source"`
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// DecodeJWT 解析 JWT payload（不验签，仅读 payload）。
func DecodeJWT(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	b, err := zcrypto.B64URLDecode(parts[1])
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// JWTUserID 从 JWT payload 取 user_id/sub，取不到返回空串。
func JWTUserID(token string) string {
	payload := DecodeJWT(token)
	if payload == nil {
		return ""
	}
	if v := str(payload["user_id"]); v != "" {
		return v
	}
	return str(payload["sub"])
}

// LooksLikeAccountJWT 判断一个 apiKey 是否为 ZCode 账号的明文 JWT。
// 用于区分账号槽位（要随切换替换）和用户自配的第三方 provider（sk-... 之类，保留）。
func LooksLikeAccountJWT(apiKey string) bool {
	if apiKey == "" || zcrypto.IsEncrypted(apiKey) || len(apiKey) < 30 {
		return false
	}
	return JWTUserID(apiKey) != ""
}

// ReadCredentialProfile 从 credentials 对象（可为实时文件或快照字段）解密账号资料。
func ReadCredentialProfile(cred map[string]any, secret string) *Profile {
	if cred == nil {
		return nil
	}
	p := &Profile{ActiveProvider: "zai"}
	if raw := str(cred["oauth:active_provider"]); raw != "" {
		if zcrypto.IsEncrypted(raw) {
			if v, err := zcrypto.Decrypt(raw, secret); err == nil && v != "" {
				p.ActiveProvider = v
			}
		} else {
			p.ActiveProvider = raw
		}
	}
	if raw := str(cred["oauth:"+p.ActiveProvider+":user_info"]); raw != "" {
		if ui := zcrypto.DecryptJSON(raw, secret); ui != nil {
			// rawProfile 嵌套对象里可能还有 email/phone 等字段，顶层优先
			rp, _ := ui["rawProfile"].(map[string]any)
			pick := func(keys ...string) string {
				for _, k := range keys {
					if v := str(ui[k]); v != "" {
						return v
					}
				}
				for _, k := range keys {
					if v := str(rp[k]); v != "" {
						return v
					}
				}
				return ""
			}
			p.Email = pick("email", "mail")
			p.Phone = pick("phone", "mobile", "phoneNumber", "phone_number")
			p.Name = pick("name", "username", "displayName", "nickName")
			p.Avatar = pick("avatar", "avatarUrl", "picture")
			p.CredentialUserID = pick("user_id", "userId", "id")
		}
	}
	if raw := str(cred["oauth:"+p.ActiveProvider+":access_token"]); raw != "" {
		plain, err := zcrypto.Decrypt(raw, secret)
		if err != nil {
			plain = raw
		}
		if payload := DecodeJWT(plain); payload != nil {
			p.CustomerID = str(payload["customer_id"])
			p.AccessUserID = firstNonEmpty(str(payload["user_id"]), str(payload["sub"]))
			p.UserKey = str(payload["user_key"])
		}
	}
	return p
}

// Extract 从 credentials + config 对象提取账号指纹。识别不了返回 nil。
func Extract(cred, cfg map[string]any, secret string) *Fingerprint {
	profile := ReadCredentialProfile(cred, secret)

	// 1. config.json 里启用中且带明文 JWT apiKey 的 provider
	type candidate struct {
		id      string
		enabled bool
		apiKey  string
	}
	var candidates []candidate
	if providers, ok := cfg["provider"].(map[string]any); ok {
		for id, pv := range providers {
			p, _ := pv.(map[string]any)
			opts, _ := p["options"].(map[string]any)
			apiKey := str(opts["apiKey"])
			if apiKey == "" || zcrypto.IsEncrypted(apiKey) || len(apiKey) < 30 {
				continue
			}
			enabled, _ := p["enabled"].(bool)
			candidates = append(candidates, candidate{id, enabled, apiKey})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].enabled && !candidates[j].enabled
	})
	for _, c := range candidates {
		uid := JWTUserID(c.apiKey)
		if uid == "" {
			continue
		}
		return build(uid, c.id, profile, "config.jwt")
	}

	// 2. 兜底：用 active_provider 密文前缀生成弱指纹（仅去重用）
	raw := str(cred["oauth:active_provider"])
	if raw == "" {
		return nil
	}
	h := Hash(raw)
	uid := h
	if profile != nil {
		uid = firstNonEmpty(profile.CredentialUserID, profile.AccessUserID, "enc-"+h)
	}
	fp := build(uid, "(encrypted)", profile, "credentials.fallback")
	fp.ShortID = h[:min(8, len(h))]
	fp.EmailShortID = identityID(fp, fp.ShortID)
	if profile == nil || (profile.Email == "" && profile.Name == "") {
		fp.Label = "账号-" + fp.ShortID
	}
	return fp
}

func build(uid, provider string, profile *Profile, source string) *Fingerprint {
	shortID := uid
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	fp := &Fingerprint{
		UserID:   uid,
		ShortID:  shortID,
		Provider: provider,
		Source:   source,
	}
	if profile != nil {
		fp.Email = profile.Email
		fp.Phone = profile.Phone
		fp.Name = profile.Name
		fp.Avatar = profile.Avatar
		fp.CustomerID = profile.CustomerID
		fp.UserKey = profile.UserKey
	}
	fp.Label = firstNonEmpty(fp.Email, MaskPhone(fp.Phone), fp.Name, "账号-"+shortID)
	fp.EmailShortID = identityID(fp, shortID)
	if fp.Email != "" || fp.Phone != "" {
		fp.Source = source + "+credentials.user_info"
	}
	return fp
}

// MaskPhone 手机号脱敏显示：138****1234。
func MaskPhone(phone string) string {
	if len(phone) >= 7 {
		return phone[:3] + "****" + phone[len(phone)-4:]
	}
	return phone
}

// identityID 去重 id：有邮箱用邮箱 hash，其次手机号 hash，否则回退 uid 前 8 位。
func identityID(fp *Fingerprint, fallback string) string {
	if fp.Email != "" {
		h := Hash(strings.ToLower(fp.Email))
		return "em-" + h[:min(10, len(h))]
	}
	if fp.Phone != "" {
		h := Hash(fp.Phone)
		return "ph-" + h[:min(10, len(h))]
	}
	return fallback
}

// Hash 与 JS 版一致的 djb2 变体（hex 输出）。
func Hash(s string) string {
	var h uint32 = 5381
	for i := 0; i < len(s); i++ {
		h = (h*33 ^ uint32(s[i]))
	}
	return strconv.FormatUint(uint64(h), 16)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
