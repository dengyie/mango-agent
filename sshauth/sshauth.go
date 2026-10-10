package sshauth

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	leaseMu    sync.Mutex
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
		var err error
		homeDir, err = os.UserHomeDir()
		if err != nil {
			return "", err
		}
	}

	return filepath.Join(homeDir, ".ssh", "authorized_keys"), nil
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

	leaseMu.Lock()
	defer leaseMu.Unlock()

	// 先清理可能存在的同 ticket_id 旧租约（保证幂等性）
	_ = removeKeyByTicketLocked(authPath, ticketID)

	dir := filepath.Dir(authPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create .ssh dir: %w", err)
	}

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

	// 写回文件
	outContent := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(authPath, []byte(outContent), 0600); err != nil {
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
		RevokeKey(ticketID, targetUser)
	})

	return nil
}

// RevokeKey 从 authorized_keys 中注销并删除指定 ticketID 的公钥与标记
func RevokeKey(ticketID, targetUser string) error {
	authPath, err := ResolveAuthorizedKeysPath(targetUser)
	if err != nil {
		return fmt.Errorf("resolve authorized_keys path: %w", err)
	}

	leaseMu.Lock()
	defer leaseMu.Unlock()

	if t, exists := leaseTimers[ticketID]; exists {
		t.Stop()
		delete(leaseTimers, ticketID)
	}

	return removeKeyByTicketLocked(authPath, ticketID)
}

func removeKeyByTicketLocked(authPath, ticketID string) error {
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
			continue
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
	if err := os.WriteFile(authPath, []byte(outContent), 0600); err != nil {
		return err
	}
	log.Printf("[sshauth] Revoked and pruned SSH key for ticket %s from %s", ticketID, authPath)
	return nil
}
