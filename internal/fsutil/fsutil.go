// Package fsutil 提供原子写等文件工具。
package fsutil

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic 先写临时文件再 rename，避免半写状态损坏登录态。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.zcas.tmp-%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// WriteJSONAtomic 原子写入 JSON（缩进格式）。
func WriteJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, b, 0o600)
}

// ReadJSONFile 读取 JSON 对象为 map。文件不存在时返回 os.IsNotExist 错误。
func ReadJSONFile(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	return m, nil
}

func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
