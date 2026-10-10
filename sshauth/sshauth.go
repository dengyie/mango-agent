package sshauth

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	leaseMu     sync.Mutex
	leaseTimers = make(map[string]*time.Timer)
)

const leaseTagPrefix = "# MANGOHUB_TICKET:"

// ResolveAuthorizedKeysPath 自动计算当前操作系统及用户下的 authorized_keys 绝对路径
func ResolveAuthorizedKeysPath(targetUsername string) (string, error) {
	if runtime.GOOS == "windows" {
		// Windows: 管理员或默认路径
		progData := os.Getenv("ProgramData")
		if progData == "" {
			progData = `C:\ProgramData`
		}
		adminPath := filepath.Join(progData, "ssh", "administrators_authorized_keys")
		if _, err := os.Stat(filepath.Dir(adminPath)); err == nil {
			return adminPath, nil
		}
		// 回退到当前用户目录
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(homeDir, ".ssh", "authorized_keys"), nil
	}

	// Linux / Darwin / Unix:
	var homeDir string
	if targetUsername != "" {
		if u, err := user.Lookup(targetUsername); err == nil && u.HomeDir != "" {
			homeDir = u.HomeDir
		}
	}
	if homeDir == "" {
		if u, err := user.Current(); err == nil && u.HomeDir != "" {
			homeDir = u.HomeDir
		} else if h, err := os.UserHomeDir(); err == nil && h != "" {
			homeDir = h
		} else if os.Geteuid() == 0 {
			homeDir = "/root"
		}
	}
	if homeDir == "" {
		return "", fmt.Errorf("cannot resolve home directory for user %q", targetUsername)
	}

	return filepath.Join(homeDir, ".ssh", "authorized_keys"), nil
}

// writeAuthorizedKeysAtomic 使用临时文件 + fsync + 原子 Rename 确保授权文件写入不发生损坏，
// 并自动继承或设置文件属主属组，杜绝在 root 权限下写入导致 OpenSSH StrictModes 校验失败。
func writeAuthorizedKeysAtomic(authPath string, content []byte, targetUser string) error {
	dir := filepath.Dir(authPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create auth dir %s: %w", dir, err)
	}

	tmpFile, err := os.CreateTemp(dir, "authkeys-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp auth file: %w", err)
	}
	tmpName := tmpFile.Name()

	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpName)
	}()

	if err := tmpFile.Chmod(0600); err != nil {
		return fmt.Errorf("chmod temp auth file: %w", err)
	}

	// 继承或设置目标属主属组
	uid, gid := getTargetOwnership(authPath, targetUser)
	if uid >= 0 && gid >= 0 {
		_ = setFileOwner(tmpName, uid, gid)
	}

	if _, err := tmpFile.Write(content); err != nil {
		return fmt.Errorf("write temp auth file: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("fsync temp auth file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp auth file: %w", err)
	}

	if err := os.Rename(tmpName, authPath); err != nil {
		return fmt.Errorf("atomic rename auth file: %w", err)
	}

	return nil
}

// AuthorizeKey 将指定的公钥以租约形式注入 authorized_keys，并配置过期时间
func AuthorizeKey(ticketID, pubKey, targetUser string, expiresAt time.Time) error {
	if ticketID == "" || pubKey == "" {
		return fmt.Errorf("ticket_id and public_key are required")
	}

	authPath, err := ResolveAuthorizedKeysPath(targetUser)
	if err != nil {
		return fmt.Errorf("resolve authorized_keys path: %w", err)
	}

	return authorizeKeyAtPath(authPath, ticketID, pubKey, targetUser, expiresAt)
}

