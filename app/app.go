package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"zcas/internal/auth"
	"zcas/internal/buildinfo"
	"zcas/internal/config"
	"zcas/internal/fingerprint"
	"zcas/internal/paths"
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

// zcodeWatchInterval ZCode 运行状态轮询周期：仅在前端窗口可见时探测，状态翻转才发事件。
const zcodeWatchInterval = 3 * time.Second

// App 是 Wails 绑定层：只做参数转换 + 调 internal 包，不含业务逻辑。
type App struct {
	ctx context.Context

	mu    sync.Mutex
	oauth *oauthPending // 进行中的 OAuth 会话

	// 真退出标志：OnBeforeClose 据此放行关闭；托盘「退出」/关闭时选择退出都会置位。
	reallyQuit atomic.Bool

	// 托盘（关闭即最小化 + 状态栏快捷操作）。
	tray *trayLoop

	// quotas 额度共享存储：主界面与托盘菜单的唯一数据源（见 quotastore.go）。
	quotas *quotaStore
	// curSavedID 当前登录账号对应的已保存账号 id 缓存（GetState 时刷新）：
	// 实时查询（id=""）回写共享存储时用它归档到账号键下。
	curSavedID atomic.Pointer[string]

	// ZCode 运行状态监控：watchOn 由前端按窗口可见性开关；lastRunning/haveLast
	// 用于「变化才发事件」，避免每个轮询周期都打扰前端。
	watchOn     atomic.Bool
	lastRunning atomic.Bool
	haveLast    atomic.Bool
}

type oauthPending struct {
	state    string
	provider auth.Provider
	name     string
	note     string
	reauthID string // 非空表示这是对已有账号的重新授权

	// zai_cn 轮询式登录（ZCode 3.18.2 新流程）：flow 非空表示走「服务端 init +
	// 轮询取 token」，完成不依赖 zcode:// 回调，由后台 poller 驱动。
	flow   *auth.CliFlow
	cancel chan struct{}
}

func NewApp() *App { return &App{quotas: newQuotaStore()} }

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.autoLaunchZCode()
	a.tray = newTrayLoop(a)
	a.tray.start()
	go a.runHealthKeeper()
	go a.runZCodeWatch()
}

