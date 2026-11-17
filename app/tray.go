package main

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/options"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"zcas/internal/buildinfo"
	"zcas/internal/config"
	"zcas/internal/platform"
	"zcas/internal/quota"
	"zcas/internal/switcher"
	"zcas/internal/tray"
)

// trayLoop 托盘生命周期管理：菜单构建、点击分发、真退出。
// 额度数据统一走 App 层共享存储（见 quotastore.go），这里只按节流触发补查、构建展示。
// 平台差异收敛在 internal/tray（darwin 自建 NSStatusItem / windows energye-systray / 其他 no-op）。
type trayLoop struct {
	app *App
	tr  tray.Controller
}

func newTrayLoop(app *App) *trayLoop {
	t := &trayLoop{app: app}
	t.tr = tray.New()
	return t
}

// start 建立托盘并注册回调。
func (t *trayLoop) start() {
	tray.OnClick = t.onClick
	// Dock 图标点击（应用已运行）：托盘态下恢复主窗口（wails 未实现 reopen，darwin 层动态补挂）
	tray.OnDockIconClick = t.app.showMainWindow
	tray.OnOpen = func() {
		// 菜单展开回调（darwin=menuWillOpen 主线程同步 / windows=SetOnRClick 弹出前同步）：
		// 原地重建保证本次展开就是共享存储里的最新状态（只读内存，毫秒级），
		// 随后按短节流补查（15 秒内连开不重复查）——结果回来重建菜单，下次展开即新数据
		t.tr.Update(t.buildMenu())
		go t.fetchQuotas(quotaOpenFetchGap)
	}
	t.tr.Start(trayIconCurrent, buildinfo.Current.DisplayName, t.buildMenu())
	// 启动即预热：首次点开托盘菜单就有进度条（存储为空 → 全部视为该查，直接全量）
	go func() {
		time.Sleep(2 * time.Second)
		t.fetchQuotas(quotaWarmFetchGap)
	}()
	// 周期保活巡检：5 小时窗口会随时间自然回落，没人点开菜单时数据也会变旧；
	// 每分钟巡检 + quotaWarmFetchGap 节流（只查超过间隔没查的账号，开销极小）
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-t.app.ctx.Done():
				return
			case <-ticker.C:
				t.fetchQuotas(quotaWarmFetchGap)
			}
		}
	}()
}

func (t *trayLoop) quit() { t.tr.Quit() }

// refreshMenu 重建托盘菜单（异步，不阻塞调用方）。
func (t *trayLoop) refreshMenu() {
	go t.tr.Update(t.buildMenu())
}

// showMainWindow 恢复主窗口（含把 Dock 图标切回显示——托盘态时它是隐藏的）。
func (a *App) showMainWindow() {
	a.tray.tr.SetDockVisible(true)
	wruntime.WindowUnminimise(a.ctx)
	wruntime.WindowShow(a.ctx)
}

// hideToTray 最小化到托盘：藏窗口并隐藏 Dock 图标（应用只留状态栏图标）。
func (a *App) hideToTray() {
	wruntime.WindowHide(a.ctx)
	a.tray.tr.SetDockVisible(false)
}

// onClick 分发托盘菜单动作。
func (t *trayLoop) onClick(action string) {
	a := t.app
	switch {
	case action == "show":
		a.showMainWindow()
	case action == "launch":
		_ = platform.LaunchZCode()
		time.Sleep(1500 * time.Millisecond) // 给进程一点起来时间再刷菜单
		t.refreshMenu()
	case action == "settings":
		a.showMainWindow()
		wruntime.EventsEmit(a.ctx, "menu:settings")
	case action == "about":
		a.showMainWindow()
		wruntime.EventsEmit(a.ctx, "menu:about")
	case action == "quit":
		a.doQuit()
	case strings.HasPrefix(action, "use:"):
		id := strings.TrimPrefix(action, "use:")
		go func() {
			// 切换失败也不弹窗：下次打开菜单看到的还是旧状态即自说明
			_, _ = switcher.Use(id, true)
			t.fetchQuotas(quotaOpenFetchGap)
			t.refreshMenu()
		}()
	}
}

// buildMenu 组装托盘菜单（读快照数据 + 额度缓存，不发网络请求）。
func (t *trayLoop) buildMenu() []tray.Item {
	state := t.app.GetState()

	status := "ZCode 未运行"
	if state.Running {
		status = "ZCode 运行中"
	}
	if state.Current != nil {
		status += " · " + state.Current.Label
	}
	menu := []tray.Item{
		{Title: status, Disabled: true},
		{Separator: true},
		{Title: "恢复窗口", Action: "show"},
	}
	if state.Running {
		menu = append(menu, tray.Item{Title: "启动 ZCode", Disabled: true})
	} else {
		menu = append(menu, tray.Item{Title: "启动 ZCode", Action: "launch"})
	}

	// 账号子菜单：切换与额度合一——每项一个账号，点击即切换，标题后是 5 小时额度进度条；
	// 当前账号打 ✓ 并禁用（无谓切到自己），额度读共享存储（最新一次查询结果，
	// 主界面刷新/菜单展开/后台巡检都会喂它，无需手动刷新入口）。
	currentID := ""
	if state.Current != nil {
		currentID = state.Current.SavedID
	}
	accountItems := []tray.Item{}
	for _, m := range state.Accounts {
		it := tray.Item{Title: m.Label, Tooltip: "点击切换并重启 ZCode", Bar: quotaBar5h(t.app.quotas.get(m.ID))}
		if m.ID == currentID {
			it.Checked = true // 当前账号：名称右侧绿色圆点（darwin 视图自绘）
			it.Disabled = true
			it.Tooltip = "当前账号"
		} else {
			it.Action = "use:" + m.ID
		}
		accountItems = append(accountItems, it)
	}
	if len(accountItems) == 0 {
		accountItems = []tray.Item{{Title: "（没有保存的账号）", Disabled: true}}
	}
	menu = append(menu, tray.Item{Title: "账号额度", Children: accountItems})

	menu = append(menu,
		tray.Item{Separator: true},
		tray.Item{Title: "设置", Action: "settings"},
		tray.Item{Title: "关于 " + buildinfo.Current.DisplayName, Action: "about"},
		tray.Item{Separator: true},
		tray.Item{Title: "退出", Action: "quit"},
	)
	return menu
}