func authorizeKeyAtPath(authPath, ticketID, pubKey, targetUser string, expiresAt time.Time) error {
	leaseMu.Lock()
	defer leaseMu.Unlock()

	// 先清理可能存在的同 ticket_id 旧租约（保证幂等性）
	_ = removeKeyByTicketLocked(authPath, ticketID, targetUser)

	// 读取现有内容
	var lines []string
	if fileBytes, err := os.ReadFile(authPath); err == nil {
		scanner := bufio.NewScanner(strings.NewReader(string(fileBytes)))
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
	}

	// 格式化租约标记和公钥
	tagLine := fmt.Sprintf("%s%s EXP:%d", leaseTagPrefix, ticketID, expiresAt.Unix())
	keyLine := strings.TrimSpace(pubKey)
	lines = append(lines, tagLine, keyLine)

	// 原子写回文件
	outContent := strings.Join(lines, "\n") + "\n"
	if err := writeAuthorizedKeysAtomic(authPath, []byte(outContent), targetUser); err != nil {
		return fmt.Errorf("write authorized_keys: %w", err)
	}

	log.Printf("[sshauth] Authorized SSH key for ticket %s (expires: %s) to %s", ticketID, expiresAt.Format(time.RFC3339), authPath)

	// 启动/重设本地自动回收定时器
	if t, exists := leaseTimers[ticketID]; exists {
		t.Stop()
	}
	remaining := time.Until(expiresAt)
	if remaining <= 0 {
		remaining = 5 * time.Second
	}
	leaseTimers[ticketID] = time.AfterFunc(remaining, func() {
		_ = RevokeKey(ticketID, targetUser)
	})

	return nil
}

// RevokeKey 从 authorized_keys 中注销并删除指定 ticketID 的公钥与标记
func RevokeKey(ticketID, targetUser string) error {
	if targetUser != "" {
		authPath, err := ResolveAuthorizedKeysPath(targetUser)
		if err != nil {
			return fmt.Errorf("resolve authorized_keys path: %w", err)
		}
		return revokeKeyAtPath(authPath, ticketID, targetUser)
	}

	// targetUser 未知时，按默认路径注销；在 Unix 下额外扫描所有用户目录
	var firstErr error
	if defaultPath, err := ResolveAuthorizedKeysPath(""); err == nil {
		if err := revokeKeyAtPath(defaultPath, ticketID, ""); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if runtime.GOOS != "windows" {
		userKeys, _ := filepath.Glob("/home/*/.ssh/authorized_keys")
		for _, p := range userKeys {
			parts := strings.Split(p, string(filepath.Separator))
			uName := ""
			if len(parts) >= 3 {
				uName = parts[2]
			}
			_ = revokeKeyAtPath(p, ticketID, uName)
		}
		_ = revokeKeyAtPath("/root/.ssh/authorized_keys", ticketID, "root")
	}

	return firstErr
}

func revokeKeyAtPath(authPath, ticketID, targetUser string) error {
	leaseMu.Lock()
	defer leaseMu.Unlock()

	if t, exists := leaseTimers[ticketID]; exists {
		t.Stop()
		delete(leaseTimers, ticketID)
	}

	return removeKeyByTicketLocked(authPath, ticketID, targetUser)
}

func removeKeyByTicketLocked(authPath, ticketID, targetUser string) error {
	if _, err := os.Stat(authPath); os.IsNotExist(err) {
		return nil
	}

	fileBytes, err := os.ReadFile(authPath)
	if err != nil {
		return err
	}

	tagPrefix := fmt.Sprintf("%s%s", leaseTagPrefix, ticketID)
	lines := strings.Split(string(fileBytes), "\n")
	var newLines []string

	skipNextKey := false
	removed := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, tagPrefix) {
			skipNextKey = true
			removed = true
			continue
		}
		if skipNextKey {
			skipNextKey = false
			if !strings.HasPrefix(trimmed, leaseTagPrefix) {
				continue
			}
		}
		if trimmed != "" {
			newLines = append(newLines, line)
		}
	}

	if !removed {
		return nil
	}

	outContent := ""
	if len(newLines) > 0 {
		outContent = strings.Join(newLines, "\n") + "\n"
	}
	if err := writeAuthorizedKeysAtomic(authPath, []byte(outContent), targetUser); err != nil {
		return err
	}
	log.Printf("[sshauth] Revoked and pruned SSH key for ticket %s from %s", ticketID, authPath)
	return nil
}

