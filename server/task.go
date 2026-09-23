package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/komari-monitor/komari-agent/dnsresolver"
	v2 "github.com/komari-monitor/komari-agent/protocol/v2"
	"github.com/komari-monitor/komari-agent/ws"
	ping "github.com/prometheus-community/pro-bing"
)

func NewTask(task_id, command string) {
	if task_id == "" {
		return
	}
	if strings.TrimSpace(command) == "" {
		uploadTaskResult(task_id, "No command provided", 0, time.Now())
		return
	}
	if flags.DisableWebSsh {
		uploadTaskResult(task_id, "Remote control is disabled.", -1, time.Now())
		return
	}
	log.Printf("Executing task %s with command: %s", task_id, command)
	result, exitCode := runTaskCommand(command)
	uploadTaskResult(task_id, result, exitCode, time.Now())
}

func runTaskCommand(command string) (string, int) {
	cmd, cleanup, err := buildTaskCommand(command)
	if err != nil {
		return err.Error(), -1
	}
	defer cleanup()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()

	result := stdout.String()
	if stderr.Len() > 0 {
		result = appendErrorResult(result, stderr.String())
	}
	result = strings.ReplaceAll(result, "\r\n", "\n")
	exitCode := 0
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		} else {
			result = appendErrorResult(result, err.Error())
			exitCode = -1
		}
	}

	return result, exitCode
}

func buildTaskCommand(command string) (*exec.Cmd, func(), error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		scriptFile, err := os.CreateTemp("", "komari-task-*.ps1")
		if err != nil {
			return nil, func() {}, err
		}
		cleanup := func() {
			_ = os.Remove(scriptFile.Name())
		}
		if _, err := scriptFile.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
			_ = scriptFile.Close()
			cleanup()
			return nil, func() {}, err
		}
		script := "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8\n" + command
		if _, err := scriptFile.WriteString(script); err != nil {
			_ = scriptFile.Close()
			cleanup()
			return nil, func() {}, err
		}
		if err := scriptFile.Close(); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		cmd = exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptFile.Name())
		return cmd, cleanup, nil
	} else {
		cmd = exec.Command("sh", "-s")
		cmd.Stdin = strings.NewReader(command)
	}
	return cmd, func() {}, nil
}

func appendErrorResult(result, err string) string {
	if result == "" {
		return err
	}
	return result + "\n" + err
}

func uploadTaskResult(taskID, result string, exitCode int, finishedAt time.Time) {
	payload := v2.Request{
		JSONRPC: v2.Version,
		Method:  v2.MethodAgentTaskResult,
		Params: v2.TaskResultParams{
			TaskID:     taskID,
			Result:     result,
			ExitCode:   exitCode,
			FinishedAt: finishedAt,
		},
	}
	const maxRetries = 3
	var err error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		err = postV2RPC(payload)
		if err == nil {
			return
		}
		log.Printf("Failed to upload task result for task %s (attempt %d/%d): %v", taskID, attempt, maxRetries, err)
		if attempt < maxRetries {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}
	log.Printf("Permanently failed to upload task result for task %s after %d attempts: %v", taskID, maxRetries, err)
}

// resolveIP 解析域名到 IP 地址，排除 DNS 查询时间
func resolveIP(target string) (string, error) {
	// 如果已经是 IP 地址，直接返回
	if ip := net.ParseIP(target); ip != nil {
		return target, nil
	}
	// 解析域名到 IP
	addrs, err := net.LookupHost(target)
	if err != nil || len(addrs) == 0 {
		return "", errors.New("failed to resolve target")
	}
	return addrs[0], nil // 返回第一个解析的 IP
}

