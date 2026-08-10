package monitoring

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CgroupLimits reports Linux cgroup memory/CPU quotas for the current process
// (Docker / Pterodactyl / LXC). When the cgroup is effectively unlimited, ok is false.
type CgroupMem struct {
	Limit uint64
	Usage uint64
	OK    bool
	Path  string
}

type CgroupCPU struct {
	// QuotaCores is CFS quota expressed in cores (e.g. 0.25 for 25% of one CPU).
	QuotaCores float64
	// UsagePercent is usage relative to the quota (0–100+), not host-wide %.
	UsagePercent float64
	OK           bool
	Path         string
}

const cgroupUnlimited = uint64(1) << 62

func readFirstExisting(paths ...string) (string, []byte, error) {
	var last error
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err == nil {
			return p, b, nil
		}
		last = err
	}
	return "", nil, last
}

func parseUintBytes(b []byte) (uint64, error) {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "max" {
		return 0, strconv.ErrSyntax
	}
	// cgroup v1 sometimes appends newline only; also handle "9223372036854771712"
	return strconv.ParseUint(s, 10, 64)
}

// resolveCgroupPaths finds the unified (v2) or controller-specific (v1) dirs for self.
func resolveCgroupV2Dir() string {
	// Prefer /sys/fs/cgroup if it has cgroup.controllers (unified)
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		// May still be hierarchical: read /proc/self/cgroup for relative path
		rel := selfCgroupRelUnified()
		if rel == "" || rel == "/" {
			return "/sys/fs/cgroup"
		}
		return filepath.Join("/sys/fs/cgroup", rel)
	}
	return ""
}

func selfCgroupRelUnified() string {
	f, err := os.Open("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// v2: 0::/path
		line := sc.Text()
		if strings.HasPrefix(line, "0::") {
			return strings.TrimPrefix(line, "0::")
		}
	}
	return ""
}

func selfCgroupV1Path(controller string) string {
	f, err := os.Open("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// 4:memory:/docker/xxx
		parts := strings.SplitN(sc.Text(), ":", 3)
		if len(parts) != 3 {
			continue
		}
		for _, c := range strings.Split(parts[1], ",") {
			if c == controller {
				rel := parts[2]
				if rel == "/" {
					return filepath.Join("/sys/fs/cgroup", controller)
				}
				return filepath.Join("/sys/fs/cgroup", controller, rel)
			}
		}
	}
	return ""
}

// ReadCgroupMemory returns cgroup memory limit + current usage when a finite quota exists.
func ReadCgroupMemory() CgroupMem {
	out := CgroupMem{}
	if dir := resolveCgroupV2Dir(); dir != "" {
		_, limB, err1 := readFirstExisting(filepath.Join(dir, "memory.max"))
		_, useB, err2 := readFirstExisting(filepath.Join(dir, "memory.current"))
		if err2 == nil {
			if use, err := parseUintBytes(useB); err == nil {
				out.Usage = use
				out.Path = dir
			}
		}
		if err1 == nil {
			limS := strings.TrimSpace(string(limB))
			if limS != "max" {
				if lim, err := parseUintBytes(limB); err == nil && lim > 0 && lim < cgroupUnlimited {
					out.Limit = lim
					out.OK = true
					return out
				}
			}
		}
		return out // may have Usage only
	}
	// v1
	dir := selfCgroupV1Path("memory")
	if dir == "" {
		dir = "/sys/fs/cgroup/memory"
	}
	_, limB, err1 := readFirstExisting(filepath.Join(dir, "memory.limit_in_bytes"))
	_, useB, err2 := readFirstExisting(filepath.Join(dir, "memory.usage_in_bytes"))
	if err2 == nil {
		if use, err := parseUintBytes(useB); err == nil {
			out.Usage = use
			out.Path = dir
		}
	}
	if err1 != nil {
		return out
	}
	lim, err := parseUintBytes(limB)
	if err != nil || lim == 0 || lim >= cgroupUnlimited {
		return out
	}
	out.Limit = lim
	out.OK = true
	return out
}

