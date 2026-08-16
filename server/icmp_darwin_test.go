//go:build darwin

package server

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSystemPingLocalhost(t *testing.T) {
	lat, err := systemPing("127.0.0.1", 2*time.Second)
	if err != nil {
		t.Fatalf("systemPing localhost failed: %v", err)
	}
	if lat < 0 {
		t.Fatalf("negative latency: %d", lat)
	}
}

func TestSystemPingUnreachable(t *testing.T) {
	// 192.0.2.1 = TEST-NET-1，按规范不应回显；若网络环境受限导致 ping 进程都无法起（无 /sbin/ping）则跳过
	_, err := systemPing("192.0.2.1", 2*time.Second)
	if err == nil {
		t.Skip("test-net host unexpectedly reachable")
	}
}

func TestSystemPingOutputReal(t *testing.T) {
	// 直接跑一次系统 ping 抓真实输出，确保解析正则以 macOS 实际格式吻合（版本差异最容易翻车）。
	// 家宽 CF 链路有真实随机丢包（文档记 33-40%，可波动到 0%），故不硬性断言必须 1/1，
	// 而是验证两种分支（有回包原文案 RTT / 无回包则无 RTT 行）的解析行为自洽。
	cmd := exec.Command("/sbin/ping", "-n", "-c", "1", "-W", "3000", "1.1.1.1")
	out, _ := cmd.CombinedOutput()
	output := string(out)
	t.Logf("real ping output:\n%s", output)
	if !strings.Contains(output, "packets transmitted") {
		t.Skip("no statistics section; network unreachable?")
	}
	recv := pingPacketsReceived(output)
	t.Logf("received=%d", recv)
	if recv == 0 {
		if _, ok := pingAvgRttMs(output); ok {
			t.Fatalf("rtt parsed despite 0 received:\n%s", output)
		}
		t.Logf("offline sample (recv=0): RTT line absent as expected")
		return
	}
	if lat, ok := pingAvgRttMs(output); !ok {
		t.Fatalf("failed to parse real RTT line:\n%s", output)
	} else {
		t.Logf("parsed RTT=%d ms", lat)
	}
}

func TestPingPacketParse(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"1 packets transmitted, 1 packets received, 0.0% packet loss", 1},
		{"3 packets transmitted, 3 packets received, 0.0% packet loss", 3},
		{"1 packets transmitted, 0 packets received, +1 errors, 100.0% packet loss", 0},
		{"100.0% packet loss", 0},
		{"", 0},
	}
	for _, c := range cases {
		if got := pingPacketsReceived(c.in); got != c.want {
			t.Errorf("pingPacketsReceived(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestPingRttParse(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"round-trip min/avg/max/stddev = 35.339/35.999/36.369/0.468 ms", 35, true},
		{"round-trip min/avg/max/stddev = 44.898/44.898/44.898/nan ms", 44, true},
		{"round-trip min/avg/max/stddev = 0.117/0.130/0.141/0.007 s", 130, true},
		{"no stats", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := pingAvgRttMs(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("pingAvgRttMs(%q) = %d,%v want %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}