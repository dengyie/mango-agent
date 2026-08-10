package monitoring

import (
	"testing"

	"github.com/shirou/gopsutil/v4/disk"
)

func TestTrimDevPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/dev/vda5", "vda5"},
		{"/dev/sda1", "sda1"},
		{"vda5", "vda5"},
	}
	for _, c := range cases {
		if got := trimDevPrefix(c.in); got != c.want {
			t.Errorf("trimDevPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDiskSafeDelta(t *testing.T) {
	cases := []struct{ cur, prev, want uint64 }{
		{100, 50, 50}, // 正常增量
		{50, 100, 0},  // 回绕/重置 → 0
		{50, 50, 0},
	}
	for _, c := range cases {
		if got := diskSafeDelta(c.cur, c.prev); got != c.want {
			t.Errorf("diskSafeDelta(%d,%d) = %d, want %d", c.cur, c.prev, got, c.want)
		}
	}
}

func TestFilterMountTargetsWithIncludes(t *testing.T) {
	orig := flags.IncludeMountpoints
	flags.IncludeMountpoints = "/tmp/mnt;/hostfs"
	defer func() { flags.IncludeMountpoints = orig }()

	parts := []disk.PartitionStat{
		{Device: "/dev/vda5", Mountpoint: "/tmp/mnt", Fstype: "ext4"},
		{Device: "/dev/sda1", Mountpoint: "/hostfs", Fstype: "ext4"},
		{Device: "/dev/vda1", Mountpoint: "/", Fstype: "ext4"},
	}
	got := filterMountTargets(parts)
	if len(got) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(got))
	}
	for _, g := range got {
		if g.Mountpoint == "/tmp/mnt" && g.Device != "vda5" {
			t.Errorf("wrong device for /tmp/mnt: %q", g.Device)
		}
		if g.Mountpoint == "/hostfs" && g.Device != "sda1" {
			t.Errorf("wrong device for /hostfs: %q", g.Device)
		}
	}
}

func TestFilterMountTargetsSkipsMissing(t *testing.T) {
	orig := flags.IncludeMountpoints
	flags.IncludeMountpoints = "/nonexistent"
	defer func() { flags.IncludeMountpoints = orig }()

	parts := []disk.PartitionStat{
		{Device: "/dev/vda1", Mountpoint: "/", Fstype: "ext4"},
	}
	if got := filterMountTargets(parts); len(got) != 0 {
		t.Fatalf("expected 0 targets for missing mountpoint, got %d", len(got))
	}
}

func TestFilterMountTargetsDefaultDedupes(t *testing.T) {
	orig := flags.IncludeMountpoints
	flags.IncludeMountpoints = ""
	defer func() { flags.IncludeMountpoints = orig }()

	parts := []disk.PartitionStat{
		{Device: "/dev/vda5", Mountpoint: "/", Fstype: "ext4"},
		{Device: "/dev/vda1", Mountpoint: "/boot", Fstype: "tmpfs"},   // tmpfs 非物理盘,应排除
		{Device: "/dev/vda5", Mountpoint: "/mnt/sub", Fstype: "ext4"}, // 同设备,保留路径最短根挂载点
	}
	got := filterMountTargets(parts)
	if len(got) != 1 {
		t.Fatalf("expected 1 physical target, got %d", len(got))
	}
	if got[0].Device != "vda5" || got[0].Mountpoint != "/" {
		t.Errorf("expected vda5@/, got %+v", got[0])
	}
}

