package sshauth

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAtomicWriteAndRevokeKey(t *testing.T) {
	tmpDir := t.TempDir()
	authPath := filepath.Join(tmpDir, "authorized_keys")

	// 初始写入已有 key
	existingKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAExistingAdminKey admin@host"
	if err := writeAuthorizedKeysAtomic(authPath, []byte(existingKey+"\n")); err != nil {
		t.Fatalf("atomic write existing: %v", err)
	}

	ticketID := "test-ticket-12345"
	newKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAFreshGrantedKey granted@guest"
	exp := time.Now().Add(10 * time.Minute)

	// 正规格式：EXP:<unix_number>
	tagLine := fmt.Sprintf("%s%s EXP:%d", leaseTagPrefix, ticketID, exp.Unix())
	lines := []string{existingKey, tagLine, newKey}
	if err := writeAuthorizedKeysAtomic(authPath, []byte(strings.Join(lines, "\n")+"\n")); err != nil {
		t.Fatalf("atomic write with lease: %v", err)
	}

	content, err := os.ReadFile(authPath)
	if err != nil || !strings.Contains(string(content), newKey) {
		t.Fatalf("Expected newKey present: %v", err)
	}

	// 执行注销
	leaseMu.Lock()
	err = removeKeyByTicketLocked(authPath, ticketID)
	leaseMu.Unlock()
	if err != nil {
		t.Fatalf("removeKeyByTicketLocked failed: %v", err)
	}

	contentAfter, _ := os.ReadFile(authPath)
	strAfter := string(contentAfter)
	if strings.Contains(strAfter, newKey) {
		t.Fatalf("newKey should have been removed after revoke")
	}
	if strings.Contains(strAfter, ticketID) {
		t.Fatalf("ticket tag should have been removed after revoke")
	}
	if !strings.Contains(strAfter, existingKey) {
		t.Fatalf("existingKey should have been preserved!")
	}
}

func TestReconcileLeases(t *testing.T) {
	tmpDir := t.TempDir()
	authPath := filepath.Join(tmpDir, "authorized_keys")

	adminKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAAdmin admin@internal"

	// 构造三个租约场景：
	// 1. 已过期的 ticket 1
	// 2. 依然有效的 ticket 2
	// 3. 普通管理员 key
	expPast := time.Now().Add(-10 * time.Minute).Unix()
	expFuture := time.Now().Add(30 * time.Minute).Unix()

	lines := []string{
		adminKey,
		fmt.Sprintf("%sstale-ticket EXP:%d", leaseTagPrefix, expPast),
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAStaleKey guest@stale",
		fmt.Sprintf("%sactive-ticket EXP:%d", leaseTagPrefix, expFuture),
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAActiveKey guest@active",
	}
	_ = writeAuthorizedKeysAtomic(authPath, []byte(strings.Join(lines, "\n")+"\n"))

	// 直接调用真正的 reconcileLeasesAtPath 测试真实业务逻辑！
	cleaned, err := reconcileLeasesAtPath(authPath, "")
	if err != nil {
		t.Fatalf("reconcileLeasesAtPath failed: %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("Expected 1 cleaned, got %d", cleaned)
	}

	finalContent, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read finalContent failed: %v", err)
	}
	strFinal := string(finalContent)
	if strings.Contains(strFinal, "StaleKey") {
		t.Fatalf("StaleKey was not purged!")
	}
	if strings.Contains(strFinal, "stale-ticket") {
		t.Fatalf("stale-ticket tag was not purged!")
	}
	if !strings.Contains(strFinal, "ActiveKey") {
		t.Fatalf("ActiveKey was wrongly purged!")
	}
	if !strings.Contains(strFinal, "active-ticket") {
		t.Fatalf("active-ticket tag was wrongly purged!")
	}
	if !strings.Contains(strFinal, adminKey) {
		t.Fatalf("adminKey was wrongly purged!")
	}
}
