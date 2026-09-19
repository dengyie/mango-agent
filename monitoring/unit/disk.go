package monitoring

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
)

type DiskInfo struct {
	Total uint64 `json:"total"`
	Used  uint64 `json:"used"`
}

func Disk() DiskInfo {
	diskinfo := DiskInfo{}
	// 获取所有分区，使用 true 避免物理磁盘被 gopsutil 错误排除
	usage, err := disk.Partitions(true)
	if err != nil {
		diskinfo.Total = 0
		diskinfo.Used = 0
	} else {
		// 如果指定了自定义挂载点，只统计指定的挂载点
		if flags.IncludeMountpoints != "" {
			includeMounts := strings.Split(flags.IncludeMountpoints, ";")
			for _, mountpoint := range includeMounts {
				mountpoint = strings.TrimSpace(mountpoint)
				if mountpoint != "" {
					u, err := disk.Usage(mountpoint)
					if err != nil {
						continue
					} else {
						diskinfo.Total += u.Total
						diskinfo.Used += u.Used
					}
				}
			}
		} else {
			// 使用默认逻辑，排除临时文件系统和网络驱动器
			deviceMap := make(map[string]*disk.UsageStat)

			for _, part := range usage {
				if isPhysicalDisk(part) {
					u, err := disk.Usage(part.Mountpoint)
					if err != nil {
						continue
					}

					deviceID := part.Device
					// ZFS去重: 基于 pool 名称 (例如 pool/dataset -> pool)
					if strings.ToLower(part.Fstype) == "zfs" {
						if idx := strings.Index(deviceID, "/"); idx != -1 {
							deviceID = deviceID[:idx]
						}
					}

					// 如果该设备已存在，且当前挂载点的 Total 更大，则替换（处理 quota 等情况）
					// 否则保留现有的（通常我们希望统计物理 pool 的总量）
					if existing, ok := deviceMap[deviceID]; ok {
						if u.Total > existing.Total {
							deviceMap[deviceID] = u
						}
					} else {
						deviceMap[deviceID] = u
					}
				}
			}

			for _, u := range deviceMap {
				diskinfo.Total += u.Total
				diskinfo.Used += u.Used
			}
		}
	}
	// ForceDiskTotal (panel quota) 覆盖 Total（面板配额，字节）。
	// Pterodactyl/容器下 /home/container 的 statfs 报告的是宿主 overlay 的
	// Total/Used（虚高 ~百 GiB），既不等于面板配额，Used 也不是容器目录真实占用。
	// 面板本身用 `du -sb <mount>` 取真实占用（apparent size）。故 force 时：
	//   Total = 面板配额；Used = include-mountpoint 目录实际占用（walk，等价 du -sb）。
	// 目录 walk 失败或未指定单一挂载点时，退回到把 statfs Used 截断到配额。
	if flags.ForceDiskTotal > 0 {
		diskinfo.Total = flags.ForceDiskTotal
		if used, ok := forcedDiskUsed(); ok {
			diskinfo.Used = used
		}
		if diskinfo.Used > flags.ForceDiskTotal {
			diskinfo.Used = flags.ForceDiskTotal
		}
	}
	return diskinfo
}

// forcedDiskUsed 计算 force 模式下的真实磁盘占用（字节，apparent size，等价 du -sb）。
// 优先用 AGENT_INCLUDE_MOUNTPOINTS 的第一个挂载点作为根目录；未指定则回退。
// 返回 (used, ok)；ok=false 时调用方沿用 statfs 结果。
//
// 带 60s TTL 缓存：agent 默认每 3s 采集一次（Interval=3），而目录 walk 是
// 同步阻塞在采集循环里的。缓存避免高频全目录扫描拖慢上报周期；目录变化
// 最多延迟 60s 反映（对配额监控可接受）。挂载点变更时 key 变化自动失效。
func forcedDiskUsed() (uint64, bool) {
	root := ""
	if flags.IncludeMountpoints != "" {
		for _, mp := range strings.Split(flags.IncludeMountpoints, ";") {
			mp = strings.TrimSpace(mp)
			if mp != "" {
				root = mp
				break
			}
		}
	}
	if root == "" {
		return 0, false
	}
	return dirApparentSizeCached(root)
}

const diskWalkTTL = 60 * time.Second

var (
	diskWalkMu   sync.Mutex
	diskWalkRoot string
	diskWalkUsed uint64
	diskWalkOK   bool
	diskWalkAt   time.Time
)

// dirApparentSizeCached 带 TTL 缓存的目录大小计算；并发安全。
func dirApparentSizeCached(root string) (uint64, bool) {
	diskWalkMu.Lock()
	defer diskWalkMu.Unlock()
	if diskWalkRoot == root && !diskWalkAt.IsZero() && time.Since(diskWalkAt) < diskWalkTTL {
		return diskWalkUsed, diskWalkOK
	}
	used, ok := dirApparentSize(root)
	diskWalkRoot, diskWalkUsed, diskWalkOK, diskWalkAt = root, used, ok, time.Now()
	return used, ok
}

