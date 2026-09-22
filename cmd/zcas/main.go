// zcas — ZCode 账号无感切换工具（macOS CLI）
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"zcas/internal/auth"
	"zcas/internal/config"
	"zcas/internal/fingerprint"
	"zcas/internal/platform"
	"zcas/internal/quota"
	"zcas/internal/store"
	"zcas/internal/switcher"
	"zcas/internal/zcrypto"
)

const usage = `zcas — ZCode 账号无感切换工具（macOS）

用法:
  zcas                     当前登录账号 + 已保存账号列表（默认命令）
  zcas ls                  列出所有已保存账号
  zcas capture [--name 名称] [--note 备注] [--overwrite]
                           把当前 ZCode 登录态存为账号快照
  zcas add [--name 名称] [--provider zai|bigmodel]
                           浏览器 OAuth 添加新账号（zai=全球，bigmodel=中国；不影响当前登录）
  zcas use <序号|id|前缀> [--no-restart]
                           切换账号（自动关闭并重启 ZCode）
  zcas rm <序号|id>        删除账号快照
  zcas rename <序号|id> <新名称>
  zcas rollback [--no-restart]
                           回滚到切换前的登录态
  zcas quota [序号|id]     查询额度（不带参数查当前登录账号）
  zcas kill | launch       手动关闭 / 启动 ZCode
`

func main() {
	if len(os.Args) < 2 {
		cmdStatus()
		return
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "status", "ls", "list":
		cmdStatus()
	case "capture":
		err = cmdCapture(args)
	case "add":
		err = cmdAdd(args)
	case "use":
		err = cmdUse(args)
	case "rm", "delete":
		err = cmdRm(args)
	case "rename":
		err = cmdRename(args)
	case "rollback":
		err = cmdRollback(args)
	case "quota":
		err = cmdQuota(args)
	case "kill":
		err = platform.KillZCode(8 * time.Second)
	case "launch":
		err = platform.LaunchZCode()
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "✗", err)
		os.Exit(1)
	}
}

// ===== status / ls =====

func cmdStatus() {
	secret := zcrypto.DefaultSecret()
	cred, cfg, err := store.ReadLive()
	if err != nil || !store.HasSession(cred) {
		fmt.Println("当前登录: （未登录或登录态不可读）")
	} else if fp := fingerprint.Extract(cred, cfg, secret); fp == nil {
		fmt.Println("当前登录: （无法识别账号指纹）")
	} else {
		saved := ""
		if store.Exists(fp.EmailShortID) {
			saved = "  [已保存]"
		}
		running := ""
		if platform.IsZCodeRunning() {
			running = "  ●运行中"
		}
		fmt.Printf("当前登录: %s  provider=%s  uid=%s%s%s\n", fp.Label, fp.Provider, fp.ShortID, saved, running)
	}
	fmt.Println()
	printAccounts()
}

func printAccounts() {
	metas, err := store.List()
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取账号列表失败:", err)
		return
	}
	if len(metas) == 0 {
		fmt.Println("（还没有保存的账号。用 zcas capture 保存当前登录，或 zcas add 添加新账号）")
		return
	}
	fmt.Printf("%-3s %-13s %-24s %-10s %-17s %s\n", "#", "ID", "名称", "PROVIDER", "保存时间", "备注")
	for i, m := range metas {
		fmt.Printf("%-3d %-13s %-24s %-10s %-17s %s\n",
			i+1, m.ID, trunc(m.Label, 24), m.Provider,
			time.UnixMilli(m.CapturedAt).Format("2006-01-02 15:04"), m.Note)
	}
}

// ===== capture =====

func cmdCapture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	name := fs.String("name", "", "账号名称（默认用邮箱）")
	note := fs.String("note", "", "备注")
	overwrite := fs.Bool("overwrite", false, "同账号已存在时覆盖")
	if err := fs.Parse(args); err != nil {
		return err
	}
	meta, created, err := store.Capture(*name, *note, *overwrite)
	if err != nil {
		return err
	}
	if !created {
		fmt.Printf("○ 该账号已存在（%s），加 --overwrite 覆盖\n", meta.Label)
		return nil
	}
	fmt.Printf("✓ 已保存账号 %s（%s, provider=%s）\n", meta.Label, meta.ID, meta.Provider)
	return nil
}

