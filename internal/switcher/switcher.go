// Package switcher 切换编排：关闭 ZCode → 备份 .last → 回写保鲜 → 应用切片 → 重启。
package switcher

import (
	"fmt"
	"time"

	"zcas/internal/platform"
	"zcas/internal/store"
)

const killTimeout = 8 * time.Second

type Result struct {
	Account      *store.Meta
	WriteBackID  string // 切换离开时保鲜回写的账号 id（空=跳过）
	WriteBackErr error  // 保鲜失败不阻断切换，仅提示
	Restarted    bool
}

// Use 切换到指定账号。restart=false 时只换文件不启动 ZCode。
func Use(id string, restart bool) (*Result, error) {
	meta, snap, err := store.Load(id)
	if err != nil {
		return nil, err
	}
	if len(snap.CredentialFields) == 0 {
		return nil, fmt.Errorf("账号 %s 的快照缺少登录态字段，无法切换", id)
	}

	// 1. 切换前必须关闭 ZCode：运行中改登录态文件不可靠（客户端会回写覆盖）
	if platform.IsZCodeRunning() {
		if err := platform.KillZCode(killTimeout); err != nil {
			return nil, err
		}
	}

	// 2. 整文件备份当前现场到 .last（回滚保险）
	if err := store.BackupLast(); err != nil {
		return nil, fmt.Errorf("备份当前登录态失败: %w", err)
	}

	// 3. 把当前账号的实时登录态回写到它自己的快照（保鲜，防 token 搁浅）
	wbID, wbErr := store.WriteBackCurrent()

	// 4. 合并切片并原子写回；失败自动用 .last 恢复
	cred, cfg, err := store.ReadLive()
	if err != nil {
		cred, cfg = map[string]any{}, map[string]any{} // 从未登录过也能切换
	}
	newCred, newCfg := store.Apply(snap, cred, cfg)
	if err := store.WriteLive(newCred, newCfg); err != nil {
		_ = store.RestoreLast()
		return nil, fmt.Errorf("写入登录态失败，已自动回滚: %w", err)
	}

	// 5. 重启
	res := &Result{Account: meta, WriteBackID: wbID, WriteBackErr: wbErr}
	if restart {
		if err := platform.LaunchZCode(); err != nil {
			res.WriteBackErr = fmt.Errorf("启动 ZCode 失败（登录态已切换）: %w", err)
		} else {
			res.Restarted = true
		}
	}
	return res, nil
}

// Rollback 回滚到切换前的登录态（.last）。
func Rollback(restart bool) error {
	if !store.HasLast() {
		return fmt.Errorf("没有可回滚的备份（.last 不存在）")
	}
	if platform.IsZCodeRunning() {
		if err := platform.KillZCode(killTimeout); err != nil {
			return err
		}
	}
	if err := store.RestoreLast(); err != nil {
		return fmt.Errorf("回滚失败: %w", err)
	}
	if restart {
		_ = platform.LaunchZCode()
	}
	return nil
}