// autoLaunchZCode 启动 zcas 时，若配置允许、当前已登录、且 ZCode 未运行，则后台拉起 ZCode，
// 省去「开完 zcas 还得手动再开一次 ZCode」。未登录时不拉起（否则只会停在 ZCode 登录页）。
// 关闭方式：~/.zcas/config.json 里设 "autoLaunchZCode": false。
func (a *App) autoLaunchZCode() {
	if !config.Load().ShouldAutoLaunch() || platform.IsZCodeRunning() {
		return
	}
	if cred, _, err := store.ReadLive(); err != nil || !hasSession(cred) {
		return
	}
	_ = platform.LaunchZCode()
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

// ZCodeView 本机 ZCode 客户端信息（渠道兼容引导用）：
// zai_cn（zchat 登录）账号需要 ZCode ≥ 3.18.2，旧版客户端不认这个渠道。
type ZCodeView struct {
	Installed     bool   `json:"installed"`
	Version       string `json:"version,omitempty"`
	SupportsZaiCN bool   `json:"supportsZaiCN"`
}

type StateView struct {
	Running        bool              `json:"running"`
	Current        *CurrentView      `json:"current"`
	Accounts       []*store.Meta     `json:"accounts"`
	HasLastBackup  bool              `json:"hasLastBackup"`
	ZCode          *ZCodeView        `json:"zcode"`
	LoginProviders map[string]string `json:"loginProviders,omitempty"` // 账号 id → 登录渠道
}

func (a *App) GetState() *StateView {
	view := &StateView{Running: platform.IsZCodeRunning(), HasLastBackup: store.HasLast()}
	if cred, cfg, err := store.ReadLive(); err == nil && hasSession(cred) {
		if fp := fingerprint.Extract(cred, cfg, zcrypto.DefaultSecret()); fp != nil {
			savedID := store.ResolveSavedID(fp)
			a.curSavedID.Store(&savedID) // 实时额度查询回写共享存储时按它归档
			view.Current = &CurrentView{
				Label: fp.Label, Provider: fp.Provider, ShortID: fp.ShortID,
				Email: fp.Email, Saved: savedID != "",
			}
			view.Current.SavedID = savedID
		}
	}
	metas, _ := store.List()
	view.Accounts = metas
	// ZCode 版本与各账号登录渠道：界面据此引导渠道选择 / 切换前兼容提醒
	ver := platform.ZCodeVersion()
	view.ZCode = &ZCodeView{Installed: ver != "", Version: ver, SupportsZaiCN: platform.ZCodeSupportsZaiCN()}
	if len(metas) > 0 {
		secret := zcrypto.DefaultSecret()
		view.LoginProviders = map[string]string{}
		for _, m := range metas {
			if _, snap, err := store.Load(m.ID); err == nil {
				if p := store.LoginProvider(snap, secret); p != "" {
					view.LoginProviders[m.ID] = p
				}
			}
		}
	}
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

// StartOAuth 打开浏览器发起登录。providerID: zai(全球) / zai_cn(中国) / bigmodel(旧中国渠道)。
func (a *App) StartOAuth(providerID, name, note string) error {
	p, err := auth.ParseProvider(providerID)
	if err != nil {
		return err
	}
	return a.beginOAuth(&oauthPending{provider: p, name: name, note: note}, p)
}

// beginOAuth 注册会话、接管协议并打开授权页；zai_cn 轮询渠道额外启动后台 poller。
func (a *App) beginOAuth(pending *oauthPending, p auth.Provider) error {
	// 先快速失败再发起网络请求（zai_cn 的 cli/init 是真实服务端调用）
	a.mu.Lock()
	busy := a.oauth != nil
	a.mu.Unlock()
	if busy {
		return fmt.Errorf("已有进行中的登录流程，请先完成或取消")
	}
	if !p.Polling {
		state, err := auth.NewState()
		if err != nil {
			return err
		}
		pending.state = state
	}
	authURL := auth.BuildAuthorizeURL(p, pending.state)
	cancel := make(chan struct{})
	pending.cancel = cancel
	if p.Polling {
		flow, err := auth.StartCliFlow(p)
		if err != nil {
			return err
		}
		pending.flow = flow
		authURL = flow.AuthorizeURL
	}
	a.mu.Lock()
	if a.oauth != nil {
		a.mu.Unlock()
		return fmt.Errorf("已有进行中的登录流程，请先完成或取消")
	}
	a.oauth = pending
	a.mu.Unlock()
	// 临时接管 zcode://（仅在打包成 .app 运行时有效）；
	// zai_cn 完成虽不依赖回调，但接管可避免登录尾跳误唤起官方客户端
	if bp := selfBundlePath(); bp != "" {
		setDefaultURLHandler("zcode", bp)
	}
	if err := platform.OpenURL(authURL); err != nil {
		// 打开浏览器失败时释放会话，让用户可以重试或手动复制链接
		a.mu.Lock()
		if a.oauth == pending {
			a.oauth = nil
		}
		a.mu.Unlock()
		close(cancel)
		if pending.flow != nil {
			return fmt.Errorf("登录流已创建但打开浏览器失败: %w（可把此链接手动粘到浏览器打开: %s）", err, authURL)
		}
		return err
	}
	if pending.flow != nil {
		f := pending.flow
		oauthLog("flow 开始: provider=%s flowID=%s state=%s 授权页=%s 过期于=%s 轮询间隔=%s",
			f.Provider, f.FlowID, f.State, f.AuthorizeURL,
			f.ExpiresAt.Format("15:04:05"), f.Interval)
		go a.pollFlow(pending, cancel)
	}
	if a.ctx != nil {
		// 前端据此区分 UI：轮询渠道自动完成（无粘贴框），回调渠道保留粘贴兜底。
		// state/expiresAt 用于界面展示——用户可与浏览器地址栏的 state 比对，
		// 不一致说明完成的是旧标签页（对应 flow 已废弃，中转页会报「授权失败」）。
		started := map[string]any{
			"provider": p.ID,
			"polling":  pending.flow != nil,
			"reauth":   pending.reauthID != "",
		}
		if pending.flow != nil {
			started["state"] = pending.flow.State
			started["expiresAt"] = pending.flow.ExpiresAt.UnixMilli()
		}
		wruntime.EventsEmit(a.ctx, "oauth:started", started)
	}
	return nil
}

// oauthLog OAuth 流程诊断日志（追加写 ~/.zcas/oauth-debug.log，出错排查用；
// 写失败静默——诊断日志不能影响主流程）。
func oauthLog(format string, args ...any) {
	f, err := os.OpenFile(paths.OAuthLogFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// pollFlow zai_cn 后台轮询：按服务端建议间隔查询，直到 ready/failed/过期/取消。
func (a *App) pollFlow(pending *oauthPending, cancel chan struct{}) {
	flow := pending.flow
	ticker := time.NewTicker(flow.Interval)
	defer ticker.Stop()
	pollCount := 0
	for {
		select {
		case <-cancel:
			oauthLog("flow 取消: flowID=%s state=%s", flow.FlowID, flow.State)
			return
		case <-a.ctx.Done():
			return
		case <-ticker.C:
		}
		status, ts, err := flow.Poll()
		pollCount++
		// 心跳事件：前端展示「最近检测时间 · 第 N 次」，证明流程活着、正在推进
		// （网页显示登录完成到轮询端点 ready 之间有数秒服务端同步窗口，没有心跳
		// 用户无法区分「正在执行」和「已卡死」）
		if a.ctx != nil {
			wruntime.EventsEmit(a.ctx, "oauth:poll", map[string]any{
				"at":     time.Now().UnixMilli(),
				"count":  pollCount,
				"reauth": pending.reauthID != "",
			})
		}
		switch status {
		case auth.FlowReady:
			oauthLog("flow ready: flowID=%s state=%s", flow.FlowID, flow.State)
			if ts != nil {
				a.completeOAuth(ts, pending)
				return
			}
			if err != nil {
				a.failOAuth(pending, err)
				return
			}
		case auth.FlowFailed:
			oauthLog("flow failed: flowID=%s state=%s err=%v", flow.FlowID, flow.State, err)
			a.failOAuth(pending, err)
			return
		}
		// pending（含网络抖动）：继续下一轮；过期判定在 Poll 内部返回 failed
	}
}

// failOAuth 终止登录会话并通知前端。
func (a *App) failOAuth(pending *oauthPending, err error) {
	a.mu.Lock()
	stale := a.oauth != pending
	if !stale {
		a.oauth = nil
	}
	a.mu.Unlock()
	if stale {
		return
	}
	a.restoreScheme()
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "oauth:error", "登录未完成："+err.Error())
	}
}

// restoreScheme 把 zcode:// 协议 handler 归还给官方 ZCode 客户端（按平台实现）。
func (a *App) restoreScheme() { restoreURLSchemeToZCode() }

// handleOpenURL 处理 macOS open-url 事件（浏览器 zcode:// 回调送达本应用）。
func (a *App) handleOpenURL(raw string) {
	a.mu.Lock()
	p := a.oauth
	a.mu.Unlock()
	if p == nil {
		return // 没有进行中的登录会话，忽略（可能是官方客户端的回调被误投递）
	}
	if p.flow != nil {
		// zai_cn 轮询流程：完成以轮询为准，回调（只带归属参数）静默忽略
		return
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

// finishOAuth 授权码换 token 后走统一完成路径（zai/bigmodel 浏览器回调）。
func (a *App) finishOAuth(code string, p *oauthPending) {
	ts, err := auth.ExchangeCode(p.provider, code, p.state)
	if err != nil {
		wruntime.EventsEmit(a.ctx, "oauth:error", err.Error())
		return
	}
	a.completeOAuth(ts, p)
}

// emitPhase 推送登录阶段事件（前端等待面板的分步进度）：
// "authorized"（已拿到 token）→ "saving"（正在入库），终态仍是 oauth:done / oauth:error。
func (a *App) emitPhase(p *oauthPending, phase string) {
	if a.ctx == nil {
		return
	}
	wruntime.EventsEmit(a.ctx, "oauth:phase", map[string]any{
		"phase":  phase,
		"reauth": p.reauthID != "",
	})
}

// completeOAuth 统一完成路径：入库 + 清会话 + 通知前端（zai_cn 轮询与回调渠道共用）。
func (a *App) completeOAuth(ts *auth.TokenSet, p *oauthPending) {
	if p.reauthID != "" {
		out, err := a.doReAuth(ts, p)
		if err != nil {
			wruntime.EventsEmit(a.ctx, "oauth:error", err.Error())
			return
		}
		a.showMainWindow()
		wruntime.EventsEmit(a.ctx, "oauth:done", map[string]any{
			"reauth":         true,
			"id":             out.Meta.ID,
			"label":          out.Meta.Label,
			"liveRefreshed":  out.LiveRefreshed,
			"zcodeRestarted": out.ZCodeRestarted,
		})
		return
	}
	a.emitPhase(p, "authorized")
	a.emitPhase(p, "saving")
	meta, created, err := auth.AddAccount(p.provider, ts, p.name, p.note, config.Load())
	if err != nil {
		wruntime.EventsEmit(a.ctx, "oauth:error", err.Error())
		return
	}
	a.clearPending(p)
	a.showMainWindow()
	wruntime.EventsEmit(a.ctx, "oauth:done", map[string]any{
		"created": created,
		"label":   meta.Label,
		"id":      meta.ID,
	})
}

// clearPending 结束会话：置空、停 poller、归还协议。
func (a *App) clearPending(p *oauthPending) {
	a.mu.Lock()
	stale := a.oauth != p
	if !stale {
		a.oauth = nil
	}
	a.mu.Unlock()
	if stale {
		return
	}
	if p.cancel != nil {
		close(p.cancel)
	}
	a.restoreScheme()
}

type OAuthResult struct {
	Created bool        `json:"created"`
	Meta    *store.Meta `json:"meta"`
}

// FinishOAuth 兑底路径：用户手动粘贴回调链接完成登录（zai_cn 轮询渠道不需要）。
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
	if p.flow != nil {
		return nil, fmt.Errorf("该渠道登录会自动完成，无需粘贴回调链接（请等待浏览器完成授权）")
	}
	code, cbState, err := auth.ParseCallbackInput(input)
	if err != nil {
		return nil, err
	}
	if cbState != "" && cbState != p.state {
		return nil, fmt.Errorf("回调 state 不匹配（可能粘贴了旧的链接），请重新发起登录")
	}
	a.emitPhase(p, "authorized")
	a.emitPhase(p, "saving")
	ts, err := auth.ExchangeCode(p.provider, code, p.state)
	if err != nil {
		return nil, err
	}
	meta, created, err := auth.AddAccount(p.provider, ts, name, note, config.Load())
	if err != nil {
		return nil, err
	}
	a.clearPending(p)
	return &OAuthResult{Created: created, Meta: meta}, nil
}

func (a *App) CancelOAuth() {
	a.mu.Lock()
	p := a.oauth
	a.oauth = nil
	a.mu.Unlock()
	if p == nil {
		return
	}
	if p.cancel != nil {
		close(p.cancel)
	}
	a.restoreScheme()
}

// ===== 重新授权（会话过期的账号刷新登录态） =====

// StartReOAuth 对指定账号发起重新授权：自动识别渠道（含 zai_cn 轮询）、接管回调协议、打开浏览器。
func (a *App) StartReOAuth(id string) error {
	meta, snap, err := store.Load(id)
	if err != nil {
		return err
	}
	p := auth.ProviderFromSnapshot(snap, zcrypto.DefaultSecret())
	return a.beginOAuth(&oauthPending{provider: p, reauthID: meta.ID}, p)
}

// FinishReOAuth 兑底路径：粘贴回调链接完成重新授权。
func (a *App) FinishReOAuth(input string) (*ReAuthOutcome, error) {
	a.mu.Lock()
	p := a.oauth
	a.mu.Unlock()
	if p == nil || p.reauthID == "" {
		return nil, fmt.Errorf("请先点击「重新授权」打开浏览器")
	}
	if p.flow != nil {
		return nil, fmt.Errorf("该渠道重新授权会自动完成，无需粘贴回调链接（请等待浏览器完成授权）")
	}
	code, cbState, err := auth.ParseCallbackInput(input)
	if err != nil {
		return nil, err
	}
	if cbState != "" && cbState != p.state {
		return nil, fmt.Errorf("回调 state 不匹配（可能粘贴了旧的链接），请重新发起授权")
	}
	ts, err := auth.ExchangeCode(p.provider, code, p.state)
	if err != nil {
		return nil, err
	}
	return a.doReAuth(ts, p)
}

// finishReOAuth 协议回调自动完成路径，事件通知前端。
func (a *App) finishReOAuth(code string, p *oauthPending) {
	ts, err := auth.ExchangeCode(p.provider, code, p.state)
	if err != nil {
		wruntime.EventsEmit(a.ctx, "oauth:error", err.Error())
		return
	}
	out, err := a.doReAuth(ts, p)
	if err != nil {
		wruntime.EventsEmit(a.ctx, "oauth:error", err.Error())
		return
	}
	a.showMainWindow()
	wruntime.EventsEmit(a.ctx, "oauth:done", map[string]any{
		"reauth":         true,
		"id":             out.Meta.ID,
		"label":          out.Meta.Label,
		"liveRefreshed":  out.LiveRefreshed,
		"zcodeRestarted": out.ZCodeRestarted,
	})
}

// doReAuth 用新 token 覆盖更新目标账号快照。
// 若目标账号正是当前 ZCode 登录的账号，同时把新登录态写入 live 文件并重启 ZCode
// （否则 ZCode 还拿着旧 token，套餐查询依旧失败，用户也不用手动切出去再切回来）。
type ReAuthOutcome struct {
	Meta           *store.Meta `json:"meta"`
	LiveRefreshed  bool        `json:"liveRefreshed"`
	ZCodeRestarted bool        `json:"zcodeRestarted"`
}

func (a *App) doReAuth(ts *auth.TokenSet, p *oauthPending) (*ReAuthOutcome, error) {
	a.emitPhase(p, "authorized")
	a.emitPhase(p, "saving")
	meta, err := auth.UpdateAccount(p.provider, ts, p.reauthID, config.Load())
	if err != nil {
		return nil, err
	}
	a.clearPending(p)

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
	a.quotas.forget(id)
	session.Forget(id)
	return nil
}

func (a *App) RenameAccount(id, label string) error {
	_, err := store.Rename(id, label, "")
	return err
}

// ===== 额度 =====

// GetQuota 查询账号额度；id 为空查当前登录账号。
// 成功结果统一回写共享存储（主界面与托盘共用一份），托盘菜单下次构建即与主界面一致。
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
	if id == "" {
		// 实时查询按已保存账号 id 归档；guest（未保存）无处归档，跳过
		if key := a.currentSavedID(); key != "" {
			a.quotas.put(key, ov)
			a.emitQuota(key, ov)
		}
	} else {
		a.quotas.put(id, ov)
		a.emitQuota(id, ov)
	}
	return ov, nil
}

// currentSavedID 读当前登录账号对应的已保存账号 id（GetState 时缓存）；未登录/未保存为空。
func (a *App) currentSavedID() string {
	if p := a.curSavedID.Load(); p != nil {
		return *p
	}
	return ""
}

// emitQuota 共享存储有新结果时推送前端：窗口可见才发（隐藏期间无人观看），
// 隐藏期漏掉的更新由前端聚焦时的 QuotaCache 轻量同步补齐。
func (a *App) emitQuota(id string, ov *quota.Overview) {
	if ov == nil || !a.watchOn.Load() {
		return
	}
	keys := []string{id}
	if a.currentSavedID() == id {
		keys = append(keys, "current") // 前端 quotas 状态里实时账号的键
	}
	wruntime.EventsEmit(a.ctx, "quota:updated", map[string]any{"keys": keys, "data": ov})
}

// QuotaCache 共享存储快照（账号 id → 最近查询结果）：主界面聚焦时轻量同步用，不发网络。
func (a *App) QuotaCache() map[string]*quota.Overview { return a.quotas.snapshot() }

// ===== ZCode 进程 =====

func (a *App) KillZCode() error   { return platform.KillZCode(8 * time.Second) }
func (a *App) LaunchZCode() error { return platform.LaunchZCode() }

// SetRunningWatch 由前端按窗口可见性开关 ZCode 运行状态轮询：
// 用户实际看着界面时才探测，窗口隐藏/最小化时停止，避免无人观看的无用轮询。
func (a *App) SetRunningWatch(on bool) { a.watchOn.Store(on) }

// runZCodeWatch 每 zcodeWatchInterval 检查一次 ZCode 是否运行，仅在窗口可见（watchOn）时探测，
// 且状态相对上次发生翻转时才发 zcode:running 事件——平时不打扰前端，退出/启动 ZCode 后 ≤3s 自动更新徽标。
func (a *App) runZCodeWatch() {
	ticker := time.NewTicker(zcodeWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			if !a.watchOn.Load() {
				continue
			}
			running := platform.IsZCodeRunning()
			if !a.haveLast.Load() || a.lastRunning.Load() != running {
				a.lastRunning.Store(running)
				a.haveLast.Store(true)
				wruntime.EventsEmit(a.ctx, "zcode:running", running)
			}
		}
	}
}

// ===== 版本 / 更新 =====

// GetAppInfo 应用信息（名称/版本/仓库，「关于」弹窗用），源自内嵌全局配置。
func (a *App) GetAppInfo() buildinfo.Info { return buildinfo.Current }

// CheckUpdate 查询 GitHub 是否有新版本。
func (a *App) CheckUpdate() (*update.Result, error) { return update.Check() }
