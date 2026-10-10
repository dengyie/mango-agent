package sshauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuthorizeAndRevokeKey(t *testing.T) {
	tmpDir := t.TempDir()
	authPath := filepath.Join(tmpDir, "authorized_keys")

	// 初始写入一个已有 key
	existingKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAExistingAdminKey admin@host"
	if err := os.WriteFile(authPath, []byte(existingKey+"\n"), 0600); err != nil {
		t.Fatalf("write existing: %v", err)
	}

	ticketID := "test-ticket-12345"
	newKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAFreshGrantedKey granted@guest"
	exp := time.Now().Add(10 * time.Minute)

	// Direct test for removeKeyByTicketLocked and format
	tagLine := leaseTagPrefix + ticketID + " EXP:" + string(rune(exp.Unix()))
	lines := []string{existingKey, tagLine, newKey}
	_ = os.WriteFile(authPath, []byte(strings.Join(lines, "\n")+"\n"), 0600)

	content, _ := os.ReadFile(authPath)
	if !strings.Contains(string(content), newKey) {
		t.Fatalf("Expected newKey present")
	}

	// Revoke
	leaseMu.Lock()
	err := removeKeyByTicketLocked(authPath, ticketID)
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