func TestIsVirtualDevice(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"/dev/sda1", false},
		{"/dev/vda5", false},
		{"overlay", true},
		{"tmpfs", true},
		{"none", true},
		{"shm", true},
	}
	for _, c := range cases {
		if got := isVirtualDevice(c.in); got != c.want {
			t.Errorf("isVirtualDevice(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestFindMountinfoMajorMinor(t *testing.T) {
	content := `995 939 0:87 / / rw,relatime master:363 - overlay overlay rw,lowerdir=...,upperdir=...
996 995 0:90 / /proc rw,nosuid - proc proc rw
1015 995 0:61 / /tmp/mnt rw,relatime - ext4 /dev/vda5 rw,prjquota`
	if got := findMountinfoMajorMinor(content, "/"); got != "0:87" {
		t.Errorf("mountpoint / => %q, want 0:87", got)
	}
	if got := findMountinfoMajorMinor(content, "/tmp/mnt"); got != "0:61" {
		t.Errorf("mountpoint /tmp/mnt => %q, want 0:61", got)
	}
	if got := findMountinfoMajorMinor(content, "/nonexistent"); got != "" {
		t.Errorf("mountpoint /nonexistent => %q, want empty", got)
	}
}

// 白名单挂载点解析成虚拟设备(overlay 根)且无其它可用目标 → 回退默认物理盘遍历。
func TestFilterMountTargetsOverlayFallback(t *testing.T) {
	orig := flags.IncludeMountpoints
	flags.IncludeMountpoints = "/"
	defer func() { flags.IncludeMountpoints = orig }()

	parts := []disk.PartitionStat{
		{Device: "overlay", Mountpoint: "/", Fstype: "overlay"},
		{Device: "/dev/vda5", Mountpoint: "/tmp/mnt", Fstype: "ext4"},
	}
	got := filterMountTargets(parts)
	if len(got) != 1 {
		t.Fatalf("expected 1 physical fallback target, got %d", len(got))
	}
	if got[0].Device != "vda5" || got[0].Mountpoint != "/tmp/mnt" {
		t.Errorf("expected vda5@/tmp/mnt (fallback), got %+v", got[0])
	}
}

// 白名单里既有虚拟根又有真实数据盘 → 只保留真实目标,不触发回退(避免过度采集)。
func TestFilterMountTargetsMixedVirtualAndReal(t *testing.T) {
	orig := flags.IncludeMountpoints
	flags.IncludeMountpoints = "/;/tmp/mnt"
	defer func() { flags.IncludeMountpoints = orig }()

	parts := []disk.PartitionStat{
		{Device: "overlay", Mountpoint: "/", Fstype: "overlay"},
		{Device: "/dev/vda5", Mountpoint: "/tmp/mnt", Fstype: "ext4"},
	}
	got := filterMountTargets(parts)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 target, got %d", len(got))
	}
	if got[0].Device != "vda5" || got[0].Mountpoint != "/tmp/mnt" {
		t.Errorf("expected vda5@/tmp/mnt, got %+v", got[0])
	}
}

// 兜底按真实块设备收:overlay 根排除,/tmp/mnt 的真实数据盘收进来,loop 虚拟盘排除。
func TestFallbackPhysicalTargets(t *testing.T) {
	parts := []disk.PartitionStat{
		{Device: "overlay", Mountpoint: "/", Fstype: "overlay"},
		{Device: "/dev/vda5", Mountpoint: "/tmp/mnt", Fstype: "ext4"},
		{Device: "/dev/vda5", Mountpoint: "/etc/hosts", Fstype: "ext4"}, // 同设备,保留最短挂载点 /tmp/mnt
		{Device: "/dev/loop0", Mountpoint: "/mnt/loop", Fstype: "ext4"}, // loop 虚拟盘,排除
	}
	got := fallbackPhysicalTargets(parts)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 target, got %d", len(got))
	}
	if got[0].Device != "vda5" || got[0].Mountpoint != "/tmp/mnt" {
		t.Errorf("expected vda5@/tmp/mnt, got %+v", got[0])
	}
}

func TestComputeDiskIORates(t *testing.T) {
	targets := []mountTarget{{Mountpoint: "/tmp/mnt", Device: "vda5"}}
	prev := map[string]disk.IOCountersStat{
		"vda5": {ReadBytes: 100, WriteBytes: 200, ReadCount: 10, WriteCount: 20},
	}
	cur := map[string]disk.IOCountersStat{
		"vda5": {ReadBytes: 200, WriteBytes: 400, ReadCount: 20, WriteCount: 40},
	}
	got := computeDiskIORates(targets, prev, cur, 2.0)
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	g := got[0]
	if g.ReadBytes != 50 || g.WriteBytes != 100 || g.ReadIOPS != 5 || g.WriteIOPS != 10 {
		t.Errorf("unexpected rates (delta/2s), got %+v", g)
	}
}

func TestComputeDiskIORatesCounterReset(t *testing.T) {
	targets := []mountTarget{{Mountpoint: "/hostfs", Device: "sda1"}}
	prev := map[string]disk.IOCountersStat{
		"sda1": {ReadBytes: 1000, WriteBytes: 1000, ReadCount: 100, WriteCount: 100},
	}
	cur := map[string]disk.IOCountersStat{
		"sda1": {ReadBytes: 100, WriteBytes: 200, ReadCount: 10, WriteCount: 20}, // 全部 < prev → 0 增量
	}
	got := computeDiskIORates(targets, prev, cur, 1.0)
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	g := got[0]
	if g.ReadBytes != 0 || g.WriteBytes != 0 || g.ReadIOPS != 0 || g.WriteIOPS != 0 {
		t.Errorf("expected all-zero on counter reset, got %+v", g)
	}
}

func TestComputeDiskIORatesFirstSample(t *testing.T) {
	targets := []mountTarget{{Mountpoint: "/", Device: "vda1"}}
	got := computeDiskIORates(targets, nil, map[string]disk.IOCountersStat{"vda1": {ReadBytes: 10}}, 1.0)
	if got != nil {
		t.Fatalf("expected nil on first sample, got %+v", got)
	}
}