// dirApparentSize 递归累加目录下常规文件的 apparent size（等价 du -sb）。
// 不跟随符号链接，跳过无法读取的项，避免因单个错误中断统计。
func dirApparentSize(root string) (uint64, bool) {
	var total uint64
	walked := false
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// 跳过不可访问项（权限/竞态），不整体失败
			return nil
		}
		walked = true
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		// 只统计常规文件（排除符号链接/设备/socket）
		if info.Mode().IsRegular() {
			total += uint64(info.Size())
		}
		return nil
	})
	if err != nil || !walked {
		return 0, false
	}
	return total, true
}

// isPhysicalDisk 判断分区是否为物理磁盘
func isPhysicalDisk(part disk.PartitionStat) bool {
	// 对于LXC等基于loop的根文件系统，始终包含根挂载点
	if part.Mountpoint == "/" {
		return true
	}
	mountpoint := strings.ToLower(part.Mountpoint)
	// 排除挂载点
	var mountpointsToExcludePerfix = []string{
		"/tmp",
		"/var/tmp",
		"/dev",
		"/run",
		"/var/lib/containers",
		"/var/lib/docker",
		"/proc",
		"/sys",
		"/sys/fs/cgroup",
		"/etc/resolv.conf",
		"/etc/host", // /etc/hosts,/etc/hostname
		"/nix/store",
	}
	for _, mp := range mountpointsToExcludePerfix {
		if mountpoint == mp || strings.HasPrefix(mountpoint, mp) {
			return false
		}
	}

	fstype := strings.ToLower(part.Fstype)

	// 针对 Linux autofs：排除自动挂载的 trigger，真实文件系统会作为单独分区出现不会被排除。
	// 将 autofs 视为“非物理磁盘”可以避免重复统计容量。
	if fstype == "autofs" && !strings.HasPrefix(part.Device, "/dev/") {
		return false
	}

	// 针对 Linux 下通过 ntfs-3g 挂载的 NTFS 分区 (fuseblk)，这是实际物理磁盘，不应排除
	if fstype == "fuseblk" {
		return true
	}

	// Android 的 /sdcard 通过 FUSE (/dev/fuse) 挂载真实存储，不是网络文件系统。
	// 限定挂载点，避免将其他 FUSE 文件系统计入磁盘统计。
	if part.Device == "/dev/fuse" && fstype == "fuse" && mountpoint == "/sdcard" {
		return true
	}

	var fstypeToExclude = []string{
		"tmpfs",
		"devtmpfs",
		"udev",
		"nfs",
		"cifs",
		"smb",
		"vboxsf",
		"9p",
		"fuse",
		"overlay",
		"proc",
		"devpts",
		"sysfs",
		"cgroup",
		"mqueue",
		"hugetlbfs",
		"debugfs",
		"binfmt_misc",
		"securityfs",
		"nullfs",
	}
	for _, fs := range fstypeToExclude {
		if fstype == fs || strings.HasPrefix(fstype, fs) {
			return false
		}
	}
	// Windows 网络驱动器通常是映射盘符，但不容易通过fstype判断
	// 可以通过opts判断，Windows网络驱动通常有相关选项
	optsStr := strings.ToLower(strings.Join(part.Opts, ","))
	if strings.Contains(optsStr, "remote") || strings.Contains(optsStr, "network") {
		return false
	}

	// 虚拟内存
	if strings.HasPrefix(part.Device, "/dev/loop") {
		return false
	}

	return true
}

func DiskList() ([]string, error) {
	diskList := []string{}
	if flags.IncludeMountpoints != "" {
		includeMounts := strings.Split(flags.IncludeMountpoints, ";")
		for _, mountpoint := range includeMounts {
			mountpoint = strings.TrimSpace(mountpoint)
			if mountpoint != "" {
				diskList = append(diskList, mountpoint)
			}
		}
	} else {
		usage, err := disk.Partitions(true)
		if err != nil {
			return nil, err
		}

		// 同一物理设备只保留路径最短的根挂载点
		deviceMap := make(map[string]disk.PartitionStat)
		for _, part := range usage {
			if isPhysicalDisk(part) {
				deviceID := part.Device
				// ZFS去重: 基于 pool 名称
				if strings.ToLower(part.Fstype) == "zfs" {
					if idx := strings.Index(deviceID, "/"); idx != -1 {
						deviceID = deviceID[:idx]
					}
				}

				if existing, ok := deviceMap[deviceID]; ok {
					// 优先保留路径更短的挂载点 (e.g., /volume1 优于 /volume1/@appdata/...)
					if len(part.Mountpoint) < len(existing.Mountpoint) {
						deviceMap[deviceID] = part
					}
				} else {
					deviceMap[deviceID] = part
				}
			}
		}

		for _, part := range deviceMap {
			diskList = append(diskList, fmt.Sprintf("%s (%s)", part.Mountpoint, part.Fstype))
		}
	}
	return diskList, nil
}
