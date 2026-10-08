import React, {useCallback, useEffect, useRef, useState} from 'react'
import {
    CancelOAuth, Capture, CheckHealthNow, CheckUpdate, DeleteAccount, FinishOAuth,
    FinishReOAuth, GetAppInfo, GetHealth, GetQuota, GetState, RenameAccount, Rollback,
    StartOAuth, StartReOAuth, Use,
} from '../wailsjs/go/main/App'
import {BrowserOpenURL, EventsOn} from '../wailsjs/runtime/runtime'

function fmtNum(v) {
    return Number.isInteger(v) ? v.toLocaleString('zh-CN') : v.toFixed(2)
}

function fmtDate(ms) {
    if (!ms) return ''
    // nextResetTime 可能是秒级时间戳，按数量级兜底
    if (ms < 1e12) ms *= 1000
    const d = new Date(ms)
    const p = n => String(n).padStart(2, '0')
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

// pctClass 用量百分比分级着色：<60 绿、<85 黄、否则红
function pctClass(p) {
    return p < 60 ? 'pct-low' : p < 85 ? 'pct-mid' : 'pct-high'
}

// PctBar 使用率迷你进度条（汇总行与限额行统一使用）
function PctBar({percent, width = 60}) {
    return (
        <span className="pct-bar" style={{width}}>
            <span className={`pct-bar-fill ${pctClass(percent)}`} style={{width: `${percent}%`}}/>
        </span>
    )
}

// ===== 术语提示文案（tooltip / 帮助弹窗共用，改文案只动这里）=====
const TIPS = {
    id: '账号去重 ID：优先取邮箱 hash，其次手机号 hash，都没有时用 uid 前 8 位',
    provider: raw => `登录渠道：抓取时从 ZCode 配置识别出的官方内置槽位（${raw}）`,
    captured: t => `抓取时间：该账号快照首次保存的时间 · ${t}`,
    fresh: t => `保鲜时间：最近一次把该账号最新登录态（刷新后的 token）写回快照的时间，越新鲜切换后越不易过期 · ${t}`,
    usagePct: '使用率 = 已用 ÷ 总额 ×100%；绿色 <60%，黄色 <85%，红色 ≥85%',
    validPeriod: '当前计费周期的结束时间（季付/年付的下一续费日）',
    fiveHourCard: '5小时重置卡：立即重置「每5小时」窗口的已用量',
    weekCard: '周重置卡：立即重置「每周」额度的已用量',
    sessionExpired: '该账号的登录会话（oauth token）已在服务端过期，切换后 ZCode 会提示「套餐查询失败」。写代码不受影响（用的是长效凭证），需要点击「重新授权」刷新登录态',
}

// providerLabel 把内部渠道槽位 ID 映射成可读名称
function providerLabel(raw) {
    const map = {
        'builtin:zai-start-plan': 'z.ai Start Plan',
        'builtin:zai-coding-plan': 'z.ai Coding',
        'builtin:zai': 'z.ai',
        'builtin:bigmodel-coding-plan': 'BigModel Coding',
        'builtin:bigmodel-start-plan': 'BigModel Start Plan',
        '(encrypted)': '渠道未知',
    }
    return map[raw] || raw
}

// fmtRelative 相对时间：刚抓取/保鲜这类信息看新旧比看绝对日期更直观
function fmtRelative(ms) {
    if (!ms) return ''
    if (ms < 1e12) ms *= 1000
    const diff = Date.now() - ms
    if (diff < 0) return fmtShortDate(ms)
    const min = Math.floor(diff / 60000)
    if (min < 1) return '刚刚'
    if (min < 60) return `${min}分钟前`
    const hour = Math.floor(min / 60)
    if (hour < 24) return `${hour}小时前`
    const day = Math.floor(hour / 24)
    if (day === 1) return '昨天'
    if (day <= 30) return `${day}天前`
    return fmtShortDate(ms)
}

// fmtShortDate 月-日（当年省去年份）
function fmtShortDate(ms) {
    if (!ms) return ''
    if (ms < 1e12) ms *= 1000
    const d = new Date(ms)
    const now = new Date()
    const md = `${d.getMonth() + 1}月${d.getDate()}日`
    return d.getFullYear() === now.getFullYear() ? md : `${d.getFullYear()}年${md}`
}

// QuotaView 展示一个账号的额度状态（自动加载 + 手动刷新）
function QuotaView({qs}) {
    if (!qs) return null
    if (qs.loading && !qs.data) return <div className="quota-panel">查询中…</div>
    if (qs.error) return <div className="quota-panel quota-error">{qs.error}</div>
    const quota = qs.data
    if (!quota) return null
    // 无套餐账号（plans/balances 均空）三个汇总字段都是 null，逐个条件渲染，避免整行"未知"
    const hasSummary = !!(quota.planTier || quota.total != null || quota.used != null || quota.remaining != null)
    const hasItems = !!(quota.items && quota.items.length > 0)
    const rc = quota.resetCards
    const hasCards = !!(rc && (rc.fiveHour.count > 0 || rc.week.count > 0))
    const earliestExpiry = rc
        ? [rc.fiveHour.count > 0 ? rc.fiveHour.earliestExpiry : 0, rc.week.count > 0 ? rc.week.earliestExpiry : 0]
            .filter(v => v > 0).sort((a, b) => a - b)[0]
        : 0
    const lastUsedAt = rc
        ? [rc.fiveHour.lastUsedAt, rc.week.lastUsedAt].filter(v => v > 0).sort((a, b) => b - a)[0]
        : 0
    // 重置卡是 Coding Plan 的权益，渲染在 codingPlan 区块内部；
    // codingPlan 查询失败（静默）但有卡时兜底独立显示。
    const resetCardsRow = hasCards && (
        <div className="quota-row quota-reset-cards">
            <span className="tier">重置卡</span>
            {rc.fiveHour.count > 0 &&
                <span title={TIPS.fiveHourCard}>⚡ 5小时 ×{rc.fiveHour.count}</span>}
            {rc.week.count > 0 &&
                <span title={TIPS.weekCard}>📅 周 ×{rc.week.count}</span>}
            {earliestExpiry > 0 && <span className="reset-time">最早 {fmtDate(earliestExpiry)} 过期</span>}
            {lastUsedAt > 0 && <span className="reset-time">上次使用 {fmtDate(lastUsedAt)}</span>}
        </div>
    )
    if (!hasSummary && !hasItems && !quota.codingPlan && !hasCards) {
        return <div className="quota-panel quota-empty">暂无有效套餐</div>
    }
    return (
        <div className="quota-panel">
            {hasSummary && (
                <div className="quota-row">
                    {quota.planTier && <span className="tier">{quota.planTier.label}</span>}
                    {quota.total != null && <span>总额 {fmtNum(quota.total)}</span>}
                    {quota.used != null && <span>已用 {fmtNum(quota.used)}</span>}
                    {quota.remaining != null &&
                        <span>剩余 <b className="num-remaining">{fmtNum(quota.remaining)}</b></span>}
                    {quota.percentUsed !== null && quota.percentUsed !== undefined &&
                        <span title={TIPS.usagePct}>使用率 <b className={pctClass(quota.percentUsed)}>{quota.percentUsed.toFixed(1)}%</b>
                            <PctBar percent={quota.percentUsed}/>
                        </span>}
                </div>
            )}
            {quota.items && quota.items.map((it, i) => (
                <div className="quota-item" key={i}>
                    <span className="quota-item-name">{it.name}</span>
                    {it.remaining != null && it.total != null &&
                        <span className="quota-item-value">剩 <b className="num-remaining">{fmtNum(it.remaining)}</b>
                            <span className="num-dim"> / {fmtNum(it.total)}</span></span>}
                </div>
            ))}
            {quota.codingPlan ? (
                <>
                    {/* 上方无内容时不出分割线，避免唯一区块顶部多一条线 */}
                    <div className={`quota-row${hasSummary || hasItems ? ' quota-coding-plan' : ''}`}>
                        <span className="tier">
                            {quota.codingPlan.productName ||
                                `Coding Plan${quota.codingPlan.level ? ' ' + quota.codingPlan.level : ''}`}
                        </span>
                        {quota.codingPlan.validPeriod &&
                            <span className="valid-period" title={TIPS.validPeriod}>有效期至 {quota.codingPlan.validPeriod}</span>}
                    </div>
                    {/* 所有限额行放在同一个 grid 容器里，列宽跨行共享，5小时/周两列严格对齐 */}
                    <div className="quota-limits">
                        {quota.codingPlan.limits.map((l, i) => (
                            <React.Fragment key={`cp-${i}`}>
                                <span className="quota-item-name">{l.name}</span>
                                <span className="quota-limit-usage"><b>{fmtNum(l.used)}</b>
                                    <span className="num-dim"> / {fmtNum(l.limit)}</span></span>
                                <span className="quota-limit-pct" title={TIPS.usagePct}>
                                    <b className={pctClass(l.percent)}>{l.percent}%</b>
                                    <PctBar percent={l.percent} width={44}/>
                                </span>
                                <span className="reset-time">{l.nextReset > 0 ? `重置 ${fmtDate(l.nextReset)}` : ''}</span>
                            </React.Fragment>
                        ))}
                    </div>
                    {resetCardsRow}
                </>
            ) : resetCardsRow}
        </div>
    )
}

function ConfirmDialog({text, onOk, onCancel}) {
    return (
        <div className="modal-mask" onClick={onCancel}>
            <div className="modal modal-sm" onClick={e => e.stopPropagation()}>
                <p className="confirm-text">{text}</p>
                <div className="confirm-actions">
                    <button className="btn" onClick={onCancel}>取消</button>
                    <button className="btn btn-danger-solid" onClick={onOk}>确定</button>
                </div>
            </div>
        </div>
    )
}



// VersionModal 关于弹窗（数据来自后端内嵌的全局配置）
function VersionModal({info, onClose}) {
    return (
        <div className="modal-mask" onClick={onClose}>
            <div className="modal modal-sm" onClick={e => e.stopPropagation()}>
                <h3 className="help-title">关于 {info.displayName}</h3>
                <div className="version-row"><span>当前版本</span><b>v{info.version}</b></div>
                <div className="version-row"><span>项目地址</span><b>{info.repository.replace('https://', '')}</b></div>
                <div className="confirm-actions">
                    <button className="btn" onClick={() => BrowserOpenURL(info.repository)}>项目主页</button>
                    <button className="btn btn-primary" onClick={onClose}>关闭</button>
                </div>
            </div>
        </div>
    )
}

// UpdateModal 检查更新（打开即查询）
function UpdateModal({onClose}) {
    const [st, setSt] = useState({loading: true})
    useEffect(() => {
        CheckUpdate()
            .then(r => setSt({loading: false, result: r}))
            .catch(e => setSt({loading: false, error: String(e)}))
    }, [])
    const r = st.result
    return (
        <div className="modal-mask" onClick={onClose}>
            <div className="modal modal-sm" onClick={e => e.stopPropagation()}>
                <h3 className="help-title">检查更新</h3>
                {st.loading && <p className="hint">正在查询最新版本…</p>}
                {st.error && <p className="hint warn">检查失败：{st.error}</p>}
                {r && (
                    <>
                        <div className="version-row"><span>当前版本</span><b>v{r.current}</b></div>
                        <div className="version-row"><span>最新版本</span><b>v{r.latest}</b></div>
                        {r.hasUpdate
                            ? <p className="hint warn">发现新版本 v{r.latest}</p>
                            : <p className="hint">已是最新版本 ✓</p>}
                    </>
                )}
                <div className="confirm-actions">
                    {r?.hasUpdate &&
                        <button className="btn btn-primary" onClick={() => BrowserOpenURL(r.url)}>去下载</button>}
                    <button className="btn" onClick={onClose}>关闭</button>
                </div>
            </div>
        </div>
    )
}

// HelpModal 术语说明（tooltip 没注意到的用户从这里查）
function HelpModal({onClose}) {
    const terms = [
        ['渠道', '账号登录的官方渠道，如 BigModel Coding / z.ai Start Plan，抓取时自动识别'],
        ['ID', TIPS.id],
        ['🕐 抓取时间', '该账号快照首次保存的时间'],
        ['🔄 保鲜', '最近一次把该账号的最新登录态（刷新后的 token）写回快照的时间。越新鲜，切换过去越不容易遇到登录过期'],
        ['使用率', TIPS.usagePct],
        ['有效期至', TIPS.validPeriod],
        ['⚡ 5小时重置卡', TIPS.fiveHourCard],
        ['📅 周重置卡', TIPS.weekCard],
        ['会话过期', '该账号的 BigModel/z.ai 登录会话已在服务端过期，ZCode 里会提示「套餐查询失败」。写代码不受影响（用长效凭证）；点击账号上的「重新授权」重新走一次浏览器授权即可恢复，之后后台会定期保活以减少再次过期的概率'],
    ]
    return (
        <div className="modal-mask" onClick={onClose}>
            <div className="modal modal-sm" onClick={e => e.stopPropagation()}>
                <h3 className="help-title">字段说明</h3>
                <dl className="help-glossary">
                    {terms.map(([t, d]) => (
                        <div className="help-term" key={t}>
                            <dt>{t}</dt>
                            <dd>{d}</dd>
                        </div>
                    ))}
                </dl>
                <div className="confirm-actions">
                    <button className="btn" onClick={onClose}>关闭</button>
                </div>
            </div>
        </div>
    )
}

// indexHealth 把后端的健康状态列表转成按账号 id 索引的对象
function indexHealth(list) {
    const out = {}
    ;(list || []).forEach(r => {
        out[r.id] = r
    })
    return out
}

// reauthDoneMsg 重新授权完成的提示文案（是否刷新了当前登录态、是否重启了 ZCode）
function reauthDoneMsg(label, liveRefreshed, restarted) {
    if (liveRefreshed) {
        return `已重新授权 ${label}，当前登录态已刷新${restarted ? '，ZCode 已重启' : ''}`
    }
    return `已重新授权 ${label}，登录态已更新（下次切换生效）`
}

// AccountRow 一行一个账号：左侧信息（额度内容多少不定），操作按钮固定右上角。
// health: 会话健康报告（undefined=未检测）；reauthActive: 该行正处于重新授权等待回调状态
function AccountRow({meta, isCurrent, quotaState, onRefreshQuota, onAction, notify, askConfirm,
                         health, reauthActive, onStartReauth, onFinishReauth, onCancelReauth}) {
    const [busy, setBusy] = useState('')
    const [renaming, setRenaming] = useState(false)
    const [newName, setNewName] = useState(meta.label)
    const [callback, setCallback] = useState('')
    const expired = health?.status === 'expired'

    const run = async (name, fn) => {
        setBusy(name)
        try {
            await fn()
        } catch (e) {
            notify(String(e), 'error')
        } finally {
            setBusy('')
        }
    }

    return (
        <div className={`row ${isCurrent ? 'row-current' : ''}`}>
            <div className="row-main">
                <div className="row-title">
                    {renaming ? (
                        <input className="rename-input" value={newName}
                               onChange={e => setNewName(e.target.value)}
                               autoFocus onKeyDown={e => {
                            if (e.key === 'Enter') run('rename', async () => {
                                await RenameAccount(meta.id, newName)
                                setRenaming(false)
                                notify('已重命名')
                                onAction()
                            })
                            if (e.key === 'Escape') setRenaming(false)
                        }}/>
                    ) : (
                        <span className="row-label" onDoubleClick={() => setRenaming(true)}
                              title="双击重命名">{meta.label}</span>
                    )}
                    {isCurrent && <span className="badge-live">当前登录</span>}
                    {expired && <span className="badge-expired" title={TIPS.sessionExpired}>会话过期</span>}
                </div>
                <div className="row-meta">
                    <span className="meta-provider" title={TIPS.provider(meta.provider)}>{providerLabel(meta.provider)}</span>
                    <span className="meta-id" title={TIPS.id}>ID {meta.id}</span>
                    <span title={TIPS.captured(fmtDate(meta.capturedAt))}>🕐 {fmtShortDate(meta.capturedAt)}</span>
                    {meta.updatedAt > 0 &&
                        <span title={TIPS.fresh(fmtDate(meta.updatedAt))}>🔄 {fmtRelative(meta.updatedAt)}</span>}
                    {meta.note && <span className="meta-note" title={meta.note}>💬 {meta.note}</span>}
                </div>
                <QuotaView qs={quotaState}/>
                {reauthActive && (
                    <div className="reauth-panel">
                        <p className="hint">已打开浏览器等待授权，完成后会自动跳回并更新登录态。
                            请确认浏览器里登录的是「{meta.label}」，不是的话先在网页上切换登录。
                            若跳转到了 ZCode 客户端，请复制地址栏链接粘贴到下方：</p>
                        <textarea rows={2} value={callback} onChange={e => setCallback(e.target.value)}
                                  placeholder="zcode://oauth/callback?authCode=...&state=..."/>
                        <div className="reauth-actions">
                            <button className="btn btn-primary" disabled={!!busy || !callback.trim()}
                                    onClick={() => run('reauth', async () => {
                                        await onFinishReauth(callback)
                                        setCallback('')
                                    })}>
                                {busy === 'reauth' ? '正在授权…' : '完成授权'}
                            </button>
                            <button className="btn" disabled={!!busy} onClick={() => {
                                setCallback('')
                                onCancelReauth()
                            }}>取消</button>
                        </div>
                    </div>
                )}
            </div>
            <div className="row-actions">
                <button className="btn btn-primary" disabled={!!busy || isCurrent}
                        onClick={() => {
                            const doUse = () => run('use', async () => {
                                const r = await Use(meta.id)
                                notify(`已切换到 ${r.label}${r.restarted ? '，ZCode 已重启' : ''}`)
                                onAction()
                            })
                            if (expired) {
                                askConfirm(`账号「${meta.label}」的登录会话已过期，切换后 ZCode 的套餐/用量查询会失败（写代码不受影响）。仍要切换吗？`, doUse)
                            } else {
                                doUse()
                            }
                        }}>
                    {busy === 'use' ? '切换中…' : '切换'}
                </button>
                <button className="btn" disabled={!!busy || quotaState?.loading}
                        onClick={() => onRefreshQuota(meta.id)}>
                    {quotaState?.loading ? '查询中…' : '刷新额度'}
                </button>
                {expired && !reauthActive &&
                    <button className="btn btn-warn" disabled={!!busy}
                            title="该账号登录会话已过期，重新走一次浏览器授权即可恢复"
                            onClick={() => run('reauth-start', async () => {
                                await onStartReauth(meta.id)
                            })}>重新授权</button>}
                <button className="btn" disabled={!!busy} onClick={() => setRenaming(true)}>改名</button>
                <button className="btn btn-danger" disabled={!!busy}
                        onClick={() => askConfirm(`确定删除账号「${meta.label}」的快照？`,
                            () => run('del', async () => {
                                await DeleteAccount(meta.id)
                                notify('已删除')
                                onAction()
                            }))}>删除</button>
            </div>
        </div>
    )
}

// CurrentGuestRow 当前登录但未保存的账号（置顶显示，可一键保存入库）
function CurrentGuestRow({current, quotaState, onRefreshQuota, onSave}) {
    return (
        <div className="row row-current row-guest">
            <div className="row-main">
                <div className="row-title">
                    <span className="row-label">{current.label}</span>
                    <span className="badge-live">当前登录</span>
                    <span className="badge-unsaved">未保存</span>
                </div>
                <div className="row-meta">
                    <span className="meta-provider" title={TIPS.provider(current.provider)}>{providerLabel(current.provider)}</span>
                    <span className="meta-id" title={TIPS.id}>ID {current.shortId}</span>
                </div>
                <QuotaView qs={quotaState}/>
            </div>
            <div className="row-actions">
                <button className="btn btn-primary" onClick={onSave}>保存入库</button>
                <button className="btn" disabled={quotaState?.loading}
                        onClick={() => onRefreshQuota('current')}>
                    {quotaState?.loading ? '查询中…' : '刷新额度'}
                </button>
            </div>
        </div>
    )
}

function AddAccountModal({onClose, onDone, notify}) {
    const [tab, setTab] = useState('capture')
    const [provider, setProvider] = useState('zai')
    const [name, setName] = useState('')
    const [note, setNote] = useState('')
    const [busy, setBusy] = useState(false)
    const [oauthOpened, setOauthOpened] = useState(false)
    const [callback, setCallback] = useState('')
    const [existLabel, setExistLabel] = useState('')

    // 监听浏览器回调自动完成事件（协议接管成功时，无需任何手动操作）
    useEffect(() => {
        const offDone = EventsOn('oauth:done', (r) => {
            if (r?.reauth) return // 重新授权流程的事件由主界面处理
            notify(r.created
                ? `已添加账号 ${r.label}${r.billingReady ? '' : '（额度初始化中，稍后查询）'}`
                : `该账号已存在（${r.label}）`)
            onDone()
        })
        const offErr = EventsOn('oauth:error', (msg) => notify(String(msg), 'error'))
        return () => {
            offDone()
            offErr()
        }
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [])

    const doCapture = async (overwrite) => {
        setBusy(true)
        try {
            const r = await Capture(name, note, overwrite)
            if (!r.created && !overwrite) {
                setExistLabel(r.meta.label)
                return
            }
            notify(overwrite ? `已用当前登录态覆盖更新 ${r.meta.label}` : `已保存账号 ${r.meta.label}`)
            onDone()
        } catch (e) {
            notify(String(e), 'error')
        } finally {
            setBusy(false)
        }
    }

    const doFinishOAuth = async () => {
        setBusy(true)
        try {
            const r = await FinishOAuth(callback, name, note)
            if (!r.created) {
                notify(`该账号已存在（${r.meta.label}）`)
            } else {
                notify(`已添加账号 ${r.meta.label}${r.billingReady ? '' : '（额度初始化中，稍后查询）'}`)
            }
            onDone()
        } catch (e) {
            notify(String(e), 'error')
        } finally {
            setBusy(false)
        }
    }

    return (
        <div className="modal-mask" onClick={onClose}>
            <div className="modal" onClick={e => e.stopPropagation()}>
                <div className="modal-tabs">
                    <button className={tab === 'capture' ? 'tab active' : 'tab'}
                            onClick={() => setTab('capture')}>抓取当前登录</button>
                    <button className={tab === 'oauth' ? 'tab active' : 'tab'}
                            onClick={() => setTab('oauth')}>OAuth 登录新账号</button>
                </div>
                <div className="field">
                    <label>名称（留空用邮箱/手机号）</label>
                    <input value={name} onChange={e => setName(e.target.value)} placeholder="例如：主账号"/>
                </div>
                <div className="field">
                    <label>备注</label>
                    <input value={note} onChange={e => setNote(e.target.value)} placeholder="可选"/>
                </div>
                {tab === 'capture' ? (
                    <>
                        <p className="hint">把 ZCode 客户端当前登录的账号保存为快照（只保存账号字段，不影响其他设置）。</p>
                        {existLabel ? (
                            <>
                                <p className="hint warn">该账号已存在（{existLabel}）。是否用当前登录态覆盖更新？</p>
                                <button className="btn btn-primary btn-block" disabled={busy}
                                        onClick={() => doCapture(true)}>
                                    {busy ? '更新中…' : '覆盖更新为当前登录'}</button>
                            </>
                        ) : (
                            <button className="btn btn-primary btn-block" disabled={busy} onClick={() => doCapture(false)}>
                                {busy ? '保存中…' : '保存当前登录'}</button>
                        )}
                    </>
                ) : (
                    <>
                        <div className="field">
                            <label>登录渠道</label>
                            <div className="provider-choices">
                                <button className={provider === 'zai' ? 'choice active' : 'choice'}
                                        onClick={() => setProvider('zai')}>z.ai（全球）</button>
                                <button className={provider === 'bigmodel' ? 'choice active' : 'choice'}
                                        onClick={() => setProvider('bigmodel')}>BigModel（中国）</button>
                            </div>
                        </div>
                        <p className="hint">点击打开浏览器登录。登录完成后浏览器会跳回本工具<b>自动完成</b>；
                            如果弹出「打开 ZCode」说明协议被官方客户端占用，点「取消」并复制地址栏链接粘贴到下方即可。</p>
                        <button className="btn btn-block" disabled={busy} onClick={async () => {
                            setBusy(true)
                            try {
                                await StartOAuth(provider, name, note)
                                setOauthOpened(true)
                            } catch (e) {
                                notify(String(e), 'error')
                            } finally {
                                setBusy(false)
                            }
                        }}>打开浏览器登录</button>
                        {oauthOpened && (
                            <>
                                <div className="field">
                                    <label>回调链接（zcode://…callback?code=… 或 authCode=…）</label>
                                    <textarea rows={3} value={callback}
                                              onChange={e => setCallback(e.target.value)}
                                              placeholder="zcode://zai-auth/callback?code=...&state=..."/>
                                </div>
                                <button className="btn btn-primary btn-block" disabled={busy || !callback.trim()}
                                        onClick={doFinishOAuth}>
                                    {busy ? '正在登录…（约20秒）' : '完成登录并保存'}</button>
                                <p className="hint">如果刚才已经跳转到了 ZCode 客户端但账号没变化，那是因为登录是从本工具发起的、
                                    ZCode 校验不通过。请在 ZCode 客户端里自行退出并登录新账号，成功后点下方按钮：</p>
                                <button className="btn btn-block" disabled={busy}
                                        onClick={() => doCapture(true)}>
                                    从 ZCode 抓取当前登录入库</button>
                            </>
                        )}
                    </>
                )}
                <button className="btn btn-block btn-ghost" onClick={() => {
                    CancelOAuth()
                    onClose()
                }}>取消</button>
            </div>
        </div>
    )
}

export default function App() {
    const [state, setState] = useState(null)
    const [showAdd, setShowAdd] = useState(false)
    const [showHelp, setShowHelp] = useState(false)
    const [showVersion, setShowVersion] = useState(false)
    const [showUpdate, setShowUpdate] = useState(false)
    const [info, setInfo] = useState(null) // 全局配置（名称/版本/仓库）
    const [toast, setToast] = useState(null)
    const [quotas, setQuotas] = useState({}) // key: 'current' 或账号 id → {loading, data, error}
    const [confirm, setConfirm] = useState(null) // {text, action}
    const [health, setHealth] = useState({}) // 账号 id → {status, checkedAt}
    const [reauthID, setReauthID] = useState(null) // 正在重新授权的账号 id
    // oauth:error 事件监听注册一次，用 ref 读最新的 reauthID（避免闭包旧值）
    const reauthRef = useRef(null)
    reauthRef.current = reauthID

    const notify = useCallback((msg, type = 'ok') => {
        setToast({msg, type})
        setTimeout(() => setToast(null), 4000)
    }, [])

    const askConfirm = useCallback((text, action) => setConfirm({text, action}), [])

    // 查询额度（静默：失败只显示在行内，不弹 toast）
    const loadQuota = useCallback(async (key) => {
        setQuotas(q => ({...q, [key]: {loading: true, data: q[key]?.data, error: null}}))
        try {
            const data = await GetQuota(key === 'current' ? '' : key)
            setQuotas(q => ({...q, [key]: {loading: false, data}}))
        } catch (e) {
            setQuotas(q => ({...q, [key]: {loading: false, error: String(e)}}))
        }
    }, [])

    const refresh = useCallback(async () => {
        try {
            const s = await GetState()
            setState(s)
            // 自动加载额度
            if (s.current) loadQuota('current')
            ;(s.accounts || []).forEach(a => loadQuota(a.id))
        } catch (e) {
            notify(String(e), 'error')
        }
    }, [loadQuota, notify])

    // ===== 重新授权流程 =====
    const startReauth = useCallback(async (id) => {
        await StartReOAuth(id)
        setReauthID(id)
        notify('已打开浏览器，完成授权后会自动更新登录态')
    }, [notify])

    const finishReauth = useCallback(async (input) => {
        const r = await FinishReOAuth(input) // 失败抛错（身份不一致等），保留等待状态让用户重试
        setReauthID(null)
        notify(reauthDoneMsg(r.meta?.label || '账号', r.liveRefreshed, r.zcodeRestarted))
        refresh()
    }, [notify, refresh])

    const cancelReauth = useCallback(async () => {
        try {
            await CancelOAuth()
        } catch (e) { /* 忽略 */ }
        setReauthID(null)
    }, [])

    useEffect(() => {
        refresh()
        GetAppInfo().then(setInfo).catch(() => {})
        // 会话健康：先显示缓存，再后台探测刷新（结果也由保活巡检事件推送）
        GetHealth().then(l => setHealth(indexHealth(l))).catch(() => {})
        CheckHealthNow().then(l => setHealth(indexHealth(l))).catch(() => {})
        // 菜单栏「帮助 → 检查更新 / 关于」（非 macOS 平台有「关于」项）
        const offUpdate = EventsOn('menu:check-update', () => setShowUpdate(true))
        const offAbout = EventsOn('menu:about', () => setShowVersion(true))
        const offHealth = EventsOn('health:updated', l => setHealth(indexHealth(l)))
        // 重新授权协议回调自动完成（r.reauth 标记区分于添加账号）
        const offOauthDone = EventsOn('oauth:done', r => {
            if (!r?.reauth) return
            setReauthID(null)
            notify(reauthDoneMsg(r.label, r.liveRefreshed, r.zcodeRestarted))
            refresh()
        })
        const offOauthErr = EventsOn('oauth:error', msg => {
            if (reauthRef.current != null) notify(String(msg), 'error') // 添加账号弹窗有自己的错误提示
        })
        return () => {
            offUpdate()
            offAbout()
            offHealth()
            offOauthDone()
            offOauthErr()
        }
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [refresh])

    const currentId = state?.current?.savedId || null
    const showGuestRow = state?.current && !currentId

    return (
        <div className="container">
            <header className="topbar">
                <h1>ZCode 账号切换</h1>
                <span className={state?.running ? 'status on' : 'status off'}>
                    {state?.running ? '● ZCode 运行中' : '○ ZCode 已关闭'}
                </span>
                <div className="spacer"/>
                {state?.hasLastBackup &&
                    <button className="btn" onClick={() => askConfirm('回滚到切换前的登录态？', async () => {
                        try {
                            await Rollback()
                            notify('已回滚')
                            refresh()
                        } catch (e) {
                            notify(String(e), 'error')
                        }
                    })}>回滚</button>}
                <button className="btn" onClick={refresh}>刷新</button>
                <button className="btn btn-primary" onClick={() => setShowAdd(true)}>添加账号</button>
                <button className="btn btn-help" title="字段说明" onClick={() => setShowHelp(true)}>ⓘ</button>
            </header>

            <main className="list">
                {showGuestRow &&
                    <CurrentGuestRow current={state.current}
                                     quotaState={quotas['current']}
                                     onRefreshQuota={loadQuota}
                                     onSave={() => setShowAdd(true)}/>}
                {state?.accounts?.map(m => (
                    <AccountRow key={m.id} meta={m}
                                isCurrent={currentId === m.id}
                                quotaState={quotas[m.id]}
                                onRefreshQuota={loadQuota}
                                onAction={refresh} notify={notify} askConfirm={askConfirm}
                                health={health[m.id]}
                                reauthActive={reauthID === m.id}
                                onStartReauth={startReauth}
                                onFinishReauth={finishReauth}
                                onCancelReauth={cancelReauth}/>
                ))}
                {!showGuestRow && state?.accounts?.length === 0 &&
                    <p className="empty">还没有保存的账号。点击右上角「添加账号」，或在 ZCode 登录后使用「抓取当前登录」。</p>}
            </main>

            {showHelp && <HelpModal onClose={() => setShowHelp(false)}/>}
            {showVersion && info && <VersionModal info={info} onClose={() => setShowVersion(false)}/>}
            {showUpdate && <UpdateModal onClose={() => setShowUpdate(false)}/>}
            {showAdd && <AddAccountModal onClose={() => setShowAdd(false)}
                                         onDone={() => {
                                             setShowAdd(false)
                                             refresh()
                                         }}
                                         notify={notify}/>}
            {confirm && <ConfirmDialog text={confirm.text}
                                       onOk={() => {
                                           const act = confirm.action
                                           setConfirm(null)
                                           act()
                                       }}
                                       onCancel={() => setConfirm(null)}/>}
            {toast && <div className={`toast toast-${toast.type}`}>{toast.msg}</div>}
        </div>
    )
}
