package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"zcas/internal/auth"
	"zcas/internal/buildinfo"
	"zcas/internal/config"
	"zcas/internal/fingerprint"
	"zcas/internal/platform"
	"zcas/internal/quota"
	"zcas/internal/session"
	"zcas/internal/store"
	"zcas/internal/switcher"
	"zcas/internal/update"
	"zcas/internal/zcrypto"
)

// zcodeAppBundle 官方 ZCode 客户端的 .app 路径（协议归还目标）。
const zcodeAppBundle = "/Applications/ZCode.app"

// keepAliveInterval 保活巡检周期：距上次探测超过该时长的账号会被重新探测
// （探测本身即保活尝试；服务端按闲置过期时可自动续命）。
const keepAliveInterval = 20 * time.Hour

// App 是 Wails 绑定层：只做参数转换 + 调 internal 包，不含业务逻辑。
type App struct {
	ctx context.Context

	mu    sync.Mutex
	oauth *oauthPending // 进行中的 OAuth 会话
}

type oauthPending struct {
	state    string
	provider auth.Provider
	name     string
	note     string
	reauthID string // 非空表示这是对已有账号的重新授权
}

func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go a.runHealthKeeper()
}

// ===== 状态 =====

type CurrentView struct {
	Label    string `json:"label"`
	Provider string `json:"provider"`
	ShortID  string `json:"shortId"`
	Email    string `json:"email,omitempty"`
	Saved    bool   `json:"saved"`
	SavedID  string `json:"savedId,omitempty"`
}

type StateView struct {
	Running       bool          `json:"running"`
	Current       *CurrentView  `json:"current"`
	Accounts      []*store.Meta `json:"accounts"`
	HasLastBackup bool          `json:"hasLastBackup"`
}

func (a *App) GetState() *StateView {
	view := &StateView{Running: platform.IsZCodeRunning(), HasLastBackup: store.HasLast()}
	if cred, cfg, err := store.ReadLive(); err == nil && hasSession(cred) {
		if fp := fingerprint.Extract(cred, cfg, zcrypto.DefaultSecret()); fp != nil {
			saved := store.Exists(fp.EmailShortID)
			view.Current = &CurrentView{
				Label: fp.Label, Provider: fp.Provider, ShortID: fp.ShortID,
				Email: fp.Email, Saved: saved,
			}
			if saved {
				view.Current.SavedID = fp.EmailShortID
			}
		}
	}
	metas, _ := store.List()
	view.Accounts = metas
	return view
}

// hasSession 判定是否存在有效登录会话（转调 store.HasSession）。
func hasSession(cred map[string]any) bool { return store.HasSession(cred) }

// ===== capture =====

type CaptureResult struct {
	Created bool        `json:"created"`
	Meta    *store.Meta `json:"meta"`
}

func (a *App) Capture(name, note string, overwrite bool) (*CaptureResult, error) {
	meta, created, err := store.Capture(name, note, overwrite)
	if err != nil {
		return nil, err
	}
	return &CaptureResult{Created: created, Meta: meta}, nil
}

// ===== OAuth 添加账号 =====
// 主路径：发起时临时把 zcode:// 协议 handler 切换为本工具（浏览器回调直接回本应用，
// 自动完成登录）；完成/取消后归还给官方 ZCode 客户端。
// 兑底路径（协议接管不可用或回调被抢）：粘贴回调链接，或从 ZCode 抓取当前登录。

// StartOAuth 打开浏览器发起登录。providerID: zai(全球) / bigmodel(中国)。
func (a *App) StartOAuth(providerID, name, note string) error {
	p, err := auth.ParseProvider(providerID)
	if err != nil {
		return err
	}
	state, err := auth.NewState()
	if err != nil {
		return err
	}
	a.mu.Lock()
	if a.oauth != nil {
		a.mu.Unlock()
		return fmt.Errorf("已有进行中的登录流程，请先完成或取消")
	}
	a.oauth = &oauthPending{state: state, provider: p, name: name, note: note}
	a.mu.Unlock()
	// 临时接管 zcode://（仅在打包成 .app 运行时有效）
	if bp := selfBundlePath(); bp != "" {
		setDefaultURLHandler("zcode", bp)
	}
	return platform.OpenURL(auth.BuildAuthorizeURL(p, state))
}