// fetchQuotas 补查「超过 gap 没查过」的账号额度并回写共享存储，完成后重建托盘菜单（下次展开即新数据）。
// 菜单展开/切换账号后用 quotaOpenFetchGap（用户动作，几乎每次都真查）；
// 每分钟巡检与启动预热用 quotaWarmFetchGap（低频保活）。
func (t *trayLoop) fetchQuotas(gap time.Duration) {
	var ids []string
	for _, m := range t.app.GetState().Accounts {
		ids = append(ids, m.ID)
	}
	due := t.app.quotas.due(ids, gap)
	if len(due) == 0 {
		return
	}
	t.fetchIDs(due)
	t.refreshMenu()
}

// fetchIDs 并发查询一组账号并回写共享存储（GetQuota 成功路径内部完成回写与前端推送）。
func (t *trayLoop) fetchIDs(ids []string) {
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := t.app.GetQuota(id); err != nil {
				t.app.quotas.put(id, nil) // 失败：记尝试时间（节流照常生效），保留旧数据
			}
		}(id)
	}
	wg.Wait()
}

// quotaBar5h 额度进度条数据：5 小时窗口的**已用**百分比（订阅套餐等其他信息不进托盘）。
// 口径与主界面一致（已用/限额）——曾经用剩余百分比，两处数字对不上容易误读。
func quotaBar5h(ov *quota.Overview) *float64 {
	if ov == nil || ov.CodingPlan == nil {
		return nil
	}
	for _, l := range ov.CodingPlan.Limits {
		if strings.Contains(l.Name, "5小时") && l.Limit > 0 {
			// 取整后的值转回 float：托盘/macOS 的 %.0f 不再各自舍入，与主窗口逐字一致
			pct := float64(quota.PercentOf(l.Used, l.Limit))
			return &pct
		}
	}
	return nil
}

// ===== 关闭行为 / 真退出 =====

// beforeClose Wails OnBeforeClose：真退出时放行，否则按配置最小化到托盘或弹询问。
// Wails 在 macOS 上把红叉关闭和 Dock 退出 / Cmd+Q 都收敛到这，用应用级退出标志区分：
// Dock 退出 / Cmd+Q = 真退出（符合系统习惯），红叉才走用户偏好（询问/最小化/退出）。
func (a *App) beforeClose(ctx context.Context) bool {
	if a.reallyQuit.Load() {
		return false // 放行关闭
	}
	if tray.ConsumeAppQuitRequest() {
		a.doQuit()
		return false
	}
	switch config.Load().CloseAction {
	case config.CloseQuit:
		a.doQuit()
	case config.CloseToTray:
		a.hideToTray()
	default: // 未设置：询问（前端弹窗，选择后回调 ResolveClose）
		a.showMainWindow()
		wruntime.EventsEmit(ctx, "close:confirm")
	}
	return true // 拦截本次关闭
}

// ResolveClose 前端关闭询问弹窗的选择回调：action 为 tray/quit；remember 时持久化偏好。
func (a *App) ResolveClose(action string, remember bool) {
	if remember {
		_ = config.SetCloseAction(action)
	}
	if action == config.CloseQuit {
		a.doQuit()
		return
	}
	a.hideToTray()
}

// GetCloseAction 读取关闭行为偏好（""=每次询问 / tray / quit），设置弹窗回显用。
func (a *App) GetCloseAction() string { return config.Load().CloseAction }

// SetCloseAction 保存关闭行为偏好（设置弹窗用，无 ResolveClose 的隐藏/退出副作用）。
func (a *App) SetCloseAction(action string) error { return config.SetCloseAction(action) }

// doQuit 真退出：置标志 → 停托盘/归还协议 handler → 退出。
// 后台 goroutine（healthKeeper/zcodeWatch）监听 ctx.Done，由 Wails 退出时统一取消。
func (a *App) doQuit() {
	if !a.reallyQuit.CompareAndSwap(false, true) {
		return // 已在退出流程中
	}
	a.mu.Lock()
	had := a.oauth != nil
	a.oauth = nil
	a.mu.Unlock()
	if had {
		a.restoreScheme() // 有进行中的登录流程时把 zcode:// 归还官方客户端
	}
	a.tray.quit()
	wruntime.Quit(a.ctx)
	// 兜底：主退出路径若被卡住（如某平台 Quit 事件未送达），强制结束进程
	time.AfterFunc(3*time.Second, func() { os.Exit(0) })
}

// onSecondInstance 二次启动：把已有实例唤到前台，不开第二个窗口。
// Windows 上 OAuth 回调以 `zcas.exe zcode://...` 启动第二个实例，
// 参数经单实例锁转到这里，转发给 handleOpenURL（与 macOS open-url 同一条路）。
func (a *App) onSecondInstance(secondData options.SecondInstanceData) {
	for _, arg := range secondData.Args {
		if strings.HasPrefix(arg, "zcode://") {
			go a.handleOpenURL(arg)
		}
	}
	a.showMainWindow()
}
