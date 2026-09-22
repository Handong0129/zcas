// Package store 账号快照管理（字段级切片格式 v2）。
//
// 与整文件快照的区别：
//   - 快照只存账号相关字段：credentials 的 oauth:* / zcodejwttoken，
//     config 里 apiKey 为明文 JWT 的 provider 槽位；
//   - 切换时合并进当前文件，用户的其他 ZCode 设置（编辑器偏好、自配第三方
//     provider、MCP 等）不受影响；
//   - 切换离开时把当前账号的实时登录态回写到它自己的快照（保鲜），
//     避免 refresh token 长期不用被服务端 rotation 作废；
//   - .last 仍是整文件备份，职责是"恢复切换前的完整现场"，与保鲜互不替代。
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"zcas/internal/fingerprint"
	"zcas/internal/fsutil"
	"zcas/internal/paths"
	"zcas/internal/zcrypto"
)

const SnapshotVersion = 2

type Meta struct {
	ID           string `json:"id"`
	ShortID      string `json:"shortId"`
	EmailShortID string `json:"emailShortId"`
	UserID       string `json:"userId"`
	Provider     string `json:"provider"`
	Label        string `json:"label"`
	Email        string `json:"email,omitempty"`
	Phone        string `json:"phone,omitempty"`
	Name         string `json:"name,omitempty"`
	Avatar       string `json:"avatar,omitempty"`
	Note         string `json:"note,omitempty"`
	Source       string `json:"source,omitempty"`
	CapturedAt   int64  `json:"capturedAt"`
	UpdatedAt    int64  `json:"updatedAt,omitempty"`
}

