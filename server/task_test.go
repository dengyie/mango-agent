package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

var testTargets = []struct {
	target string
}{
	{"v6-sh-cm.oojj.de"},
	{"2409:8c1e:8f80:2:6a::"},
	{"[2409:8c1e:8f80:2:6a::]"},
	{"[2409:8c1e:8f80:2:6a::]:80"},
	{"v4-sh-cm.oojj.de"},
	{"117.185.125.154"},
	{"117.185.125.154:80"},
}

func TestICMPPing(t *testing.T) {
	timeout := 3 * time.Second
	for _, tt := range testTargets {
		t.Run(tt.target, func(t *testing.T) {
			latency, err := icmpPing(tt.target, timeout)
			if latency < -1 {
				t.Errorf("ICMP ping %s: invalid latency %d", tt.target, latency)
			}
			if err != nil {
				t.Errorf("ICMP ping %s error: %v", tt.target, err)
			}
		})
	}
}

func TestTCPPing(t *testing.T) {
	timeout := 3 * time.Second
	for _, tt := range testTargets {
		t.Run(tt.target, func(t *testing.T) {
			latency, err := tcpPing(tt.target, timeout)
			if latency < -1 {
				t.Errorf("TCP ping %s: invalid latency %d", tt.target, latency)
			}
			if err != nil {
				t.Errorf("TCP ping %s error: %v", tt.target, err)
			}
		})
	}
}

func TestHTTPPing(t *testing.T) {
	timeout := 3 * time.Second
	for _, tt := range testTargets {
		t.Run(tt.target, func(t *testing.T) {
			latency, err := httpPing(tt.target, timeout)
			if latency < -1 {
				t.Errorf("HTTP ping %s: invalid latency %d", tt.target, latency)
			}
			if err != nil {
				t.Errorf("HTTP ping %s error: %v", tt.target, err)
			}
		})
	}
}

func TestUploadTaskResultRetriesOnTransientFailure(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		att := atomic.AddInt32(&attempts, 1)
		if att < 2 {
			http.Error(w, "temporary error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{"status":"success"}}`))
	}))
	defer server.Close()

	oldEndpoint := flags.Endpoint
	oldToken := flags.Token
	flags.Endpoint = server.URL
	flags.Token = "test-token"
	defer func() {
		flags.Endpoint = oldEndpoint
		flags.Token = oldToken
	}()

	uploadTaskResult("task-123", "success", 0, time.Now())

	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("expected 2 attempts due to retry, got %d", got)
	}
}
