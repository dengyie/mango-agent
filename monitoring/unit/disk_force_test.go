package monitoring

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDirApparentSizeCache 验证 60s TTL 缓存：第二次调用同一 root 应返回缓存值（不重复 walk）。
func TestDirApparentSizeCache(t *testing.T) {
	// 创建一个临时目录，写一个文件
	dir := t.TempDir()
	file := filepath.Join(dir, "testfile")
	if err := os.WriteFile(file, make([]byte, 12345), 0644); err != nil {
		t.Fatal(err)
	}

	// 第一次调用：应真实 walk，返回文件大小
	used1, ok1 := dirApparentSizeCached(dir)
	if !ok1 || used1 != 12345 {
		t.Fatalf("first call: expected 12345, got %d (ok=%v)", used1, ok1)
	}

	// 第二次调用（同一 root，TTL 内）：应命中缓存，不重复 walk
	used2, ok2 := dirApparentSizeCached(dir)
	if !ok2 || used2 != 12345 {
		t.Fatalf("second call (cache): expected 12345, got %d (ok=%v)", used2, ok2)
	}

	// 立即在目录中加一个新文件，验证缓存返回旧值（不重新 walk）
	if err := os.WriteFile(filepath.Join(dir, "newfile"), make([]byte, 9999), 0644); err != nil {
		t.Fatal(err)
	}
	used3, ok3 := dirApparentSizeCached(dir)
	if !ok3 || used3 != 12345 {
		t.Fatalf("third call (should be cached old value): expected 12345, got %d (ok=%v)", used3, ok3)
	}

	// 不同 root 应失效缓存
	otherDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(otherDir, "other"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	used4, ok4 := dirApparentSizeCached(otherDir)
	if !ok4 || used4 != 1 {
		t.Fatalf("different root: expected 1, got %d (ok=%v)", used4, ok4)
	}
}

// TestDirApparentSizeCacheExpiry 验证 TTL 过期后重新 walk。
// 将 diskWalkAt 直接设为 61s 前模拟过期，再调用应重新 walk 拿到新数据。
func TestDirApparentSizeCacheExpiry(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	// 首次调用，建立缓存
	used1, ok1 := dirApparentSizeCached(dir)
	if !ok1 || used1 != 5 {
		t.Fatalf("first call: expected 5, got %d", used1)
	}

	// 把缓存时间戳推到 61s 前（模拟过期）
	diskWalkMu.Lock()
	diskWalkAt = time.Now().Add(-(diskWalkTTL + 2*time.Second))
	diskWalkMu.Unlock()

	// 添加新文件后调用，应重新 walk 并拿到新值
	if err := os.WriteFile(filepath.Join(dir, "b"), make([]byte, 100), 0644); err != nil {
		t.Fatal(err)
	}
	used2, ok2 := dirApparentSizeCached(dir)
	if !ok2 || used2 != 105 {
		t.Fatalf("after expiry: expected 105 (5+100), got %d (ok=%v)", used2, ok2)
	}
}
