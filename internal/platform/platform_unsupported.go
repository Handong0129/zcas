//go:build !darwin

// Package platform 非 macOS 平台的占位实现。当前仅支持 macOS；
// 扩展新平台时新建 platform_<goos>.go 实现同名函数即可，上层零改动。
package platform

import (
	"errors"
	"time"
)

var ErrUnsupported = errors.New("当前仅支持 macOS")

func IsZCodeRunning() bool          { return false }
func KillZCode(time.Duration) error { return ErrUnsupported }
func LaunchZCode() error            { return ErrUnsupported }
func OpenURL(string) error          { return ErrUnsupported }
func ZCodeVersion() string          { return "" }
func OSVersion() string             { return "" }