func icmpPing(target string, timeout time.Duration) (int64, error) {
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		host = target
	}
	// For ICMP, we only need the host/IP, port is irrelevant.
	// If the host is an IPv6 literal, it might be wrapped in brackets.
	host = strings.Trim(host, "[]")

	// 先解析 IP 地址
	ip, err := resolveIP(host)
	if err != nil {
		return -1, err
	}

	// macOS 例外：pro-bing 非特权 UDP ICMP 在 darwin 上同 Windows 一样静默失效
	//（收不到 echo reply → 全天 "no packets received"）。macOS 无 Linux 的 ping_group_range，
	// 普通用户无法开 raw/非特权 ICMP socket，唯一可靠路径是系统 /sbin/ping（setuid root，用户态可用）。
	// 2026-08-17 实测确诊：agent 全天 no packets received 而原生 ping 0% loss，是 ICMP 通道而非网络问题。
	if runtime.GOOS == "darwin" {
		return systemPing(ip, timeout)
	}

	pinger, err := ping.NewPinger(ip)
	if err != nil {
		return -1, err
	}
	pinger.Count = 1
	pinger.Timeout = timeout
	// 非特权 UDP ICMP：agent 常以非 root 运行（Azure/Beszel 型原生进程、Pterodactyl 容器），
	// raw socket 需要 CAP_NET_RAW 会全丢包；udp4 无特权零配置，且不依赖系统 ping_group_range。
	// 各节点实测（2026-08-13）：1.1.1.1 / 223.5.5.5 / vps.mangoqwq.com 三目标全通（Linux/macOS）。
	//
	// Windows 例外：pro-bing 的 unprivileged UDP-based ping 在 Windows 上静默失效
	//（接收不到 ICMP echo reply → 永远 loss 100%），必须改用 privileged 原始 ICMP socket。
	// Windows 上 agent 以 schtasks /rl highest（管理员）后台任务运行，有权限走 ip4:icmp。
	// 2026-08-16 实测：仅 Linux/macOS 用 UDP，Windows 用 privileged。
	windowsPrivilegedICMP := runtime.GOOS == "windows"
	if windowsPrivilegedICMP {
		pinger.SetPrivileged(true)
	} else {
		pinger.SetPrivileged(false)
	}
	err = pinger.Run()
	if err != nil {
		// Windows 上 privileged raw ICMP 需要管理员权限（schtasks /rl highest）。
		// 若任务退化到普通用户运行，ListenPacket("ip4:icmp") 会直接失败——给出明确诊断，
		// 然后降级试一次非特权 UDP（Windows 非特权通常也收不到回包，但至少区分权限 vs 网络故障）。
		if windowsPrivilegedICMP {
			log.Printf("Ping task: privileged ICMP failed on Windows (agent may not be running as admin): %v; retrying unprivileged", err)
			pinger2, p2err := ping.NewPinger(ip)
			if p2err != nil {
				return -1, err
			}
			pinger2.Count = 1
			pinger2.Timeout = timeout
			pinger2.SetPrivileged(false)
			if err2 := pinger2.Run(); err2 == nil {
				stats2 := pinger2.Statistics()
				if stats2.PacketsRecv > 0 {
					return stats2.AvgRtt.Milliseconds(), nil
				}
			}
		}
		return -1, err
	}
	stats := pinger.Statistics()
	if stats.PacketsRecv == 0 {
		return -1, errors.New("no packets received")
	}
	return stats.AvgRtt.Milliseconds(), nil
}

// systemPing 用系统 ping（macOS /sbin/ping，setuid root）做 ICMP 检查。
// pro-bing 在 darwin 上无可靠的非特权 ICMP 路径（见 icmpPing 注释），这里绕过 pro-bing
// 直接以普通用户身份调用系统 ping，解析其统计输出得到丢包与平均延迟。
func systemPing(ip string, timeout time.Duration) (int64, error) {
	// -c 1：单发；-W：单包等待毫秒数（macOS 单位就是 ms）；-n：纯数字、不复查 DNS
	ctx, cancel := context.WithTimeout(context.Background(), timeout+2*time.Second)
	defer cancel()
	timeoutMs := int(timeout.Milliseconds())
	cmd := exec.CommandContext(ctx, "/sbin/ping", "-n", "-c", "1", "-W", strconv.Itoa(timeoutMs), ip)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return -1, ctx.Err()
	}
	_ = err // macOS 下无回包时 ping 以非 0 exit 退出，判定以统计行为准

	output := string(out)
	if pingPacketsReceived(output) == 0 {
		return -1, errors.New("no packets received")
	}
	latency, ok := pingAvgRttMs(output)
	if !ok {
		// 罕见：有回包但统计行非常规（如 <1ms 显示 time<1），按 0 上报不判失败
		return 0, nil
	}
	return latency, nil
}

var pingRecvRe = regexp.MustCompile(`(\d+)\s+packets received`)

// pingPacketsReceived 从系统 ping 统计行提取收到回包数。
// 例："1 packets transmitted, 1 packets received, 0.0% packet loss" / 不可达 "+1 errors"。
func pingPacketsReceived(output string) int {
	m := pingRecvRe.FindStringSubmatch(output)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// pingAvgRttMs 返回平均往返延迟（毫秒）。macOS 统计行示例：
// "round-trip min/avg/max/stddev = 35.339/35.999/36.369/0.468 ms"
// 注意 -c 1 时 stddev 为 nan（单样本无标准差），故不能靠纯数字正则，改用字符串定位取 avg 字段。
func pingAvgRttMs(output string) (int64, bool) {
	marker := "min/avg/max"
	i := strings.Index(output, marker)
	if i < 0 {
		return 0, false
	}
	eq := strings.Index(output[i:], "=")
	if eq < 0 {
		return 0, false
	}
	rest := strings.TrimSpace(output[i+eq+1:])
	idx := strings.IndexAny(rest, " \t\r\n")
	if idx < 0 {
		return 0, false
	}
	parts := strings.Split(strings.TrimSpace(rest[:idx]), "/")
	if len(parts) < 2 {
		return 0, false
	}
	avg, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return 0, false
	}
	unit := strings.TrimSpace(rest[idx:])
	if strings.HasPrefix(unit, "s") {
		avg *= 1000
	}
	return int64(avg), true
}

