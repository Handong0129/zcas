package main

import (
	"sync"
	"time"

	"zcas/internal/quota"
)

// quotaOpenFetchGap 菜单展开触发查询的最小间隔：打开托盘菜单（用户动作，天然低频）
// 几乎总会真正发起补查，15 秒内连开不重复查；结果落到共享存储，再次打开（几秒后）即新数据。
// 注意结果供「下次展开」用——菜单开着时不能热替换内容（AppKit 会取消展示）。
const quotaOpenFetchGap = 15 * time.Second

// quotaWarmFetchGap 后台巡检的补查间隔：没人点菜单时由每分钟 ticker 按此节奏保活
// （5 小时窗口会随时间自然回落，闲置时数据也会变旧）。菜单展开不能等网络（必须秒开），
// 新鲜度模型是「展示上次结果 + 展开即补查」。
const quotaWarmFetchGap = 2 * time.Minute

// quotaEntry 共享存储条目：ov=最近一次成功查询结果，lastTry=最近一次尝试时间（含失败）。
type quotaEntry struct {
	ov      *quota.Overview
	lastTry time.Time
}

// quotaStore 额度共享存储：主界面与托盘菜单的唯一数据源。任何一次成功查询
// （主界面刷新 / 托盘补查 / 切换账号后的刷新）都回写到这里，托盘菜单构建只读它——
// 两边数字必然一致，不存在「托盘一份缓存、主界面一份现场值」的对不上问题。
// 曾经托盘私有一份 10 分钟 TTL 缓存、主界面各自现场查，两边互不相通导致数字可对不上。
type quotaStore struct {
	mu sync.Mutex
	m  map[string]quotaEntry
}

func newQuotaStore() *quotaStore { return &quotaStore{m: map[string]quotaEntry{}} }

// get 某账号最近一次成功查询结果；从未查到返回 nil。
func (s *quotaStore) get(id string) *quota.Overview {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[id].ov
}

// snapshot 全量只读拷贝（主界面聚焦时轻量同步用，键=账号 id）。
func (s *quotaStore) snapshot() map[string]*quota.Overview {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*quota.Overview, len(s.m))
	for id, e := range s.m {
		if e.ov != nil {
			out[id] = e.ov
		}
	}
	return out
}

// put 回写一次查询结果。ov=nil 表示失败：保留旧数据、只记尝试时间——
// 节流照常生效（下个周期自动重试），已有数字不会被清掉。
func (s *quotaStore) put(id string, ov *quota.Overview) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.m[id]
	if ov != nil {
		e.ov = ov
	}
	e.lastTry = time.Now()
	s.m[id] = e
}

// due 返回 ids 中「超过 gap 没查过」的子集（含从未查过的）。
func (s *quotaStore) due(ids []string, gap time.Duration) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, id := range ids {
		if time.Since(s.m[id].lastTry) > gap {
			out = append(out, id)
		}
	}
	return out
}

// forget 删除账号时清理对应条目，避免残留。
func (s *quotaStore) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}
