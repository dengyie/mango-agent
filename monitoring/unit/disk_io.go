package monitoring

import (
	"os"
	"path"
	"path/filepath"
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
// 白名单挂载点解析到虚拟设备(overlay/tmpfs 等,如容器根)且无任何可用目标时,
// 回退到默认物理盘遍历——这类根无 /dev/ 设备,IOCounters 查不到,但同机数据盘仍有 IO 可采。
func filterMountTargets(parts []disk.PartitionStat) []mountTarget {
	if flags.IncludeMountpoints != "" {
		byMount := make(map[string]string, len(parts))
		for _, p := range parts {
			byMount[p.Mountpoint] = p.Device
		}
		var targets []mountTarget
		hadVirtual := false
		for _, mp := range strings.Split(flags.IncludeMountpoints, ";") {
			mp = strings.TrimSpace(mp)
			if mp == "" {
				continue
			}
			dev, ok := byMount[mp]
			if !ok {
				continue // 挂载点在分区表里无对应设备，跳过
			}
			dev = resolvePhysicalDevice(mp, dev)
			if isVirtualDevice(dev) {
				hadVirtual = true // overlay/tmpfs 无磁盘计数键,记下待整体回退
				continue
			}
			targets = append(targets, mountTarget{Mountpoint: mp, Device: trimDevPrefix(dev)})
		}
		if len(targets) == 0 && hadVirtual {
			return fallbackPhysicalTargets(parts)
		}
		return targets
	}

	return defaultMountTargets(parts)
}

// fallbackPhysicalTargets 白名单挂载点全部落到虚拟设备(overlay 根)时的兜底:
// 按设备名收真实块设备并去重(保留路径最短的挂载点),保证同机数据盘的 IO 不落空。
// 不用 isPhysicalDisk——其 /tmp、/var/tmp 等挂载点前缀排除,会把容器内挂到 /tmp/mnt
// 的真实数据盘漏掉;IO 采集关心的是设备而不是挂载点用途。
func fallbackPhysicalTargets(parts []disk.PartitionStat) []mountTarget {
	seen := make(map[string]string) // device -> 最短 mountpoint
	for _, p := range parts {
		if !isRealBlockDevice(p.Device) {
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

// isRealBlockDevice 判断设备名是否为真实块设备(/dev/ 前缀且非 loop 虚拟盘)。
// overlay/tmpfs/none 等虚拟设备名无 /dev/ 前缀,自然被排除。
func isRealBlockDevice(dev string) bool {
	if !strings.HasPrefix(dev, "/dev/") {
		return false
	}
	if strings.HasPrefix(dev, "/dev/loop") {
		return false
	}
	return true
}

// defaultMountTargets 遍历所有物理盘并按设备去重(保留路径最短的根挂载点),与 Disk()/DiskList() 同一批设备。
func defaultMountTargets(parts []disk.PartitionStat) []mountTarget {
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

// isVirtualDevice 判断设备名是否为非块设备的虚拟文件系统(overlay/tmpfs/none/proc 等)。
// 这类名字在 /proc/diskstats 里没有计数键,IOCounters 查不到,IO 无法直接采集。
// 真块设备名(gopsutil Partitions 归一化后)一律以 /dev/ 开头。
func isVirtualDevice(dev string) bool {
	return !strings.HasPrefix(dev, "/dev/")
}

// resolvePhysicalDevice 尝试把虚拟设备名(overlay/tmpfs)解析成真实块设备。
// 已以 /dev/ 开头则原样返回。其余情况查 /proc/self/mountinfo 该挂载点的 major:minor,
// 再经 /sys/dev/block/<maj:min> readlink 得到真实设备名。任一步失败返回原 dev(行为不变)。
func resolvePhysicalDevice(mountpoint, dev string) string {
	if strings.HasPrefix(dev, "/dev/") {
		return dev
	}
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return dev
	}
	majmin := findMountinfoMajorMinor(string(raw), mountpoint)
	if majmin == "" {
		return dev
	}
	link, err := os.Readlink(filepath.Join("/sys/dev/block", majmin))
	if err != nil {
		return dev
	}
	name := path.Base(link)
	if name == "" || name == "." || name == "/" {
		return dev
	}
	return "/dev/" + name
}

// findMountinfoMajorMinor 解析 /proc/self/mountinfo 文本,返回 mountpoint 的 major:minor(如 8:5)。
// 挂载点按第 5 个字段精确匹配;major:minor 是第 3 个字段。找不到返回空串。
func findMountinfoMajorMinor(content, mountpoint string) string {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if fields[4] == mountpoint {
			return fields[2]
		}
	}
	return ""
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