func tcpPing(target string, timeout time.Duration) (int64, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		// No port, assume port 80
		host = target
		port = "80"
	}

	// If the host is an IPv6 literal, it might be wrapped in brackets.
	host = strings.Trim(host, "[]")

	ip, err := resolveIP(host)
	if err != nil {
		return -1, err
	}

	targetAddr := net.JoinHostPort(ip, port)
	start := time.Now()
	conn, err := net.DialTimeout("tcp", targetAddr, timeout)
	if err != nil {
		return -1, err
	}
	defer conn.Close()
	return time.Since(start).Milliseconds(), nil
}

func httpPing(target string, timeout time.Duration) (int64, error) {
	// Handle raw IPv6 address for URL
	if strings.Contains(target, ":") && !strings.Contains(target, "[") {
		// check if it's a valid IP to avoid wrapping hostnames
		if ip := net.ParseIP(target); ip != nil && ip.To4() == nil {
			target = "[" + target + "]"
		}
	}

	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "http://" + target
	}

	transport := &http.Transport{
		// 拨测也应走系统代理（如 HTTP(S)_PROXY）：沙箱/受限网络下直连会被拦，
		// 而 ProxyFromEnvironment 在无代理环境变量时返回 nil，行为与原来一致。
		Proxy:             http.ProxyFromEnvironment,
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// 在 Dial 之前解析 IP，排除 DNS 时间
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ip, err := resolveIP(host)
			if err != nil {
				return nil, err
			}
			return net.DialTimeout(network, net.JoinHostPort(ip, port), timeout)
		},
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
	start := time.Now()
	resp, err := client.Get(target)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return latency, nil
	}
	return latency, errors.New("http status not ok")
}

func NewPingTask(conn *ws.SafeConn, taskID uint, pingType, pingTarget string) {
	if taskID == 0 {
		log.Printf("Invalid task ID: %d", taskID)
		return
	}
	var err error = nil
	var latency int64
	pingResult := -1
	timeout := 3 * time.Second           // 默认超时时间
	const highLatencyThreshold = 1000    // ms 阈值
	const retryDropThresholdTcping = 800 // ms 重试中延迟降低超过此值则基本认为发生重传
	// 800ms = SYN/SYN-ACK 首次超时重传 1000ms - 防误判容许 200ms 延迟抖动

	measure := func() (int64, error) {
		switch pingType {
		case "icmp":
			return icmpPing(pingTarget, timeout)
		case "tcp":
			return tcpPing(pingTarget, timeout)
		case "http":
			return httpPing(pingTarget, timeout)
		default:
			return -1, errors.New("unsupported ping type")
		}
	}
	PingHighLatencyRetries := 3
	// 首次测量
	if latency, err = measure(); err == nil {
		firstLatency := latency
		if latency > int64(highLatencyThreshold) && PingHighLatencyRetries > 0 {
			attempts := PingHighLatencyRetries
			for i := 0; i < attempts; i++ {
				if second, err2 := measure(); err2 == nil {
					if second <= int64(highLatencyThreshold) {
						if pingType == "tcp" && firstLatency-second > int64(retryDropThresholdTcping) {
							err = errors.New("suspicious retransmission detected in tcp handshake")
							break
						}
						latency = second
						break
					}
					if i == attempts-1 { // 最后一次仍高
						err = errors.New("latency remains high after retries")
					}
				} else {
					err = err2
					break
				}
			}
		}
	}

	if err != nil {
		log.Printf("Ping task %d failed: %v", taskID, err)
		pingResult = -1 // 如果有错误，设置结果为 -1
	} else {
		pingResult = int(latency)
	}
	finishedAt := time.Now()
	wsPayload := v2.BuildPingResultPayload(taskID, pingType, pingResult, finishedAt)
	// https://github.com/komari-monitor/komari/commit/eb87a4fc330b7d1c407fa4ff70177615a4f50a1f
	// -1 代表丢包，服务端计算
	//if pingResult == -1 {
	//	return
	//}
	if conn == nil {
		if err := postV2RPC(wsPayload); err != nil {
			log.Printf("Failed to upload ping result over POST: %v", err)
		}
		return
	}
	if err := conn.WriteJSON(wsPayload); err != nil {
		log.Printf("Failed to write JSON to WebSocket: %v", err)
	}

}

func postV2RPC(payload interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := strings.TrimSuffix(flags.Endpoint, "/") + "/api/clients/v2/rpc?token=" + flags.Token
	compressed := false
	if !flags.DisableCompression {
		if gz, err := gzipBytes(body); err == nil {
			body = gz
			compressed = true
		}
	}
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if compressed {
		req.Header.Set("Content-Encoding", "gzip")
	}
	client := dnsresolver.GetHTTPClientWithPreference(30*time.Second, flags.PreferIPVersion)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &httpStatusError{StatusCode: resp.StatusCode, Status: resp.Status, Body: string(respBody)}
	}
	if len(bytes.TrimSpace(respBody)) > 0 {
		if _, err := parseV2Response(respBody); err != nil {
			return err
		}
	}
	return nil
}

func gzipBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