// ===== add（OAuth）=====

func cmdAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	name := fs.String("name", "", "账号名称（默认用邮箱/手机号）")
	note := fs.String("note", "", "备注")
	provider := fs.String("provider", "zai", "登录渠道: zai(全球) 或 bigmodel(中国)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := auth.ParseProvider(*provider)
	if err != nil {
		return err
	}

	state, err := auth.NewState()
	if err != nil {
		return err
	}
	authURL := auth.BuildAuthorizeURL(p, state)
	fmt.Printf("正在打开系统浏览器，请登录 %s 账号…\n", p.DisplayName)
	if err := platform.OpenURL(authURL); err != nil {
		fmt.Println("打开浏览器失败，请手动访问：")
		fmt.Println(authURL)
	}
	fmt.Println()
	fmt.Println("登录完成后浏览器会尝试跳转 zcode:// 回调：")
	fmt.Println("  - 若弹出「打开 ZCode」——可以允许，让官方客户端完成登录，然后回来运行 zcas capture；")
	fmt.Println("  - 或者复制浏览器地址栏中完整的 zcode:// 链接，粘贴到这里直接入库（不影响当前登录）。")
	fmt.Print("\n请粘贴 zcode:// 回调链接（或 code 值）: ")

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("读取输入失败: %w", err)
	}
	code, cbState, err := auth.ParseCallbackInput(line)
	if err != nil {
		return err
	}
	if cbState != "" && cbState != state {
		return fmt.Errorf("回调 state 不匹配（可能粘贴了旧的链接），请重试")
	}

	fmt.Println("正在换取 token…")
	ts, err := auth.ExchangeCode(p, code, state)
	if err != nil {
		return err
	}
	fmt.Println("正在保存账号并初始化额度（约 20s）…")
	meta, created, billingReady, err := auth.AddAccount(p, ts, *name, *note, config.Load())
	if err != nil {
		return err
	}
	if !created {
		fmt.Printf("○ 该账号已存在（%s）\n", meta.Label)
		return nil
	}
	fmt.Printf("✓ 已添加账号 %s（%s）\n", meta.Label, meta.ID)
	if !billingReady {
		fmt.Println("⚠ 额度初始化尚未就绪（服务端异步处理），稍后 zcas quota 查询会自动重试")
	}
	return nil
}

// ===== use =====

func cmdUse(args []string) error {
	fs := flag.NewFlagSet("use", flag.ContinueOnError)
	noRestart := fs.Bool("no-restart", false, "只换登录态文件，不启动 ZCode")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("用法: zcas use <序号|id|前缀> [--no-restart]")
	}
	id, err := store.Resolve(fs.Arg(0))
	if err != nil {
		return err
	}
	res, err := switcher.Use(id, !*noRestart)
	if err != nil {
		return err
	}
	fmt.Printf("✓ 已切换到 %s（%s）\n", res.Account.Label, res.Account.ID)
	if res.WriteBackID != "" {
		fmt.Printf("  已将原账号 %s 的最新登录态回写到它的快照\n", res.WriteBackID)
	}
	if res.WriteBackErr != nil {
		fmt.Println("  ⚠", res.WriteBackErr)
	}
	if res.Restarted {
		fmt.Println("  ZCode 已重启")
	} else if !*noRestart {
		fmt.Println("  ⚠ ZCode 未自动启动，请手动打开")
	}
	return nil
}

// ===== rm / rename =====

func cmdRm(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("用法: zcas rm <序号|id|前缀>")
	}
	id, err := store.Resolve(args[0])
	if err != nil {
		return err
	}
	if err := store.Delete(id); err != nil {
		return err
	}
	fmt.Println("✓ 已删除账号", id)
	return nil
}