// ReconcileLeases 启动自愈对账：扫描 authorized_keys 中残留的租约标记。
// 若已过期，则立即安全抹除该公钥；若尚未过期，则恢复其后台自动注销定时器。
// 彻底解决由于 Agent 重启、机器崩溃导致的租约定时器丢失与后门残留问题！
func ReconcileLeases(targetUser string) (int, error) {
	if targetUser != "" {
		authPath, err := ResolveAuthorizedKeysPath(targetUser)
		if err != nil {
			return 0, fmt.Errorf("resolve authorized_keys path: %w", err)
		}
		return reconcileLeasesAtPath(authPath, targetUser)
	}

	// targetUser 为空时（常用于启动自愈）：
	// 1. 扫描当前/默认用户 authorized_keys
	totalCleaned := 0
	defaultPath, err := ResolveAuthorizedKeysPath("")
	if err == nil {
		if cleaned, _ := reconcileLeasesAtPath(defaultPath, ""); cleaned > 0 {
			totalCleaned += cleaned
		}
	}

	// 2. 在 Unix 系统下，额外扫描系统所有真实用户的 /home/*/.ssh/authorized_keys 以及 /root/.ssh/authorized_keys
	if runtime.GOOS != "windows" {
		scannedPaths := make(map[string]struct{})
		if defaultPath != "" {
			scannedPaths[defaultPath] = struct{}{}
		}

		userKeys, _ := filepath.Glob("/home/*/.ssh/authorized_keys")
		for _, p := range userKeys {
			if _, seen := scannedPaths[p]; seen {
				continue
			}
			scannedPaths[p] = struct{}{}
			parts := strings.Split(p, string(filepath.Separator))
			uName := ""
			if len(parts) >= 3 {
				uName = parts[2]
			}
			if cleaned, _ := reconcileLeasesAtPath(p, uName); cleaned > 0 {
				totalCleaned += cleaned
			}
		}

		rootKey := "/root/.ssh/authorized_keys"
		if _, seen := scannedPaths[rootKey]; !seen {
			if cleaned, _ := reconcileLeasesAtPath(rootKey, "root"); cleaned > 0 {
				totalCleaned += cleaned
			}
		}
	}

	return totalCleaned, nil
}

func reconcileLeasesAtPath(authPath, targetUser string) (int, error) {
	leaseMu.Lock()
	defer leaseMu.Unlock()

	if _, err := os.Stat(authPath); os.IsNotExist(err) {
		return 0, nil
	}

	fileBytes, err := os.ReadFile(authPath)
	if err != nil {
		return 0, err
	}

	now := time.Now().Unix()
	lines := strings.Split(string(fileBytes), "\n")
	var newLines []string
	expiredTickets := make(map[string]struct{})
	activeLeases := make(map[string]int64) // ticketID -> expUnix

	skipNext := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, leaseTagPrefix) {
			parts := strings.Fields(trimmed)
			// Format: # MANGOHUB_TICKET:<ticket_id> EXP:<unix>
			ticketID := ""
			expUnix := int64(0)
			for _, p := range parts {
				if strings.HasPrefix(p, "MANGOHUB_TICKET:") {
					ticketID = strings.TrimPrefix(p, "MANGOHUB_TICKET:")
				} else if strings.HasPrefix(p, "EXP:") {
					expUnix, _ = strconv.ParseInt(strings.TrimPrefix(p, "EXP:"), 10, 64)
				}
			}

			if ticketID == "" {
				newLines = append(newLines, line)
				continue
			}

			if expUnix > 0 && expUnix <= now {
				expiredTickets[ticketID] = struct{}{}
				skipNext = true
				continue
			} else if expUnix > now {
				activeLeases[ticketID] = expUnix
			}
		}

		if skipNext {
			skipNext = false
			if !strings.HasPrefix(trimmed, leaseTagPrefix) {
				continue
			}
		}

		if trimmed != "" {
			newLines = append(newLines, line)
		}
	}

	// 如果有过期租约被清理，原子写回文件
	if len(expiredTickets) > 0 {
		outContent := ""
		if len(newLines) > 0 {
			outContent = strings.Join(newLines, "\n") + "\n"
		}
		if err := writeAuthorizedKeysAtomic(authPath, []byte(outContent), targetUser); err != nil {
			return 0, err
		}
		log.Printf("[sshauth] Reconcile cleaned %d expired SSH leases from %s", len(expiredTickets), authPath)
	}

	// 为尚未过期的租约恢复定时器
	rearmed := 0
	for ticketID, expUnix := range activeLeases {
		if _, exists := leaseTimers[ticketID]; exists {
			continue
		}
		tID := ticketID
		uUser := targetUser
		rem := time.Until(time.Unix(expUnix, 0))
		if rem <= 0 {
			rem = 1 * time.Second
		}
		leaseTimers[tID] = time.AfterFunc(rem, func() {
			_ = RevokeKey(tID, uUser)
		})
		rearmed++
	}

	if rearmed > 0 {
		log.Printf("[sshauth] Reconcile re-armed %d active SSH lease timers", rearmed)
	}

	return len(expiredTickets), nil
}