// restoreScheme 把 zcode:// 协议 handler 归还给官方 ZCode 客户端。
func (a *App) restoreScheme() {
	if _, err := os.Stat(zcodeAppBundle); err == nil {
		setDefaultURLHandler("zcode", zcodeAppBundle)
	}
}

// handleOpenURL 处理 macOS open-url 事件（浏览器 zcode:// 回调送达本应用）。
func (a *App) handleOpenURL(raw string) {
	a.mu.Lock()
	p := a.oauth
	a.mu.Unlock()
	if p == nil {
		return // 没有进行中的登录会话，忽略（可能是官方客户端的回调被误投递）
	}
	code, cbState, err := auth.ParseCallbackInput(raw)
	if err != nil {
		wruntime.EventsEmit(a.ctx, "oauth:error", "回调链接解析失败: "+err.Error())
		return
	}
	if cbState != "" && cbState != p.state {
		return // 非本次会话的回调，静默丢弃
	}
	if p.reauthID != "" {
		a.finishReOAuth(code, p)
		return
	}
	a.finishOAuth(code, p)
}

// finishOAuth 换 token + 入库，通过事件通知前端。
func (a *App) finishOAuth(code string, p *oauthPending) {
	ts, err := auth.ExchangeCode(p.provider, code, p.state)
	if err != nil {
		wruntime.EventsEmit(a.ctx, "oauth:error", err.Error())
		return
	}
	meta, created, billingReady, err := auth.AddAccount(p.provider, ts, p.name, p.note, config.Load())
	if err != nil {
		wruntime.EventsEmit(a.ctx, "oauth:error", err.Error())
		return
	}
	a.mu.Lock()
	a.oauth = nil
	a.mu.Unlock()
	a.restoreScheme()
	wruntime.WindowShow(a.ctx)
	wruntime.EventsEmit(a.ctx, "oauth:done", map[string]any{
		"created":      created,
		"billingReady": billingReady,
		"label":        meta.Label,
		"id":           meta.ID,
	})
}

type OAuthResult struct {
	Created      bool        `json:"created"`
	BillingReady bool        `json:"billingReady"`
	Meta         *store.Meta `json:"meta"`
}

// FinishOAuth 兑底路径：用户手动粘贴回调链接完成登录。
func (a *App) FinishOAuth(input, name, note string) (*OAuthResult, error) {
	a.mu.Lock()
	p := a.oauth
	a.mu.Unlock()
	if p == nil {
		return nil, fmt.Errorf("请先点击「打开浏览器登录」")
	}
	if p.reauthID != "" {
		return nil, fmt.Errorf("当前有正在进行的重新授权，请在账号行内完成或取消后再添加账号")
	}
	code, cbState, err := auth.ParseCallbackInput(input)
	if err != nil {
		return nil, err
	}
	if cbState != "" && cbState != p.state {
		return nil, fmt.Errorf("回调 state 不匹配（可能粘贴了旧的链接），请重新发起登录")
	}
	ts, err := auth.ExchangeCode(p.provider, code, p.state)
	if err != nil {
		return nil, err
	}
	meta, created, billingReady, err := auth.AddAccount(p.provider, ts, name, note, config.Load())
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.oauth = nil
	a.mu.Unlock()
	a.restoreScheme()
	return &OAuthResult{Created: created, BillingReady: billingReady, Meta: meta}, nil
}

func (a *App) CancelOAuth() {
	a.mu.Lock()
	had := a.oauth != nil
	a.oauth = nil
	a.mu.Unlock()
	if had {
		a.restoreScheme()
	}
}

// ===== 重新授权（会话过期的账号刷新登录态） =====

