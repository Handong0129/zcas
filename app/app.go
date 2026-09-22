package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"zcas/internal/auth"
	"zcas/internal/config"
	"zcas/internal/fingerprint"
	"zcas/internal/platform"
	"zcas/internal/quota"
	"zcas/internal/store"
	"zcas/internal/switcher"
	"zcas/internal/zcrypto"
)

// zcodeAppBundle 官方 ZCode 客户端的 .app 路径（协议归还目标）。
const zcodeAppBundle = "/Applications/ZCode.app"

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
}

func NewApp() *App { return &App{} }

func (a *App) startup(ctx context.Context) { a.ctx = ctx }

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

func (a *App) DeleteAccount(id string) error { return store.Delete(id) }

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
	return ov, nil
}

// ===== ZCode 进程 =====

func (a *App) KillZCode() error  { return platform.KillZCode(8 * time.Second) }
func (a *App) LaunchZCode() error { return platform.LaunchZCode() }
