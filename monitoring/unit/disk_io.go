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