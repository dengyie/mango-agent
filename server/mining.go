package server

import (
	"log"
	"strings"
	"time"
)

// miningControlTimeout 只约束 start/stop 模板。systemctl/nssm 卡住时必须回传 -1，
// 否则 Hub 占位行 exit_code 一直是 null，面板会空等到轮询窗结束。
// 通用 admin:exec 不走这个上限。
var miningControlTimeout = 60 * time.Second

// renderMiningControlCommand 校验 action 并渲染命令模板。
// 返回 (命令或错误说明, 回传 exitCode)；exitCode != 0 表示本次管控未执行。
func renderMiningControlCommand(action string) (string, int) {
	if action != "start" && action != "stop" {
		return "Invalid mining control action: " + action, -1
	}
	if flags.DisableWebSsh {
		return "Remote control is disabled.", -1
	}
	tpl := strings.TrimSpace(flags.MinerControlCmd)
	if tpl == "" || !strings.Contains(tpl, "{action}") {
		return "Mining control is not configured on this agent (set AGENT_MINER_CONTROL_CMD with an {action} placeholder).", -1
	}
	return strings.ReplaceAll(tpl, "{action}", action), 0
}

// MinerConfigured reports whether this process was started with a miner API URL.
// It describes configuration, not whether the miner is running right now.
func MinerConfigured() bool {
	return strings.TrimSpace(flags.MinerAPIUrl) != ""
}

// MinerControllable reports whether a hub start/stop can be executed.
// The template must contain a literal {action}; without it the command would be
// handed to the shell unchanged.
func MinerControllable() bool {
	tpl := strings.TrimSpace(flags.MinerControlCmd)
	return tpl != "" && strings.Contains(tpl, "{action}") && !flags.DisableWebSsh
}

// NewMiningControlTask 处理 hub 下发的挖矿管控（agent.mining.control）。
// action 替换进 AGENT_MINER_CONTROL_CMD 模板的 {action} 占位符后，
// 与远程任务（NewTask）同一条执行/回传链路跑：Windows 走 PowerShell，其它平台走 sh。
// 结果经 /api/clients/v2/rpc method=agent.taskResult 回传，hub 侧用同一个 task_id 查询。
func NewMiningControlTask(taskID, action string) {
	if taskID == "" {
		return
	}
	command, exitCode := renderMiningControlCommand(action)
	if exitCode != 0 {
		uploadTaskResult(taskID, command, exitCode, time.Now())
		return
	}
	log.Printf("Mining control %s (task %s): %s", action, taskID, command)
	result, exitCode := runTaskCommandWithTimeout(command, miningControlTimeout)
	uploadTaskResult(taskID, result, exitCode, time.Now())
}
