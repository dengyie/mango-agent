package monitoring

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 真实样本取自 home-win SRBMiner-MULTI /api/v2/status：
// 顶层 hashrate 为空对象，实值在 algorithms[0].hashrate——本测试锁定该形态。
const srbV2Sample = `{
  "rig_name":"SRBMiner-Multi-Rig",
  "miner_version":"3.6.4",
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

func TestMinerNilBeforeAnyCollect(t *testing.T) {
	resetMinerState(t)
	if got := Miner(); got != nil {
		t.Fatalf("Miner() before any collection = %#v, want nil", got)
	}
}

func TestMinerParsesSrbStatus(t *testing.T) {
	resetMinerState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(srbV2Sample))
	}))
	defer srv.Close()

	collectMinerOnce(srv.URL + "/api/v2/status")

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
	// SharesStale 不应出现在线上 JSON 里（SRBMiner 不提供，硬编 0 是假数据）
	b, err := json.Marshal(stat)
	if err != nil {
		t.Fatalf("marshal MinerStat: %v", err)
	}
	if len(b) > 0 { // avoid unused import lint if build-tag changes
		if _, ok := marshalHasKey(string(b), "shares_stale"); ok {
			t.Error("MinerStat wire JSON must not contain shares_stale (fabricated 0)")
		}
	}
	if stat.PoolLatency != 232 {
		t.Errorf("PoolLatency = %d, want 232", stat.PoolLatency)
	}
	if stat.HwErrors != 0 {
		t.Errorf("HwErrors = %d, want 0", stat.HwErrors)
	}
}

// marshalHasKey 判断一段 JSON 字符串里是否含指定 key。
func marshalHasKey(j, key string) (string, bool) {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(j), &m); err != nil {
		return "", false
	}
	_, ok := m[key]
	return "", ok
}

func TestMinerKeepsRecentCacheAfterCollectFailure(t *testing.T) {
	resetMinerState(t)
	var fail bool
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	collectMinerOnce(srv.URL + "/api/v2/status")
	first := Miner()
	if first == nil {
		t.Fatal("first collect = nil")
	}
	mu.Lock()
	fail = true
	mu.Unlock()
	resetMinerFetchTime() // 模拟刚失败但缓存仍在 freshness 窗口内
	if second := Miner(); second == nil {
		t.Fatal("Miner() after a failed collect should still return recent cache")
	} else if second.Hashrate1Min != first.Hashrate1Min {
		t.Fatalf("cached stat mismatch: %v vs %v", second.Hashrate1Min, first.Hashrate1Min)
	}
}

// resetMinerFetchTime 把 lastMinerFetch 设为当前时间，等价于一次成功刚采集。
func resetMinerFetchTime() {
	minerMu.Lock()
	lastMinerFetch = time.Now()
	minerFetchFails = 0
	minerMu.Unlock()
}

func TestMinerRejectsEmptyAlgorithms(t *testing.T) {
	resetMinerState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"rig_name":"x","algorithms":[]}`))
	}))
	defer srv.Close()

	collectMinerOnce(srv.URL)
	if got := Miner(); got != nil {
		t.Fatalf("Miner() with empty algorithms = %#v, want nil", got)
	}
}

// Miner() 是纯缓存读，绝不应在内部发 HTTP。用「首次正常、其后永久挂起」的 server 验证：
// 采集成功拿到缓存后，调用 Miner()；若它内部再发起 HTTP，会命中挂起 handler 而被卡住，
// 同时 entered 计数会超过 1，测试既能检测阻塞也能检测「多发了一次请求」。
func TestMinerNonBlocking_noHTTPInReadPath(t *testing.T) {
	resetMinerState(t)
	var entered int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&entered, 1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(srbV2Sample))
			return
		}
		// 第二次及以后：挂起连接，直到 server 关闭。
		<-time.After(10 * time.Second)
	}))

	collectMinerOnce(srv.URL + "/api/v2/status")
	if Miner() == nil {
		srv.Close()
		t.Fatal("collect should produce a cached stat")
	}

	done := make(chan struct{})
	go func() {
		_ = Miner()
		close(done)
	}()
	select {
	case <-done:
		// ok：Miner() 立即返回
	case <-time.After(500 * time.Millisecond):
		srv.Close()
		t.Fatal("Miner() blocked — it did network IO in the read path")
	}
	srv.Close()
	if n := atomic.LoadInt32(&entered); n != 1 {
		t.Fatalf("Miner() triggered %d extra HTTP requests (expected 0)", n-1)
	}
}

// 后台并发采集 + 并发读 Miner() 无竞态：-race 下 10 goroutine 同时打 server，
// 再 10 goroutine 同时读缓存。
func TestMinerConcurrentCollectReadIsRaceFree(t *testing.T) {
	resetMinerState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond) // 放大并发窗口
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(srbV2Sample))
	}))
	defer srv.Close()
	url := srv.URL + "/api/v2/status"

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			collectMinerOnce(url)
		}()
	}
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
