package monitoring

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// MinerStat 一次矿工状态快照（SRBMiner-MULTI /api/v2/status 归一化，跨版本兼容）。
// 只取第一个算法的聚合视图；hashrate 单位 H/s，power W，温度 °C。
//
// 注意：没有 SharesStale 字段。SRBMiner /api/v2/status 不暴露独立 stale 计数，
// 硬编码 0 会向 hub 和面板上报一个「看似健康」的假份额，故不产出该字段（缺失=未知）。
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

// ---------------------------------------------------------------------------
// 后台采集者架构
//
// 早期实现里 Miner() 在每次上报（GenerateReport 热路径）同步发起 HTTP GET。
// 单个 4s 超时的请求会把整条上报链路拖住——默认上报间隔 3s < 单次超时 4s，矿工
// API 一旦慢/挂，网络/CPU/内存等全部核心指标都要陪着等满超时。
//
// 根因修复：把矿工采集移到一个独立后台 goroutine（StartMinerCollector），按
// minerCollectionInterval 轮询并写入共享快照；Miner() 只做非阻塞读缓存。上报热
// 路径从此不再有任何网络 IO，矿工 API 的慢/挂只影响矿工字段本身。
// ---------------------------------------------------------------------------

const (
	// minerRequestTimeout 单次矿工 API 请求超时。只发生在后台采集 goroutine，不影响上报热路径。
	minerRequestTimeout = 4 * time.Second
	// minerCollectionInterval 后台采集轮询间隔。份额通常每 15-30 分钟才变化，5s 足够新鲜。
	minerCollectionInterval = 5 * time.Second
	// minerStaleAfter 快照年龄上限：超过该时长仍未成功采集则视为过期。
	minerStaleAfter = 60 * time.Second
	// minerMaxFails 连续失败超过该次后，放弃回报过期快照（等同旧实现的连败阈值）。
	minerMaxFails = 5
)

var (
	minerHTTPClient = &http.Client{Timeout: minerRequestTimeout}
	minerMu         sync.Mutex
	lastMinerStat   *MinerStat
	lastMinerFetch  time.Time
	minerFetchFails int
)

// StartMinerCollector 在后台轮询 SRBMiner API 并维护最新快照。url 为空时不启动（无上报）。
// 应在 agent 初始化阶段调用一次；goroutine 随进程常驻，进程退出即回收（无需显式 stop）。
func StartMinerCollector(url string) {
	if url == "" {
		return
	}
	go func() {
		log.Printf("[miner] collector started, polling %s every %s", url, minerCollectionInterval)
		collectMinerOnce(url) // 立即拉一次，避免首个周期在上报前无数据
		ticker := time.NewTicker(minerCollectionInterval)
		defer ticker.Stop()
		for range ticker.C {
			collectMinerOnce(url)
		}
	}()
}

// collectMinerOnce 拉取一次最新状态并写入共享缓存。失败仅更新失败计数并打日志（限流），
// 保留最近一次成功快照；是否返回由 Miner() 按 minerStaleAfter / minerMaxFails 判定。
func collectMinerOnce(url string) {
	stat, err := fetchMinerStat(url)
	minerMu.Lock()
	defer minerMu.Unlock()
	if err != nil {
		minerFetchFails++
		// 每次失败都打日志太吵，只在阈值附近打（首批 + 达到阈值那次）。
		if minerFetchFails <= minerMaxFails || minerFetchFails == minerMaxFails+1 {
			log.Printf("[miner] fetch failed (%d consecutive): %v", minerFetchFails, err)
		}
		return
	}
	minerFetchFails = 0
	lastMinerStat = stat
	lastMinerFetch = time.Now()
}

// Miner returns 最近一次成功采集的矿工快照（只读缓存读，无任何网络 IO，非阻塞）。
// 未采集过、或快照已过期（>minerStaleAfter 或连败>minerMaxFails）时返回 nil。
func Miner() *MinerStat {
	minerMu.Lock()
	defer minerMu.Unlock()
	if lastMinerStat == nil {
		return nil
	}
	if time.Since(lastMinerFetch) > minerStaleAfter || minerFetchFails > minerMaxFails {
		return nil
	}
	return lastMinerStat
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
// 本产品面向单算法矿机（如 XEL）：只归一化 Algorithms[0]，多算法矿机会静默只报第一个；
// 若日后需要再扩展为按算法分桶上报。
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