// StartReOAuth 对指定账号发起重新授权：自动识别渠道、接管回调协议、打开浏览器。
func (a *App) StartReOAuth(id string) error {
	meta, snap, err := store.Load(id)
	if err != nil {
		return err
	}
	p := auth.ProviderFromSnapshot(snap, zcrypto.DefaultSecret())
	state, err := auth.NewState()
	if err != nil {
		return err
	}
	a.mu.Lock()
	if a.oauth != nil {
		a.mu.Unlock()
		return fmt.Errorf("已有进行中的登录流程，请先完成或取消")
	}
	a.oauth = &oauthPending{state: state, provider: p, reauthID: meta.ID}
	a.mu.Unlock()
	if bp := selfBundlePath(); bp != "" {
		setDefaultURLHandler("zcode", bp)
	}
	return platform.OpenURL(auth.BuildAuthorizeURL(p, state))
}

// FinishReOAuth 兑底路径：粘贴回调链接完成重新授权。
func (a *App) FinishReOAuth(input string) (*ReAuthOutcome, error) {
	a.mu.Lock()
	p := a.oauth
	a.mu.Unlock()
	if p == nil || p.reauthID == "" {
		return nil, fmt.Errorf("请先点击「重新授权」打开浏览器")
	}
	code, cbState, err := auth.ParseCallbackInput(input)
	if err != nil {
		return nil, err
	}
	if cbState != "" && cbState != p.state {
		return nil, fmt.Errorf("回调 state 不匹配（可能粘贴了旧的链接），请重新发起授权")
	}
	return a.doReAuth(code, p)
}

// finishReOAuth 协议回调自动完成路径，事件通知前端。
func (a *App) finishReOAuth(code string, p *oauthPending) {
	out, err := a.doReAuth(code, p)
	if err != nil {
		wruntime.EventsEmit(a.ctx, "oauth:error", err.Error())
		return
	}
	wruntime.WindowShow(a.ctx)
	wruntime.EventsEmit(a.ctx, "oauth:done", map[string]any{
		"reauth":         true,
		"id":             out.Meta.ID,
		"label":          out.Meta.Label,
		"liveRefreshed":  out.LiveRefreshed,
		"zcodeRestarted": out.ZCodeRestarted,
	})
}

// doReAuth 换 token 并覆盖更新目标账号快照。
// 若目标账号正是当前 ZCode 登录的账号，同时把新登录态写入 live 文件并重启 ZCode
// （否则 ZCode 还拿着旧 token，套餐查询依旧失败，用户也不用手动切出去再切回来）。
type ReAuthOutcome struct {
	Meta           *store.Meta `json:"meta"`
	LiveRefreshed  bool        `json:"liveRefreshed"`
	ZCodeRestarted bool        `json:"zcodeRestarted"`
}

func (a *App) doReAuth(code string, p *oauthPending) (*ReAuthOutcome, error) {
	ts, err := auth.ExchangeCode(p.provider, code, p.state)
	if err != nil {
		return nil, err
	}
	meta, err := auth.UpdateAccount(p.provider, ts, p.reauthID, config.Load())
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.oauth = nil
	a.mu.Unlock()
	a.restoreScheme()

	out := &ReAuthOutcome{Meta: meta}
	if a.liveAccountIs(meta.ID) {
		if res, err := switcher.RefreshLive(meta.ID); err == nil {
			out.LiveRefreshed = true
			out.ZCodeRestarted = res.Restarted
		}
		// 刷新失败不报错：快照已是新的，下次切换到该账号自然生效
	}
	// 新会话立即探测一次，前端拿到最新健康状态（重新授权按钮随之消失）
	go func() {
		all := session.Sweep(0)
		wruntime.EventsEmit(a.ctx, "health:updated", healthList(all))
	}()
	return out, nil
}

// liveAccountIs 判断当前 ZCode 实时登录态是否就是指定账号。
func (a *App) liveAccountIs(id string) bool {
	cred, cfg, err := store.ReadLive()
	if err != nil || !hasSession(cred) {
		return false
	}
	fp := fingerprint.Extract(cred, cfg, zcrypto.DefaultSecret())
	if fp == nil {
		return false
	}
	if fp.EmailShortID == id {
		return true
	}
	meta, _, err := store.Load(id)
	if err != nil || meta.UserID == "" {
		return false
	}
	return fp.UserID == meta.UserID
}