func readCPUQuota(dir string, v2 bool) (quotaUS, periodUS int64, ok bool) {
	if v2 {
		b, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
		if err != nil {
			return 0, 0, false
		}
		fields := strings.Fields(string(b))
		if len(fields) < 1 {
			return 0, 0, false
		}
		if fields[0] == "max" {
			return 0, 0, false
		}
		q, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || q <= 0 {
			return 0, 0, false
		}
		p := int64(100000)
		if len(fields) >= 2 {
			if pp, err := strconv.ParseInt(fields[1], 10, 64); err == nil && pp > 0 {
				p = pp
			}
		}
		return q, p, true
	}
	// v1
	qb, err1 := os.ReadFile(filepath.Join(dir, "cpu.cfs_quota_us"))
	pb, err2 := os.ReadFile(filepath.Join(dir, "cpu.cfs_period_us"))
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	q, err := strconv.ParseInt(strings.TrimSpace(string(qb)), 10, 64)
	if err != nil || q <= 0 { // -1 = unlimited
		return 0, 0, false
	}
	p, err := strconv.ParseInt(strings.TrimSpace(string(pb)), 10, 64)
	if err != nil || p <= 0 {
		return 0, 0, false
	}
	return q, p, true
}

func readCPUUsageUsec(dir string, v2 bool) (uint64, bool) {
	if v2 {
		b, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
		if err != nil {
			return 0, false
		}
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) == 2 && f[0] == "usage_usec" {
				v, err := strconv.ParseUint(f[1], 10, 64)
				return v, err == nil
			}
		}
		return 0, false
	}
	// v1 cpuacct
	b, err := os.ReadFile(filepath.Join(dir, "cpuacct.usage"))
	if err != nil {
		// sometimes usage is under cpuacct controller dir
		return 0, false
	}
	// nanoseconds
	ns, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return ns / 1000, true // -> usec
}

// ReadCgroupCPU samples cgroup CPU usage against CFS quota.
// sampleWindow <= 0 means quota-only (no sleep); >0 samples usage over that window.
func ReadCgroupCPU(sampleWindow time.Duration) CgroupCPU {
	out := CgroupCPU{}
	var dir string
	var v2 bool
	if d := resolveCgroupV2Dir(); d != "" {
		dir, v2 = d, true
	} else {
		dir = selfCgroupV1Path("cpu")
		if dir == "" {
			dir = "/sys/fs/cgroup/cpu"
		}
		v2 = false
	}

	q, period, ok := readCPUQuota(dir, v2)
	usageDir := dir
	if !ok && !v2 {
		// split cpu / cpuacct controllers
		if q2, p2, ok2 := readCPUQuota(dir, false); ok2 {
			q, period, ok = q2, p2, true
		}
		acct := selfCgroupV1Path("cpuacct")
		if acct == "" {
			acct = "/sys/fs/cgroup/cpuacct"
		}
		usageDir = acct
	}
	if !ok {
		return out
	}
	cores := float64(q) / float64(period)
	out.QuotaCores = cores
	out.Path = usageDir
	out.OK = true
	if sampleWindow <= 0 {
		return out
	}

	u1, ok1 := readCPUUsageUsec(usageDir, v2)
	if !ok1 && !v2 {
		u1, ok1 = readCPUUsageUsec(dir, false)
	}
	if !ok1 {
		return out
	}
	t0 := time.Now()
	time.Sleep(sampleWindow)
	u2, ok2 := readCPUUsageUsec(usageDir, v2)
	if !ok2 && !v2 {
		u2, ok2 = readCPUUsageUsec(dir, false)
	}
	if !ok2 || u2 < u1 {
		return out
	}
	elapsed := time.Since(t0).Seconds()
	if elapsed <= 0 || cores <= 0 {
		return out
	}
	deltaSec := float64(u2-u1) / 1e6
	out.UsagePercent = (deltaSec / (elapsed * cores)) * 100
	return out
}
