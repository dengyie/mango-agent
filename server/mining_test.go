package server

import (
	"strings"
	"testing"
)

func TestRenderMiningControlCommand(t *testing.T) {
	defer func(orig string) { flags.MinerControlCmd = orig }(flags.MinerControlCmd)
	defer func(orig bool) { flags.DisableWebSsh = orig }(flags.DisableWebSsh)
	flags.DisableWebSsh = false

	// 非法 action 直接拒绝
	if cmd, code := renderMiningControlCommand("restart"); code == 0 || !strings.Contains(cmd, "restart") {
		t.Fatalf("invalid action should be rejected, got (%q, %d)", cmd, code)
	}

	// 未配置模板时给出可排查的说明
	flags.MinerControlCmd = ""
	if _, code := renderMiningControlCommand("stop"); code != -1 {
		t.Fatalf("empty template should fail, got code %d", code)
	}

	// {action} 占位符替换
	flags.MinerControlCmd = " systemctl {action} srb-xel "
	if cmd, code := renderMiningControlCommand("start"); code != 0 || cmd != "systemctl start srb-xel" {
		t.Fatalf("template render failed, got (%q, %d)", cmd, code)
	}

	// 远程控制被禁用时拒绝
	flags.DisableWebSsh = true
	if cmd, code := renderMiningControlCommand("stop"); code == 0 || !strings.Contains(cmd, "disabled") {
		t.Fatalf("DisableWebSsh should block mining control, got (%q, %d)", cmd, code)
	}
}
