package monitoring

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseUintBytes(t *testing.T) {
	v, err := parseUintBytes([]byte("323013632\n"))
	if err != nil || v != 323013632 {
		t.Fatalf("got %d %v", v, err)
	}
	if _, err := parseUintBytes([]byte("max\n")); err == nil {
		t.Fatal("expected error for max")
	}
}

func TestReadCgroupMemoryV2Fixture(t *testing.T) {
	// Unit test pure parsers via temp files is hard without chroot;
	// exercise unlimited filter constant and path join assumptions.
	if cgroupUnlimited <= 1<<60 {
		t.Fatal("unlimited threshold too low")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte("323013632\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte("67108864\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	limB, _ := os.ReadFile(filepath.Join(dir, "memory.max"))
	useB, _ := os.ReadFile(filepath.Join(dir, "memory.current"))
	lim, err := parseUintBytes(limB)
	if err != nil || lim != 323013632 {
		t.Fatalf("lim %d %v", lim, err)
	}
	use, err := parseUintBytes(useB)
	if err != nil || use != 67108864 {
		t.Fatalf("use %d %v", use, err)
	}
}

func TestReadCPUQuotaV2Parse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte("25000 100000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	q, p, ok := readCPUQuota(dir, true)
	if !ok || q != 25000 || p != 100000 {
		t.Fatalf("q=%d p=%d ok=%v", q, p, ok)
	}
	if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte("max 100000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := readCPUQuota(dir, true); ok {
		t.Fatal("max should be unlimited")
	}
}

func TestReadCPUQuotaV2Hierarchy(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "init.scope")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "cpu.max"), []byte("100000 100000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "cpu.max"), []byte("max 100000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	q, p, ok := readCPUQuota(child, true)
	if !ok || q != 100000 || p != 100000 {
		t.Fatalf("expected parent quota 100000/100000, got q=%d p=%d ok=%v", q, p, ok)
	}
}

func TestReadCPUUsageUsecHierarchy(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "init.scope")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "cpu.stat"), []byte("usage_usec 47561201584\nuser_usec 44672192991\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v, ok := readCPUUsageUsec(child, true)
	if !ok || v != 47561201584 {
		t.Fatalf("expected usage 47561201584, got %d ok=%v", v, ok)
	}
}
