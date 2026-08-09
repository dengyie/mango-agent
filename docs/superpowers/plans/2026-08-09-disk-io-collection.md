# Komari Agent 磁盘 IO 采集二开 — 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 给 komari-agent 增加磁盘 IO 速率采集与上报（`report.disk_io`），无关的 server 面板展示本次不做。

**Architecture:** 新增 `monitoring/unit/disk_io.go` 采集单元：用 gopsutil `disk.Partitions(true)` 做挂载点→块设备映射，`disk.IOCounters()` 读 `/proc/diskstats` 计数器，两次采样差值 ÷ 实际间隔算出速率；`monitoring/monitoring.go` 的 `report` 结构加 `disk_io` 字段填充。只读不写、失败不阻塞。

**Tech Stack:** Go 1.24.0；`github.com/shirou/gopsutil/v4` v4.26.4（已在 go.mod，无新依赖）。

## Global Constraints

- module path 保持 `github.com/komari-monitor/komari-agent`（不新增依赖，go.sum 不变）。
- 复用现有旗标 `AGENT_INCLUDE_MOUNTPOINTS`（`flags.IncludeMountpoints`），**不新造**磁盘 IO 专用旗标。
- `HOST_PROC` 由 gopsutil v4 自动读取（`common.HostProc()` 检查该 env），**无需代码改动**。
- **不得修改**现有 `report.Disk {total, used}` 容量逻辑、`isPhysicalDisk`、`DiskList`。
- 采集失败 → `disk_io` 字段缺省（`omitempty`），其余指标照常，不阻塞。
- 告警、server 端面板展示磁盘 IO 曲线：**不在本次范围**。
- 新文件必须声明 `package monitoring`（`monitoring/unit/` 目录实际包名即 `monitoring`，与父目录同名）。

---

### Task 1: 新增磁盘 IO 采集单元 `monitoring/unit/disk_io.go`

**Files:**
- Create: `monitoring/unit/disk_io.go`
- Test: `monitoring/unit/disk_io_test.go`

**Interfaces:**
- Produces:
  - `type DiskIOStat struct { Device, Mountpoint string; ReadBytes, WriteBytes, ReadIOPS, WriteIOPS float64 }`（JSON tags: `device/mountpoint/read_bytes/write_bytes/read_iops/write_iops`）
  - `func DiskIO() []DiskIOStat` — 采集入口，失败返回空切片 `[]DiskIOStat{}`，不阻塞
  - 包级状态 `lastDiskIOCounters map[string]disk.IOCountersStat` + `lastDiskSampleTime time.Time`
  - 内部纯函数（供测试注入）：
    - `func filterMountTargets(parts []disk.PartitionStat) []mountTarget`
    - `func computeDiskIORates(targets []mountTarget, prev, cur map[string]disk.IOCountersStat, elapsed float64) []DiskIOStat`
    - `func trimDevPrefix(dev string) string`
    - `func diskSafeDelta(cur, prev uint64) uint64`
  - `type mountTarget struct { Mountpoint, Device string }`

- [x] **Step 1: 写失败测试**

`monitoring/unit/disk_io_test.go`：

```go
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
		{Device: "/dev/vda1", Mountpoint: "/boot", Fstype: "tmpfs"}, // tmpfs 非物理盘,应排除
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
```

- [x] **Step 2: 运行测试确认失败**

Run: `cd /Users/mango/project/katabump-probe/vps-monitor && go test ./monitoring/unit/ -run 'TestTrimDevPrefix|TestDiskSafeDelta|TestFilterMountTargets|TestComputeDiskIORates' -v`
Expected: FAIL，报 `undefined: trimDevPrefix` / `filterMountTargets` / `computeDiskIORates` / `diskSafeDelta`。

- [x] **Step 3: 写实现**

`monitoring/unit/disk_io.go`：

