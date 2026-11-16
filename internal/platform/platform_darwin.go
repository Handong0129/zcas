//go:build darwin

// Package platform 封装所有 macOS 平台相关操作（进程管理、协议回调外的系统交互）。
package platform

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ZCode macOS 客户端实测值。
const (
	BundleID    = "dev.zcode.app"
	ProcessName = "ZCode"
)

var ErrZCodeNotRunning = errors.New("ZCode 未在运行")

// zcodePIDs 枚举 ZCode 主进程 PID。
// 不用 pgrep/pkill：实测部分 macOS（darwin 27）上 pgrep 会漏掉个别进程——
// ZCode 主进程对 pgrep 完全不可见（ps 能看到），导致运行检测恒为 false、
// 切换账号时不杀进程直接写登录态文件，客户端内存里的旧会话把切换"吃掉"。
// ps 的进程枚举是完整的，按 comm 精确匹配主进程名（不误伤 Helper）。
func zcodePIDs() []int {
	out, err := exec.Command("ps", "-axo", "pid=,comm=").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), " ", 2)
		if len(fields) != 2 {
			continue
		}
		if filepath.Base(strings.TrimSpace(fields[1])) != ProcessName {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// IsZCodeRunning 检测 ZCode 主进程是否在运行。
func IsZCodeRunning() bool {
	return len(zcodePIDs()) > 0
}

// KillZCode 先 SIGTERM 优雅退出，超时后 SIGKILL 升级，再等确认。
func KillZCode(wait time.Duration) error {
	if !IsZCodeRunning() {
		return nil
	}
	for _, pid := range zcodePIDs() {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	if waitGone(wait) {
		return nil
	}
	for _, pid := range zcodePIDs() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	if waitGone(3 * time.Second) {
		return nil
	}
	return errors.New("关闭 ZCode 超时，请手动退出 ZCode 后重试")
}

func waitGone(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !IsZCodeRunning() {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return !IsZCodeRunning()
}

// LaunchZCode 用 bundle id 启动（比 -a 应用名稳定，不受改名/多副本影响）。
func LaunchZCode() error {
	if err := exec.Command("open", "-g", "-b", BundleID).Run(); err != nil {
		if err2 := exec.Command("open", "-g", "-a", ProcessName).Run(); err2 != nil {
			return fmt.Errorf("启动 ZCode 失败: %v / %v", err, err2)
		}
	}
	return nil
}

// OpenURL 用系统默认浏览器打开 URL。
func OpenURL(url string) error {
	return exec.Command("open", url).Run()
}

var cachedZCodeVersion string

// ZCodeVersion 读取已安装 ZCode 客户端的版本（billing 等接口的 app_version 参数应与真实客户端一致）。
func ZCodeVersion() string {
	if cachedZCodeVersion != "" {
		return cachedZCodeVersion
	}
	out, err := exec.Command("defaults", "read", "/Applications/ZCode.app/Contents/Info.plist", "CFBundleShortVersionString").Output()
	if err == nil {
		cachedZCodeVersion = strings.TrimSpace(string(out))
	}
	return cachedZCodeVersion
}

// ReadKeychainSecret 读取 macOS 钥匙串通用密码项（genp）的明文值。
// 委托系统 /usr/bin/security：它是 Apple 签名二进制，继承其钥匙串信任，
// 读取 ZCode 客户端写入的项时不会额外触发授权弹窗（实测确认）。
func ReadKeychainSecret(service, account string) (string, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", service, "-a", account, "-w").Output()
	if err != nil {
		return "", fmt.Errorf("读取钥匙串项失败: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

var cachedOSVersion string

// OSVersion 返回 macOS 版本号（X-Os-Version 头用）。
func OSVersion() string {
	if cachedOSVersion != "" {
		return cachedOSVersion
	}
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err == nil {
		cachedOSVersion = strings.TrimSpace(string(out))
	}
	return cachedOSVersion
}
