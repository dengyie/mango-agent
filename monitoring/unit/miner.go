package monitoring

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	pkg_flags "github.com/komari-monitor/komari-agent/cmd/flags"
)

// MinerStat 一次矿工状态快照（SRBMiner-MULTI /api/v2/status 归一化，跨版本兼容）。
// 只取第一个算法的聚合视图；hashrate 单位 H/s，power W，温度 °C。
type MinerStat struct {
	Algorithm     string  `json:"algorithm"`
	Pool          string  `json:"pool"`
	Wallet        string  `json:"wallet"`
	Hashrate1Min  float64 `json:"hashrate_1min"`
	Hashrate1Hr   float64 `json:"hashrate_1hr"`
	PowerW        float64 `json:"power_w"`
	Temperature   float64 `json:"temperature"`
	FanPercent    float64 `json:"fan_percent"`
	SharesTotal   int64   `json:"shares_total"`
	SharesValid   int64   `json:"shares_valid"`
	SharesStale   int64   `json:"shares_stale"`
	SharesInvalid int64   `json:"shares_invalid"`
	HwErrors      int64   `json:"hw_errors"`
	PoolLatency   int64   `json:"pool_latency"`
}

// srbStatus 是 SRBMiner /api/v2/status 的响应子集；多余字段忽略。
type srbStatus struct {
	GPUDevices []struct {
		Model           string  `json:"model"`
		Temperature     float64 `json:"temperature"`
		AsicPower       float64 `json:"asic_power"`
		FanSpeedPercent float64 `json:"fan_speed_percent"`
	} `json:"gpu_devices"`
	Algorithms []struct {
		Name string `json:"name"`
		Pool struct {
			Pool    string `json:"pool"`
			Wallet  string `json:"wallet"`
			Latency int64  `json:"latency"`
		} `json:"pool"`
		Shares struct {
			Total    int64 `json:"total"`
			Accepted int64 `json:"accepted"`
			Rejected int64 `json:"rejected"`
		} `json:"shares"`
		// 顶层窗口 "1min"/"1hr"/"6hr" 是裸数值（H/s）；"cpu"/"gpu" 子对象里才是
		// 按设备的 map。用 json.RawMessage 兼容两种形态。
		Hashrate struct {
			OneMin json.RawMessage `json:"1min"`
			OneHr  json.RawMessage `json:"1hr"`
		} `json:"hashrate"`
		GPUComputeErrors map[string]int64 `json:"gpu_compute_errors"`
	} `json:"algorithms"`
}

var (
	minerHTTPClient = &http.Client{Timeout: 4 * time.Second}
	minerMu         sync.Mutex
	lastMinerStat   *MinerStat
	lastMinerFetch  time.Time
	minerFetchFails int
)

// maxMinerStaleSec 缓存最长保鲜：即使矿工 API 刚好超时，也回退到最近一次成功快照，
// 避免上报抖动导致曲线断点。超过该时限则返回 nil（本次不报 mining）。
const maxMinerStaleSec = 60

// Miner 采集 SRBMiner 统计 API。AGENT_MINER_API_URL 未配置时直接返回 nil（不上报）。
// 每次调用允许一次实时 HTTP 拉取（agent 上报间隔通常 ≥3s，矿工 API 是本机回环，开销可忽略）；
// 拉取失败回退最近成功缓存（≤60s 内），连败计数到阈值后放弃缓存不再兜底。
func Miner() *MinerStat {
	url := pkg_flags.GlobalConfig.MinerAPIUrl
	if url == "" {
		return nil
	}
	minerMu.Lock()
	defer minerMu.Unlock()

	stat, err := fetchMinerStat(url)
	if err != nil {
		minerFetchFails++
		if lastMinerStat != nil &&
			time.Since(lastMinerFetch) <= maxMinerStaleSec*time.Second &&
			minerFetchFails <= 5 {
			return lastMinerStat
		}
		return nil
	}
	minerFetchFails = 0
	lastMinerStat = stat
	lastMinerFetch = time.Now()
	return stat
}

func fetchMinerStat(rawURL string) (*MinerStat, error) {
	resp, err := minerHTTPClient.Get(rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("miner api status %d", resp.StatusCode)
	}
	var s srbStatus
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, err
	}
	return normalizeSrbStatus(&s)
}

// normalizeSrbStatus 把 SRBMiner v2 状态归一化为 MinerStat。
// hashrate 窗口 "1min"/"1hr" 官方 3.6.3 为裸数值（旧版可能为对象），两种形态都兼容。
func normalizeSrbStatus(s *srbStatus) (*MinerStat, error) {
	if len(s.Algorithms) == 0 {
		return nil, fmt.Errorf("miner api returned no algorithms")
	}
	alg := s.Algorithms[0]

	oneMin := rawMessageHash(alg.Hashrate.OneMin)
	oneHr := rawMessageHash(alg.Hashrate.OneHr)
	if oneMin == 0 && oneHr == 0 {
		return nil, fmt.Errorf("miner api returned no hashrate")
	}

	var hwErr int64
	for _, v := range alg.GPUComputeErrors {
		hwErr += v
	}

	// GPU 聚合：取第一块核心矿卡（多卡场景温度/功耗取主卡，避免均值失真）。
	var temp, power, fan float64
	if len(s.GPUDevices) > 0 {
		temp = s.GPUDevices[0].Temperature
		power = s.GPUDevices[0].AsicPower
		fan = s.GPUDevices[0].FanSpeedPercent
	}

	return &MinerStat{
		Algorithm:     alg.Name,
		Pool:          alg.Pool.Pool,
		Wallet:        alg.Pool.Wallet,
		Hashrate1Min:  oneMin,
		Hashrate1Hr:   oneHr,
		PowerW:        power,
		Temperature:   temp,
		FanPercent:    fan,
		SharesTotal:   alg.Shares.Total,
		SharesValid:   alg.Shares.Accepted,
		SharesStale:   0, // SRBMiner API v2 无独立 stale 计数（=0 占位，Kryptex 面板侧另有统计）
		SharesInvalid: alg.Shares.Rejected,
		HwErrors:      hwErr,
		PoolLatency:   alg.Pool.Latency,
	}, nil
}

// rawMessageHash 从 hashrate 窗口值提取聚合 H/s：裸数值直接取；
// 对象形态优先 "total" 键，其次任意非零值。
func rawMessageHash(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	var num float64
	if err := json.Unmarshal(raw, &num); err == nil {
		return num
	}
	var m map[string]float64
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0
	}
	if v, ok := m["total"]; ok && v > 0 {
		return v
	}
	for _, v := range m {
		if v > 0 {
			return v
		}
	}
	return 0
}
