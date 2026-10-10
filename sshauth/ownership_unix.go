//go:build !windows

package sshauth

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

func setFileOwner(path string, uid, gid int) error {
	if uid < 0 || gid < 0 {
		return nil
	}
	return os.Chown(path, uid, gid)
}

func getTargetOwnership(authPath, targetUser string) (int, int) {
	if targetUser != "" {
		if u, err := user.Lookup(targetUser); err == nil {
			uid, err1 := strconv.Atoi(u.Uid)
			gid, err2 := strconv.Atoi(u.Gid)
			if err1 == nil && err2 == nil {
				return uid, gid
			}
		}
	}
	// 优先继承已有 authorized_keys 文件的属主与属组
	if fi, err := os.Stat(authPath); err == nil {
		if stat, ok := fi.Sys().(*syscall.Stat_t); ok {
			return int(stat.Uid), int(stat.Gid)
		}
	}
	// 若文件尚未创建，继承其父目录（如 ~/.ssh）的属主与属组
	if fi, err := os.Stat(filepath.Dir(authPath)); err == nil {
		if stat, ok := fi.Sys().(*syscall.Stat_t); ok {
			return int(stat.Uid), int(stat.Gid)
		}
	}
	return -1, -1
}
