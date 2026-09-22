//go:build darwin

// Package platform 封装所有 macOS 平台相关操作（进程管理、协议回调外的系统交互）。
package platform

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ZCode macOS 客户端实测值。
const (
	BundleID    = "dev.zcode.app"
	ProcessName = "ZCode"
)

var ErrZCodeNotRunning = errors.New("ZCode 未在运行")

// IsZCodeRunning 检测 ZCode 主进程是否在运行（-x 精确匹配，不会误匹配 Helper 进程）。
func IsZCodeRunning() bool {
	return exec.Command("pgrep", "-x", ProcessName).Run() == nil
}

// KillZCode 先 SIGTERM 优雅退出，超时后 SIGKILL 升级，再等确认。
func KillZCode(wait time.Duration) error {
	if !IsZCodeRunning() {
		return nil
	}
	_ = exec.Command("pkill", "-x", ProcessName).Run()
	if waitGone(wait) {
		return nil
	}
	_ = exec.Command("pkill", "-9", "-x", ProcessName).Run()
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
