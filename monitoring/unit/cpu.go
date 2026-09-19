package monitoring

import (
	"bufio"
	"math"
	"os"
	"runtime"
	"strings"
	"time"

	pkg_flags "github.com/komari-monitor/komari-agent/cmd/flags"
	"github.com/shirou/gopsutil/v4/cpu"
)

var flags = pkg_flags.GlobalConfig

type CpuInfo struct {
	CPUName          string  `json:"cpu_name"`
	CPUArchitecture  string  `json:"cpu_architecture"`
	CPUCores         int     `json:"cpu_cores"`
	CPUPhysicalCores int     `json:"cpu_physical_cores"`
	CPUUsage         float64 `json:"cpu_usage"`
}

func Cpu() CpuInfo {
	cpuinfo := CpuStaticInfo()

	if flags.PreferCgroupLimits && runtime.GOOS == "linux" {
		if cg := ReadCgroupCPU(200 * time.Millisecond); cg.OK {
			if cg.QuotaCores > 0 {
				cores := int(math.Ceil(cg.QuotaCores))
				if cores < 1 {
					cores = 1
				}
				cpuinfo.CPUCores = cores
				cpuinfo.CPUPhysicalCores = cores
			}
			usage := cg.UsagePercent
			if usage > 100.0 {
				usage = 100.0
			} else if usage < 0.0 {
				usage = 0.0
			}
			cpuinfo.CPUUsage = usage
			return cpuinfo
		}
		if flags.ForceCPUQuotaCores > 0 {
			coresF := flags.ForceCPUQuotaCores
			cores := int(math.Ceil(coresF))
			if cores < 1 {
				cores = 1
			}
			cpuinfo.CPUCores = cores
			cpuinfo.CPUPhysicalCores = cores
			// Scale host-wide % into quota-relative %: host% * hostLogical / quotaCores
			hostN, _ := cpu.Counts(true)
			if hostN < 1 {
				hostN = 1
			}
			percentages, err := cpu.Percent(200*time.Millisecond, false)
			if err == nil && len(percentages) > 0 {
				scaled := percentages[0] * float64(hostN) / coresF
				if scaled > 100.0 {
					scaled = 100.0
				} else if scaled < 0.0 {
					scaled = 0.0
				}
				cpuinfo.CPUUsage = scaled
			}
			return cpuinfo
		}
	}

	percentages, err := cpu.Percent(0, false)
	if err == nil && len(percentages) > 0 {
		usage := percentages[0]
		if usage > 100.0 {
			usage = 100.0
		} else if usage < 0.0 {
			usage = 0.0
		}
		cpuinfo.CPUUsage = usage
	}

	return cpuinfo
}

func CpuStaticInfo() CpuInfo {
	cpuinfo := CpuInfo{
		CPUName:          "Unknown",
		CPUArchitecture:  runtime.GOARCH,
		CPUCores:         1,
		CPUPhysicalCores: 0, // 为兼容旧版 agent，0 表示未上报或未知，避免与实际核心数混淆
		CPUUsage:         0.0,
	}

	// 优先使用 gopsutil 获取 CPU 信息，避免触发 lscpu 在部分内核上的 lockdown 日志刷屏。
	info, err := cpu.Info()
	if err == nil && len(info) > 0 {
		cpuinfo.CPUName = strings.TrimSpace(info[0].ModelName)
		if cpuinfo.CPUName == "" {
			if info[0].VendorID != "" || info[0].Family != "" {
				cpuinfo.CPUName = strings.TrimSpace(info[0].VendorID + " " + info[0].Family)
			}
		}
	}

	if cpuinfo.CPUName == "Unknown" {
		name, err := readCPUNameFromProc()
		if err == nil && name != "" {
			cpuinfo.CPUName = strings.TrimSpace(name)
		}
	}

	if flags.PreferCgroupLimits && runtime.GOOS == "linux" {
		// Cheap quota-only read: zero sample window still parses cpu.max / cfs_quota.
		if cg := ReadCgroupCPU(0); cg.OK && cg.QuotaCores > 0 {
			cores := int(math.Ceil(cg.QuotaCores))
			if cores < 1 {
				cores = 1
			}
			cpuinfo.CPUCores = cores
			cpuinfo.CPUPhysicalCores = cores
			return cpuinfo
		}
		if flags.ForceCPUQuotaCores > 0 {
			cores := int(math.Ceil(flags.ForceCPUQuotaCores))
			if cores < 1 {
				cores = 1
			}
			cpuinfo.CPUCores = cores
			cpuinfo.CPUPhysicalCores = cores
			return cpuinfo
		}
	}

	cores, err := cpu.Counts(true)
	if err == nil && cores > 0 {
		cpuinfo.CPUCores = cores
	}

	physicalCores, err := cpu.Counts(false)
	if err == nil && physicalCores > 0 {
		cpuinfo.CPUPhysicalCores = physicalCores
	}

	return cpuinfo
}

// readCPUNameFromProc 从 /proc/cpuinfo 读取 CPU 名称
func readCPUNameFromProc() (string, error) {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Model\t") || strings.HasPrefix(line, "Hardware\t") || strings.HasPrefix(line, "Processor\t") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1]), nil
			}
		}
	}

	return "", scanner.Err()
}