// ===== 会话健康检测 / 保活 =====

// HealthView 账号会话健康状态的绑定层视图。
// 用切片而非 map 返回：wails 只为直接返回/切片元素类型生成 TS class，map 值类型不生成。
type HealthView struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CheckedAt int64  `json:"checkedAt"`
}

func healthList(all map[string]session.Report) []HealthView {
	out := make([]HealthView, 0, len(all))
	for id, r := range all {
		out = append(out, HealthView{ID: id, Status: string(r.Status), CheckedAt: r.CheckedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// GetHealth 读取缓存的账号会话健康状态（不发网络请求，立即返回）。
func (a *App) GetHealth() []HealthView { return healthList(session.LoadAll()) }

// CheckHealthNow 立即探测所有账号的会话状态（网络请求，前端异步调用）。
func (a *App) CheckHealthNow() []HealthView { return healthList(session.Sweep(0)) }

// runHealthKeeper 后台保活巡检：启动后先巡检一轮，之后每小时检查一次，
// 对距上次探测超过 keepAliveInterval 的账号重新探测（探测即保活）。
func (a *App) runHealthKeeper() {
	time.Sleep(3 * time.Second) // 避开启动高峰
	a.emitHealth(session.Sweep(keepAliveInterval))
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.emitHealth(session.Sweep(keepAliveInterval))
		}
	}
}

func (a *App) emitHealth(all map[string]session.Report) {
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "health:updated", healthList(all))
	}
}

// ===== 切换 / 回滚 =====

type UseResult struct {
	Label        string `json:"label"`
	WriteBackID  string `json:"writeBackId,omitempty"`
	WriteBackErr string `json:"writeBackErr,omitempty"`
	Restarted    bool   `json:"restarted"`
}

func (a *App) Use(id string) (*UseResult, error) {
	res, err := switcher.Use(id, true)
	if err != nil {
		return nil, err
	}
	out := &UseResult{Label: res.Account.Label, WriteBackID: res.WriteBackID, Restarted: res.Restarted}
	if res.WriteBackErr != nil {
		out.WriteBackErr = res.WriteBackErr.Error()
	}
	return out, nil
}

func (a *App) Rollback() error { return switcher.Rollback(true) }

// ===== 账号管理 =====

func (a *App) DeleteAccount(id string) error {
	if err := store.Delete(id); err != nil {
		return err
	}
	session.Forget(id)
	return nil
}

func (a *App) RenameAccount(id, label string) error {
	_, err := store.Rename(id, label, "")
	return err
}

// ===== 额度 =====

// GetQuota 查询账号额度；id 为空查当前登录账号。
func (a *App) GetQuota(id string) (*quota.Overview, error) {
	secret := zcrypto.DefaultSecret()
	var tokens []string
	var cred map[string]any
	if id == "" {
		var err error
		tokens, err = quota.CurrentTokens(secret)
		if err != nil {
			return nil, err
		}
		cred, _, _ = store.ReadLive()
	} else {
		_, snap, err := store.Load(id)
		if err != nil {
			return nil, err
		}
		tokens = quota.TokensFromSnapshot(snap, secret)
		cred = quota.CredMapFromSnapshot(snap)
	}
	ov, err := quota.ForTokens(tokens, config.Load())
	if err != nil {
		return nil, err
	}
	quota.EnrichCodingPlan(ov, cred, secret)
	quota.EnrichResetCards(ov, cred, secret)
	return ov, nil
}

// ===== ZCode 进程 =====

func (a *App) KillZCode() error  { return platform.KillZCode(8 * time.Second) }
func (a *App) LaunchZCode() error { return platform.LaunchZCode() }

// ===== 版本 / 更新 =====

// GetAppInfo 应用信息（名称/版本/仓库，「关于」弹窗用），源自内嵌全局配置。
func (a *App) GetAppInfo() buildinfo.Info { return buildinfo.Current }

// CheckUpdate 查询 GitHub 是否有新版本。
func (a *App) CheckUpdate() (*update.Result, error) { return update.Check() }
