import React, {useCallback, useEffect, useState} from 'react'
import {
    Capture, DeleteAccount, FinishOAuth, GetQuota, GetState,
    RenameAccount, Rollback, StartOAuth, CancelOAuth, Use,
} from '../wailsjs/go/main/App'
import {EventsOn} from '../wailsjs/runtime/runtime'

function fmtNum(v) {
    if (v === null || v === undefined) return '未知'
    return Number.isInteger(v) ? v.toLocaleString('zh-CN') : v.toFixed(2)
}

function fmtDate(ms) {
    if (!ms) return ''
    const d = new Date(ms)
    const p = n => String(n).padStart(2, '0')
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

// QuotaView 展示一个账号的额度状态（自动加载 + 手动刷新）
function QuotaView({qs}) {
    if (!qs) return null
    if (qs.loading && !qs.data) return <div className="quota-panel">查询中…</div>
    if (qs.error) return <div className="quota-panel quota-error">{qs.error}</div>
    const quota = qs.data
    if (!quota) return null
    return (
        <div className="quota-panel">
            <div className="quota-row">
                {quota.planTier && <span className="tier">{quota.planTier.label}</span>}
                <span>总额 {fmtNum(quota.total)}</span>
                <span>已用 {fmtNum(quota.used)}</span>
                <span className="remaining">剩余 {fmtNum(quota.remaining)}</span>
                {quota.percentUsed !== null && quota.percentUsed !== undefined &&
                    <span>使用率 {quota.percentUsed.toFixed(1)}%</span>}
            </div>
            {quota.items && quota.items.map((it, i) => (
                <div className="quota-item" key={i}>
                    <span className="quota-item-name">{it.name}</span>
                    <span>剩 {fmtNum(it.remaining)} / {fmtNum(it.total)}</span>
                </div>
            ))}
            {quota.codingPlan && (
                <>
                    <div className="quota-row quota-coding-plan">
                        <span className="tier">
                            {quota.codingPlan.productName ||
                                `Coding Plan${quota.codingPlan.level ? ' ' + quota.codingPlan.level : ''}`}
                        </span>
                        {quota.codingPlan.validPeriod &&
                            <span className="valid-period">有效期 {quota.codingPlan.validPeriod}</span>}
                    </div>
                    {quota.codingPlan.limits.map((l, i) => (
                        <div className="quota-item" key={`cp-${i}`}>
                            <span className="quota-item-name">{l.name}</span>
                            <span>已用 {fmtNum(l.used)} / {fmtNum(l.limit)}（{l.percent}%）</span>
                        </div>
                    ))}
                </>
            )}
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

// AccountRow 一行一个账号：左侧信息（额度内容多少不定），操作按钮固定右上角
function AccountRow({meta, isCurrent, quotaState, onRefreshQuota, onAction, notify, askConfirm}) {
    const [busy, setBusy] = useState('')
    const [renaming, setRenaming] = useState(false)
    const [newName, setNewName] = useState(meta.label)

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
                </div>
                <div className="row-meta">
                    <span>{meta.id}</span><span className="dot">·</span><span>{meta.provider}</span>
                    <span className="dot">·</span><span>{fmtDate(meta.capturedAt)}</span>
                    {meta.updatedAt > 0 && <><span className="dot">·</span><span>保鲜 {fmtDate(meta.updatedAt)}</span></>}
                    {meta.note && <><span className="dot">·</span><span>{meta.note}</span></>}
                </div>
                <QuotaView qs={quotaState}/>
            </div>
            <div className="row-actions">
                <button className="btn btn-primary" disabled={!!busy || isCurrent}
                        onClick={() => run('use', async () => {
                            const r = await Use(meta.id)
                            notify(`已切换到 ${r.label}${r.restarted ? '，ZCode 已重启' : ''}`)
                            onAction()
                        })}>
                    {busy === 'use' ? '切换中…' : '切换'}
                </button>
                <button className="btn" disabled={!!busy || quotaState?.loading}
                        onClick={() => onRefreshQuota(meta.id)}>
                    {quotaState?.loading ? '查询中…' : '刷新额度'}
                </button>
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
                    <span>{current.provider}</span><span className="dot">·</span>
                    <span>uid {current.shortId}</span>
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
    const [toast, setToast] = useState(null)
    const [quotas, setQuotas] = useState({}) // key: 'current' 或账号 id → {loading, data, error}
    const [confirm, setConfirm] = useState(null) // {text, action}

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

    useEffect(() => {
        refresh()
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
                                onAction={refresh} notify={notify} askConfirm={askConfirm}/>
                ))}
                {!showGuestRow && state?.accounts?.length === 0 &&
                    <p className="empty">还没有保存的账号。点击右上角「添加账号」，或在 ZCode 登录后使用「抓取当前登录」。</p>}
            </main>

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