func cmdRename(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("用法: zcas rename <序号|id|前缀> <新名称>")
	}
	id, err := store.Resolve(args[0])
	if err != nil {
		return err
	}
	meta, err := store.Rename(id, args[1], "")
	if err != nil {
		return err
	}
	fmt.Println("✓ 已重命名为", meta.Label)
	return nil
}

// ===== rollback =====

func cmdRollback(args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	noRestart := fs.Bool("no-restart", false, "只恢复文件，不启动 ZCode")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := switcher.Rollback(!*noRestart); err != nil {
		return err
	}
	fmt.Println("✓ 已回滚到切换前的登录态")
	return nil
}

// ===== quota =====

func cmdQuota(args []string) error {
	cfg := config.Load()
	secret := zcrypto.DefaultSecret()
	raw := false
	var rest []string
	for _, a := range args {
		if a == "--raw" {
			raw = true
		} else {
			rest = append(rest, a)
		}
	}
	args = rest
	var tokens []string
	var cred map[string]any
	var whom string
	if len(args) == 0 {
		var err error
		tokens, err = quota.CurrentTokens(secret)
		if err != nil {
			return err
		}
		cred, _, _ = store.ReadLive()
		whom = "当前账号"
	} else {
		id, err := store.Resolve(args[0])
		if err != nil {
			return err
		}
		meta, snap, err := store.Load(id)
		if err != nil {
			return err
		}
		tokens = quota.TokensFromSnapshot(snap, secret)
		cred = quota.CredMapFromSnapshot(snap)
		whom = meta.Label
	}
	if raw {
		b, err := quota.RawJSON(tokens, cfg)
		if err != nil {
			return err
		}
		fmt.Printf("账号: %s\n%s\n", whom, b)
		return nil
	}
	ov, err := quota.ForTokens(tokens, cfg)
	if err != nil {
		return err
	}
	fmt.Printf("账号: %s\n", whom)
	if ov.PlanTier != nil {
		fmt.Printf("套餐: %s\n", ov.PlanTier.Label)
	}
	fmt.Printf("总额: %s   已用: %s   剩余: %s   使用率: %s\n",
		human(ov.Total), human(ov.Used), human(ov.Remaining), percent(ov.PercentUsed))
	if ov.IsEmpty {
		fmt.Println("（该账号暂无套餐数据，可能是新账号初始化延迟，稍后重试）")
	}
	for _, it := range ov.Items {
		fmt.Printf("  - %-24s 总 %s / 已用 %s / 剩余 %s（%s）\n",
			it.Name, human(it.Total), human(it.Used), human(it.Remaining), percent(it.PercentUsed))
	}
	quota.EnrichCodingPlan(ov, cred, secret)
	if cp := ov.CodingPlan; cp != nil {
		label := cp.ProductName
		if label == "" {
			label = "Coding Plan"
			if cp.Level != "" {
				label += " " + cp.Level
			}
		}
		fmt.Printf("Coding Plan（%s）: %s\n", cp.Provider, label)
		if cp.ValidPeriod != "" {
			fmt.Printf("  有效期: %s\n", cp.ValidPeriod)
		}
		for i := range cp.Limits {
			l := &cp.Limits[i]
			fmt.Printf("  - %-8s 已用 %s / 限额 %s（%d%%）\n", l.Name, human(&l.Used), human(&l.Limit), l.Percent)
		}
	}
	return nil
}

// ===== 显示辅助 =====

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func human(v *float64) string {
	if v == nil {
		return "未知"
	}
	f := *v
	if f == float64(int64(f)) {
		return commaInt(int64(f))
	}
	return fmt.Sprintf("%.2f", f)
}

func commaInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	if neg {
		return "-" + strings.Join(parts, ",")
	}
	return strings.Join(parts, ",")
}

func percent(v *float64) string {
	if v == nil {
		return "未知"
	}
	return fmt.Sprintf("%.1f%%", *v)
}
