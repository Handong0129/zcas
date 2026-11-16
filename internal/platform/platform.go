// Package platform 平台相关能力：进程管理、URL 打开、协议注册、版本读取。
// 各平台实现在 platform_darwin / platform_windows / platform_unsupported。
package platform

import "strings"

// VersionAtLeast 语义化版本比较：v 的版本号是否 ≥ min（按点分段数值比较，
// 非数字前缀按 0 处理）。解析失败返回 false（保守）。
func VersionAtLeast(v, min string) bool {
	parse := func(s string) []int {
		var out []int
		for _, part := range strings.Split(strings.TrimSpace(s), ".") {
			n := 0
			for _, r := range part {
				if r < '0' || r > '9' {
					break
				}
				n = n*10 + int(r-'0')
			}
			out = append(out, n)
		}
		return out
	}
	a, b := parse(v), parse(min)
	if len(a) == 0 {
		return false
	}
	for i := 0; i < len(a) || i < len(b); i++ {
		av, bv := 0, 0
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av != bv {
			return av > bv
		}
	}
	return true
}

// ZCodeSupportsZaiCN 本机 ZCode 是否支持 zai_cn（zchat 登录）渠道的账号。
// 旧版客户端（<3.18.2）没有 zai_cn 概念，切换/使用该渠道账号会掉登录页。
// ZCode 未安装或版本不可读时返回 false——调用方需按自身语义决定提示方式。
func ZCodeSupportsZaiCN() bool {
	return VersionAtLeast(ZCodeVersion(), "3.18.2")
}
