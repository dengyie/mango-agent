package monitoring

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	pkg_flags "github.com/komari-monitor/komari-agent/cmd/flags"
)

// 真实样本取自 home-win 官方 SRBMiner 3.6.3 /api/v2/status（2026-09-10）：
// 顶层 hashrate 为空对象，实值在 algorithms[0].hashrate——本测试锁定该形态。
const srbV2Sample = `{
  "rig_name":"SRBMiner-Multi-Rig",
  "miner_version":"3.6.3",
  "gpu_devices":[
    {"id":0,"device":"gpu0","vendor":"nvidia","model":"nvidia_geforce_rtx_3070",
     "fan_speed_percent":86,"core_clock":1455,"memory_clock":6801,
     "temperature":74,"asic_power":149,"off_temperature":100}
  ],
  "algorithms":[
    {"id":0,"name":"pearlhash",
     "pool":{"pool":"prl-eu.kryptex.network:7048","wallet":"krxXGNKMD4/home-win",
             "time_connected":"2026-09-10 02:15:01","uptime":11076,
             "difficulty":2097152.0,"last_job_received":8,"latency":232},
     "shares":{"total":64,"accepted":64,"rejected":0,"avg_find_time":175},
     "hashrate":{"1min":62403052604616.36,"1hr":62336128008473.77,
                 "6hr":60424119992895.62,
                 "cpu":{"total":0.0},
                 "gpu":{"gpu0":63404632916632.73,"total":63404632916632.73}},
     "gpu_accepted_shares":{"gpu0":64},
     "gpu_rejected_shares":{"gpu0":0},
     "gpu_compute_errors":{"gpu0":0},
     "gpu_efficiency":{"gpu0":425534449104.92}}
  ]
}`

func resetMinerState(t *testing.T) {
	t.Helper()
	minerMu.Lock()
	lastMinerStat = nil
	lastMinerFetch = time.Time{}
	minerFetchFails = 0
	minerMu.Unlock()
	t.Cleanup(func() {
		minerMu.Lock()
		lastMinerStat = nil
		lastMinerFetch = time.Time{}
		minerFetchFails = 0
		minerMu.Unlock()
	})
}

func TestMinerDisabledByDefault(t *testing.T) {
	resetMinerState(t)
	pkg_flags.GlobalConfig.MinerAPIUrl = ""
	defer func() { pkg_flags.GlobalConfig.MinerAPIUrl = "" }()

	if got := Miner(); got != nil {
		t.Fatalf("Miner() with empty URL = %#v, want nil", got)
	}
}

func TestMinerParsesSrbV2Status(t *testing.T) {
	resetMinerState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(srbV2Sample))
	}))
	defer srv.Close()
	pkg_flags.GlobalConfig.MinerAPIUrl = srv.URL + "/api/v2/status"
	defer func() { pkg_flags.GlobalConfig.MinerAPIUrl = "" }()

	stat := Miner()
	if stat == nil {
		t.Fatal("Miner() = nil, want stat")
	}
	if stat.Algorithm != "pearlhash" {
		t.Errorf("Algorithm = %q, want pearlhash", stat.Algorithm)
	}
	if stat.Pool != "prl-eu.kryptex.network:7048" {
		t.Errorf("Pool = %q", stat.Pool)
	}
	if stat.Wallet != "krxXGNKMD4/home-win" {
		t.Errorf("Wallet = %q", stat.Wallet)
	}
	if stat.Hashrate1Min != 62403052604616.36 {
		t.Errorf("Hashrate1Min = %v, want 62403052604616.36", stat.Hashrate1Min)
	}
	if stat.Hashrate1Hr != 62336128008473.77 {
		t.Errorf("Hashrate1Hr = %v", stat.Hashrate1Hr)
	}
	if stat.PowerW != 149 || stat.Temperature != 74 || stat.FanPercent != 86 {
		t.Errorf("GPU aggregation = %vW %v°C %v%%, want 149W 74°C 86%%", stat.PowerW, stat.Temperature, stat.FanPercent)
	}
	if stat.SharesValid != 64 || stat.SharesInvalid != 0 || stat.SharesTotal != 64 {
		t.Errorf("Shares = valid %d invalid %d total %d, want 64/0/64", stat.SharesValid, stat.SharesInvalid, stat.SharesTotal)
	}
	if stat.PoolLatency != 232 {
		t.Errorf("PoolLatency = %d, want 232", stat.PoolLatency)
	}
	if stat.HwErrors != 0 {
		t.Errorf("HwErrors = %d, want 0", stat.HwErrors)
	}
}

func TestMinerFallsBackToRecentCacheOnFailure(t *testing.T) {
	resetMinerState(t)
	var fail bool
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		f := fail
		mu.Unlock()
		if f {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(srbV2Sample))
	}))
	defer srv.Close()
	pkg_flags.GlobalConfig.MinerAPIUrl = srv.URL + "/api/v2/status"
	defer func() { pkg_flags.GlobalConfig.MinerAPIUrl = "" }()

	first := Miner()
	if first == nil {
		t.Fatal("first fetch = nil")
	}
	mu.Lock()
	fail = true
	mu.Unlock()
	if second := Miner(); second == nil {
		t.Fatal("fetch after immediate failure should fall back to cached stat")
	} else if second.Hashrate1Min != first.Hashrate1Min {
		t.Fatalf("cached stat mismatch: %v vs %v", second.Hashrate1Min, first.Hashrate1Min)
	}
}

func TestMinerRejectsEmptyAlgorithms(t *testing.T) {
	resetMinerState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"rig_name":"x","algorithms":[]}`))
	}))
	defer srv.Close()
	pkg_flags.GlobalConfig.MinerAPIUrl = srv.URL
	defer func() { pkg_flags.GlobalConfig.MinerAPIUrl = "" }()

	if got := Miner(); got != nil {
		t.Fatalf("Miner() with empty algorithms = %#v, want nil", got)
	}
}

// 并发调用 Miner()（HTTP 在锁外）必须无竞态：-race 下 10 goroutine 同时打一个
// 延迟响应的 server，断言结果要么是有效快照要么是缓存/nil，且无数据竞争。
func TestMinerConcurrentAccessIsRaceFree(t *testing.T) {
	resetMinerState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond) // 放大锁外 HTTP 窗口
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(srbV2Sample))
	}))
	defer srv.Close()
	pkg_flags.GlobalConfig.MinerAPIUrl = srv.URL + "/api/v2/status"
	defer func() { pkg_flags.GlobalConfig.MinerAPIUrl = "" }()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if stat := Miner(); stat != nil && stat.Hashrate1Min != 62403052604616.36 {
				t.Errorf("unexpected hashrate %v", stat.Hashrate1Min)
			}
		}()
	}
	wg.Wait()
}