// Snapshot 字段级账号切片。
type Snapshot struct {
	Version int `json:"version"`
	// CredentialFields credentials.json 里的账号字段（oauth:* / zcodejwttoken，enc:v1 密文）。
	CredentialFields map[string]string `json:"credentialFields"`
	// ProviderSlots config.json 里账号相关的 provider 槽位（原样 JSON）。
	ProviderSlots map[string]json.RawMessage `json:"providerSlots"`
}

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{2,64}$`)

func safeID(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("非法账号 id: %q", id)
	}
	return nil
}

func metaPath(id string) (string, error) {
	if err := safeID(id); err != nil {
		return "", err
	}
	return filepath.Join(paths.StoreDir(), id+".meta.json"), nil
}

func snapPath(id string) (string, error) {
	if err := safeID(id); err != nil {
		return "", err
	}
	return filepath.Join(paths.StoreDir(), id+".snap.json"), nil
}

// ===== 登录态读写 =====

// HasSession 判定 credentials 里是否存在有效登录会话。
// ZCode 退出登录只清 credentials.json，config.json 的 apiKey 会残留，
// 所以「当前是否登录」不能以 config 指纹为准。
func HasSession(cred map[string]any) bool {
	if v, ok := cred["zcodejwttoken"].(string); ok && v != "" {
		return true
	}
	if v, ok := cred["oauth:active_provider"].(string); ok && v != "" {
		return true
	}
	return false
}

// ReadLive 读取当前 ZCode 登录态（两份文件）。
func ReadLive() (cred, cfg map[string]any, err error) {
	cred, err = fsutil.ReadJSONFile(paths.CredentialsFile())
	if err != nil {
		return nil, nil, fmt.Errorf("读取 credentials.json 失败: %w", err)
	}
	cfg, err = fsutil.ReadJSONFile(paths.ConfigFile())
	if err != nil {
		return nil, nil, fmt.Errorf("读取 config.json 失败: %w", err)
	}
	return cred, cfg, nil
}

// WriteLive 原子写回登录态。
func WriteLive(cred, cfg map[string]any) error {
	if err := fsutil.WriteJSONAtomic(paths.CredentialsFile(), cred); err != nil {
		return err
	}
	return fsutil.WriteJSONAtomic(paths.ConfigFile(), cfg)
}

// ===== 字段级切片 =====

// IsAccountCredentialKey credentials.json 里属于账号登录态的 key。
// oauth:* 是 OAuth 凭据；account-provider:* 是客户端运行时生成的账号级 API key 缓存
// （key 里内嵌账号 id，残留会串号，客户端缺失时会重新生成）。
func IsAccountCredentialKey(k string) bool {
	return k == "zcodejwttoken" || strings.HasPrefix(k, "oauth:") || strings.HasPrefix(k, "account-provider:")
}

// IsAccountSlot 判断 config provider 槽位是否绑定账号。
// 规则：builtin: 前缀的内置槽位且带 apiKey——内置槽位的 key 属于登录账号
// （可能是明文 JWT、enc: 密文或 bigmodel API key 等格式），
// 用户自建的第三方 provider（custom:xxx 等 id）永远保留不动。
func IsAccountSlot(id string, slot map[string]any) bool {
	if !strings.HasPrefix(id, "builtin:") {
		return false
	}
	opts, _ := slot["options"].(map[string]any)
	apiKey, _ := opts["apiKey"].(string)
	return apiKey != ""
}

// Slice 从完整登录态中切出账号切片。
func Slice(cred, cfg map[string]any) *Snapshot {
	snap := &Snapshot{
		Version:          SnapshotVersion,
		CredentialFields: map[string]string{},
		ProviderSlots:    map[string]json.RawMessage{},
	}
	for k, v := range cred {
		if !IsAccountCredentialKey(k) {
			continue
		}
		if s, ok := v.(string); ok {
			snap.CredentialFields[k] = s
		}
	}
	if providers, ok := cfg["provider"].(map[string]any); ok {
		for id, pv := range providers {
			slot, _ := pv.(map[string]any)
			if slot == nil || !IsAccountSlot(id, slot) {
				continue
			}
			if raw, err := json.Marshal(slot); err == nil {
				snap.ProviderSlots[id] = raw
			}
		}
	}
	return snap
}

// Apply 把账号切片合并进当前登录态：先清除当前文件里的账号字段/槽位，
// 再写入快照内容，其余配置原样保留。
func Apply(snap *Snapshot, cred, cfg map[string]any) (map[string]any, map[string]any) {
	newCred := map[string]any{}
	for k, v := range cred {
		if !IsAccountCredentialKey(k) {
			newCred[k] = v
		}
	}
	for k, v := range snap.CredentialFields {
		newCred[k] = v
	}

	newCfg := deepCopy(cfg)
	providers, _ := newCfg["provider"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
		newCfg["provider"] = providers
	}
	// 删除当前账号槽位（避免残留上一个账号的 key）
	for id, pv := range providers {
		if slot, ok := pv.(map[string]any); ok && IsAccountSlot(id, slot) {
			delete(providers, id)
		}
	}
	for id, raw := range snap.ProviderSlots {
		var snapSlot map[string]any
		if err := json.Unmarshal(raw, &snapSlot); err != nil {
			continue
		}
		if existing, ok := providers[id].(map[string]any); ok {
			// 槽位已存在（例如同 id 的非账号槽位）：只覆盖 enabled 和 options.apiKey，
			// 保留该槽位的其他字段（baseURL 等）
			mergeSlot(existing, snapSlot)
			providers[id] = existing
		} else {
			providers[id] = snapSlot
		}
	}
	return newCred, newCfg
}

func mergeSlot(dst, src map[string]any) {
	for k, v := range src {
		if k == "options" {
			dstOpts, _ := dst["options"].(map[string]any)
			srcOpts, _ := v.(map[string]any)
			if dstOpts == nil {
				dstOpts = map[string]any{}
			}
			for ok, ov := range srcOpts {
				dstOpts[ok] = ov
			}
			dst["options"] = dstOpts
			continue
		}
		dst[k] = v
	}
}

func deepCopy(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out
}

// ===== 快照 CRUD =====

func List() ([]*Meta, error) {
	dir := paths.StoreDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var metas []*Meta
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".meta.json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m Meta
		if err := json.Unmarshal(b, &m); err == nil && m.ID != "" {
			metas = append(metas, &m)
		}
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].CapturedAt < metas[j].CapturedAt })
	return metas, nil
}

func Load(id string) (*Meta, *Snapshot, error) {
	mp, err := metaPath(id)
	if err != nil {
		return nil, nil, err
	}
	sp, _ := snapPath(id)
	var meta Meta
	b, err := os.ReadFile(mp)
	if err != nil {
		return nil, nil, fmt.Errorf("找不到账号: %s", id)
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return nil, nil, fmt.Errorf("账号元数据损坏: %w", err)
	}
	b, err = os.ReadFile(sp)
	if err != nil {
		return nil, nil, fmt.Errorf("账号快照缺失: %s", id)
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, nil, fmt.Errorf("账号快照损坏: %w", err)
	}
	return &meta, &snap, nil
}

// Save 写入账号快照 + 元数据（原子写）。
func Save(meta *Meta, snap *Snapshot) error {
	mp, err := metaPath(meta.ID)
	if err != nil {
		return err
	}
	sp, _ := snapPath(meta.ID)
	if err := fsutil.WriteJSONAtomic(sp, snap); err != nil {
		return err
	}
	return fsutil.WriteJSONAtomic(mp, meta)
}

// Exists 账号是否已存在。
func Exists(id string) bool {
	mp, err := metaPath(id)
	return err == nil && fsutil.Exists(mp)
}

// Capture 把当前 ZCode 登录态存为账号快照。返回 (meta, created, error)；
// 同账号已存在且未指定 overwrite 时 created=false。
func Capture(label, note string, overwrite bool) (*Meta, bool, error) {
	cred, cfg, err := ReadLive()
	if err != nil {
		return nil, false, err
	}
	secret := zcrypto.DefaultSecret()
	fp := fingerprint.Extract(cred, cfg, secret)
	if fp == nil {
		return nil, false, errors.New("无法从当前登录态提取账号指纹（请先在 ZCode 里登录）")
	}
	id := fp.EmailShortID
	if Exists(id) && !overwrite {
		mp, _ := metaPath(id)
		b, _ := os.ReadFile(mp)
		var old Meta
		_ = json.Unmarshal(b, &old)
		return &old, false, nil
	}
	snap := Slice(cred, cfg)
	meta := metaFromFingerprint(fp, label, note)
	meta.CapturedAt = time.Now().UnixMilli()
	if err := Save(meta, snap); err != nil {
		return nil, false, err
	}
	return meta, true, nil
}

func metaFromFingerprint(fp *fingerprint.Fingerprint, label, note string) *Meta {
	if label == "" {
		label = fp.Label
	}
	return &Meta{
		ID:           fp.EmailShortID,
		ShortID:      fp.ShortID,
		EmailShortID: fp.EmailShortID,
		UserID:       fp.UserID,
		Provider:     fp.Provider,
		Label:        label,
		Email:        fp.Email,
		Phone:        fp.Phone,
		Name:         fp.Name,
		Avatar:       fp.Avatar,
		Note:         note,
		Source:       fp.Source,
	}
}

// WriteBackCurrent 切换离开时调用：把当前登录态回写到它自己的快照（保鲜）。
//
// 防污染校验：
//   - 当前指纹必须能提取，且 user_id 与已存 meta 一致（不一致说明串号，跳过）；
//   - zcodejwttoken 必须存在且可解密；
//   - 覆盖前把旧快照备份为 <id>.snap.json.bak。
//
// 返回更新的账号 id；当前账号不在库中/校验失败时返回空串（best-effort，不报错）。
func WriteBackCurrent() (string, error) {
	cred, cfg, err := ReadLive()
	if err != nil {
		return "", nil // 没有登录态可回写
	}
	secret := zcrypto.DefaultSecret()
	fp := fingerprint.Extract(cred, cfg, secret)
	if fp == nil || fp.UserID == "" {
		return "", nil
	}
	id := fp.EmailShortID
	meta, oldSnap, err := Load(id)
	if err != nil {
		return "", nil // 当前账号不在库中
	}
	if meta.UserID != "" && fp.UserID != meta.UserID {
		return "", fmt.Errorf("当前登录态 user_id 与账号 %s 不一致，跳过回写（防止串号污染）", id)
	}
	snap := Slice(cred, cfg)
	tok, ok := snap.CredentialFields["zcodejwttoken"]
	if !ok || tok == "" {
		return "", fmt.Errorf("当前登录态缺少 zcodejwttoken，跳过回写")
	}
	if zcrypto.IsEncrypted(tok) {
		if _, err := zcrypto.Decrypt(tok, secret); err != nil {
			return "", fmt.Errorf("zcodejwttoken 解密校验失败，跳过回写: %w", err)
		}
	}
	// 备份旧快照再覆盖
	sp, _ := snapPath(id)
	if b, err := json.Marshal(oldSnap); err == nil {
		_ = fsutil.WriteFileAtomic(sp+".bak", b, 0o600)
	}
	meta.UpdatedAt = time.Now().UnixMilli()
	if err := Save(meta, snap); err != nil {
		return "", err
	}
	return id, nil
}

// ===== .last 备份（整文件，恢复现场用）=====

// BackupLast 切换前把当前两份登录态整文件备份到 .last。
func BackupLast() error {
	dir := paths.BackupDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range []string{paths.CredentialsFile(), paths.ConfigFile()} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue // 文件不存在（从未登录）时跳过，RestoreLast 会对应删除
		}
		if err := fsutil.WriteFileAtomic(filepath.Join(dir, filepath.Base(f)), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func HasLast() bool {
	dir := paths.BackupDir()
	return fsutil.Exists(filepath.Join(dir, "credentials.json")) ||
		fsutil.Exists(filepath.Join(dir, "config.json"))
}

// RestoreLast 用 .last 恢复登录态；备份里不存在的文件对应删除目标。
func RestoreLast() error {
	dir := paths.BackupDir()
	for _, f := range []string{paths.CredentialsFile(), paths.ConfigFile()} {
		bak := filepath.Join(dir, filepath.Base(f))
		b, err := os.ReadFile(bak)
		if err != nil {
			_ = os.Remove(f)
			continue
		}
		if err := fsutil.WriteFileAtomic(f, b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func Delete(id string) error {
	mp, err := metaPath(id)
	if err != nil {
		return err
	}
	sp, _ := snapPath(id)
	removed := false
	for _, f := range []string{mp, sp, sp + ".bak"} {
		if err := os.Remove(f); err == nil {
			removed = true
		}
	}
	if !removed {
		return fmt.Errorf("找不到账号: %s", id)
	}
	return nil
}

func Rename(id, label, note string) (*Meta, error) {
	meta, snap, err := Load(id)
	if err != nil {
		return nil, err
	}
	if label != "" {
		meta.Label = label
	}
	meta.Note = note
	if err := Save(meta, snap); err != nil {
		return nil, err
	}
	return meta, nil
}

// Resolve 把用户输入（序号 / 完整 id / id 前缀）解析成账号 id。
func Resolve(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", errors.New("请指定账号（序号、id 或 id 前缀）")
	}
	metas, err := List()
	if err != nil {
		return "", err
	}
	for _, m := range metas {
		if m.ID == ref {
			return m.ID, nil
		}
	}
	if n, err := strconv.Atoi(ref); err == nil && n >= 1 && n <= len(metas) {
		return metas[n-1].ID, nil
	}
	var matches []string
	for _, m := range metas {
		if strings.HasPrefix(m.ID, ref) {
			matches = append(matches, m.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("找不到账号: %s", ref)
	default:
		return "", fmt.Errorf("前缀 %q 匹配到多个账号: %s", ref, strings.Join(matches, ", "))
	}
}
