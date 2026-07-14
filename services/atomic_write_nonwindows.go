//go:build !windows

package services

import (
	"fmt"
	"os"
	"path/filepath"
)

// atomicRename Unix 平台原子重命名
// os.Rename 在 POSIX 系统上本身就是原子操作
func atomicRename(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		os.Remove(src)
		return fmt.Errorf("原子替换失败 %s -> %s: os.Rename 失败: %w", src, dst, err)
	}
	dir, err := os.Open(filepath.Dir(dst))
	if err != nil {
		return fmt.Errorf("打开父目录以同步失败 %s: %w", filepath.Dir(dst), err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("同步父目录失败 %s: %w", filepath.Dir(dst), err)
	}
	return nil
}