```go
package monitoring

import (
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
)

// DiskIOStat 单块磁盘的 IO 速率（字节/秒、次/秒）
type DiskIOStat struct {
	Device     string  `json:"device"`
	Mountpoint string  `json:"mountpoint"`
	ReadBytes  float64 `json:"read_bytes"`
	WriteBytes float64 `json:"write_bytes"`
	ReadIOPS   float64 `json:"read_iops"`
	WriteIOPS  float64 `json:"write_iops"`
}

// mountTarget 挂载点 → 块设备名（已去 /dev/ 前缀）
type mountTarget struct {
	Mountpoint string
	Device     string
}

var (
	lastDiskIOCounters map[string]disk.IOCountersStat
	lastDiskSampleTime time.Time
)

// filterMountTargets 按 AGENT_INCLUDE_MOUNTPOINTS 过滤分区表，得到 挂载点→设备 映射。
// 未配置时默认遍历所有物理盘（与 Disk()/DiskList() 同一批设备，并按设备去重保留最短根挂载点）。
func filterMountTargets(parts []disk.PartitionStat) []mountTarget {
	if flags.IncludeMountpoints != "" {
		byMount := make(map[string]string, len(parts))
		for _, p := range parts {
			byMount[p.Mountpoint] = p.Device
		}
		var targets []mountTarget
		for _, mp := range strings.Split(flags.IncludeMountpoints, ";") {
			mp = strings.TrimSpace(mp)
			if mp == "" {
				continue
			}
			dev, ok := byMount[mp]
			if !ok {
				continue // 挂载点在分区表里无对应设备，跳过
			}
			targets = append(targets, mountTarget{Mountpoint: mp, Device: trimDevPrefix(dev)})
		}
		return targets
	}

	seen := make(map[string]string) // device -> 最短 mountpoint
	for _, p := range parts {
		if !isPhysicalDisk(p) {
			continue
		}
		dev := p.Device
		if strings.ToLower(p.Fstype) == "zfs" {
			if idx := strings.Index(dev, "/"); idx != -1 {
				dev = dev[:idx]
			}
		}
		if existing, ok := seen[dev]; ok {
			if len(p.Mountpoint) < len(existing) {
				seen[dev] = p.Mountpoint
			}
		} else {
			seen[dev] = p.Mountpoint
		}
	}
	var targets []mountTarget
	for dev, mp := range seen {
		targets = append(targets, mountTarget{Mountpoint: mp, Device: trimDevPrefix(dev)})
	}
	return targets
}

func trimDevPrefix(dev string) string {
	return strings.TrimPrefix(dev, "/dev/")
}

// diskSafeDelta 处理计数器回绕/重置，视为 0 增量。
func diskSafeDelta(cur, prev uint64) uint64 {
	if cur >= prev {
		return cur - prev
	}
	return 0
}

// computeDiskIORates 纯函数：两次采样计数差 ÷ elapsed 得速率（字节/秒、次/秒）。
func computeDiskIORates(targets []mountTarget, prev, cur map[string]disk.IOCountersStat, elapsed float64) []DiskIOStat {
	if elapsed <= 0 || len(prev) == 0 || len(cur) == 0 {
		return nil
	}
	out := make([]DiskIOStat, 0, len(targets))
	for _, t := range targets {
		p, okP := prev[t.Device]
		c, okC := cur[t.Device]
		if !okP || !okC {
			continue // 该设备无计数，跳过
		}
		out = append(out, DiskIOStat{
			Device:     t.Device,
			Mountpoint: t.Mountpoint,
			ReadBytes:  float64(diskSafeDelta(c.ReadBytes, p.ReadBytes)) / elapsed,
			WriteBytes: float64(diskSafeDelta(c.WriteBytes, p.WriteBytes)) / elapsed,
			ReadIOPS:   float64(diskSafeDelta(c.ReadCount, p.ReadCount)) / elapsed,
			WriteIOPS:  float64(diskSafeDelta(c.WriteCount, p.WriteCount)) / elapsed,
		})
	}
	return out
}

// DiskIO 采集磁盘 IO 速率。失败返回空切片，不阻塞。首次采样只记基线，输出空。
func DiskIO() []DiskIOStat {
	parts, err := disk.Partitions(true)
	if err != nil {
		return []DiskIOStat{}
	}
	targets := filterMountTargets(parts)
	if len(targets) == 0 {
		return []DiskIOStat{}
	}
	counters, err := disk.IOCounters()
	if err != nil {
		return []DiskIOStat{}
	}

	now := time.Now()
	elapsed := 0.0
	if !lastDiskSampleTime.IsZero() {
		elapsed = now.Sub(lastDiskSampleTime).Seconds()
	}

	var result []DiskIOStat
	if lastDiskIOCounters != nil && elapsed > 0 {
		result = computeDiskIORates(targets, lastDiskIOCounters, counters, elapsed)
	}
	// 首次采样只记基线
	lastDiskIOCounters = counters
	lastDiskSampleTime = now
	if result == nil {
		result = []DiskIOStat{}
	}
	return result
}
```

- [x] **Step 4: 运行测试确认通过**

Run: `cd /Users/mango/project/katabump-probe/vps-monitor && go test ./monitoring/unit/ -run 'TestTrimDevPrefix|TestDiskSafeDelta|TestFilterMountTargets|TestComputeDiskIORates' -v`
Expected: 全部 PASS。

- [x] **Step 5: Commit**

```bash
cd /Users/mango/project/katabump-probe/vps-monitor
git add monitoring/unit/disk_io.go monitoring/unit/disk_io_test.go
git commit -m "feat(disk-io): 新增磁盘 IO 速率采集单元与单测"
```

---

### Task 2: 在报告结构中接入 `disk_io`

