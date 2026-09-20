package server

import (
	"log"
	"strings"
	"time"
)

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
	if tpl == "" {
		return "Mining control is not configured on this agent (set AGENT_MINER_CONTROL_CMD with an {action} placeholder).", -1
	}
	return strings.ReplaceAll(tpl, "{action}", action), 0
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
	result, exitCode := runTaskCommand(command)
	uploadTaskResult(taskID, result, exitCode, time.Now())
}