**Files:**
- Modify: `monitoring/monitoring.go:18`（report 结构加字段）、`monitoring/monitoring.go:92-93`（GenerateReport 填充）

**Interfaces:**
- Consumes: `unit.DiskIO()` → `[]unit.DiskIOStat`（Task 1 产出）
- Produces: `report.DiskIO []unit.DiskIOStat json:"disk_io,omitempty"`（最终 JSON 片段）

- [x] **Step 1: report 结构加字段**

`monitoring/monitoring.go` 的 `report` 结构，在 `Disk usageReport` 字段后加一行：

```go
	Disk        usageReport       `json:"disk"`
	DiskIO      []unit.DiskIOStat `json:"disk_io,omitempty"`
	Network     networkReport     `json:"network"`
```

- [x] **Step 2: GenerateReport 里填充**

`monitoring/monitoring.go` 第 92-93 行 `disk := unit.Disk()` / `data.Disk = ...` 之后加：

```go
	disk := unit.Disk()
	data.Disk = usageReport{Total: disk.Total, Used: disk.Used}

	data.DiskIO = unit.DiskIO()
```

- [x] **Step 3: 编译验证**

Run: `cd /Users/mango/project/katabump-probe/vps-monitor && go build ./...`
Expected: 无错误输出。

- [x] **Step 4: 快速冒烟（可选，本机有 /dev 属性时）**

Run: `cd /Users/mango/project/katabump-probe/vps-monitor && go run . --help 2>&1 | head -5 || go test ./monitoring/ -run TestDiskIONothing -count=1`
Expected: 编译通过即可；若本机无法跑 agent，跳过冒烟，以 `go build` 通过为准。

- [x] **Step 5: Commit**

```bash
cd /Users/mango/project/katabump-probe/vps-monitor
git add monitoring/monitoring.go
git commit -m "feat(disk-io): 上报磁盘 IO 速率到 report.disk_io"
```

---

### Task 3: 全量校验收尾

**Files:** 无（仅验证 + 提交）

- [x] **Step 1: go vet**

Run: `cd /Users/mango/project/katabump-probe/vps-monitor && go vet ./...`
Expected: 无输出（成功）。

- [x] **Step 2: 全量测试**

Run: `cd /Users/mango/project/katabump-probe/vps-monitor && go test ./monitoring/...`
Expected: 全部 PASS，无新增失败。

- [x] **Step 3: 确认 JSON 形状（可选自查）**

小脚本验证 `disk_io` 字段名与 `omitempty` 行为（当无 IO 或无物理盘时字段不出现）：

```bash
cd /Users/mango/project/katabump-probe/vps-monitor
cat <<'EOF' > /tmp/diskio_check.go
package main
// 仅用于人工核对字段名，不加入仓库
EOF
grep -n 'disk_io\|read_bytes\|write_bytes\|read_iops\|write_iops' monitoring/unit/disk_io.go
```
Expected: 四组字段名与 spec §7 完全一致。

- [x] **Step 4: 更新任务状态 + 提交（若 Task 1/2 已各自提交，本步仅确认 clean）**

```bash
cd /Users/mango/project/katabump-probe/vps-monitor
git status --short
git log --oneline -3
```
Expected: 工作区 clean，分支 `dev/disk-io` 上有 Task 1、Task 2 两个提交。

---

## Self-Review

**Spec 覆盖核对：**
- §4 新增采集单元 + 扩展 report → Task 1、Task 2 ✅
- §5 数据流（Partitions→IOCounters→delta/elapsed→DiskIO 数组） → Task 1 `filterMountTargets`/`computeDiskIORates` ✅
- §6 复用 `AGENT_INCLUDE_MOUNTPOINTS`、`HOST_PROC` 自动 → Global Constraints + `filterMountTargets` ✅
- §7 上报格式四字段 + `omitempty` 兼容 → Task 2 字段 tag ✅
- §8 错误处理（Partitions/IOCounters 失败→空；缺设备→跳过；首采样→空） → Task 1 `DiskIO()` ✅
- §9 测试清单（映射、delta/elapsed 缩放、首采/回绕、空配置默认物理盘） → Task 1 测试 ✅
- §10 不新造旗标、module path 不变、部署注意不在代码范围 → Global Constraints ✅

**Placeholder 扫描：** 无 TBD/TODO；所有代码步骤含完整代码。✅

**类型一致性：** `DiskIOStat` 字段名（Device/Mountpoint/ReadBytes/WriteBytes/ReadIOPS/WriteIOPS）与 JSON tag 在 Task 1 定义、Task 2 引用一致；`mountTarget`、`filterMountTargets`、`computeDiskIORates` 签名在测试与实现间一致。✅

**一处有意决策：** 速率字段用 `float64`（delta/elapsed 天然为小数），spec §7 示例用整数仅为示意；JSON 名 `read_bytes` 等与 spec 完全一致。