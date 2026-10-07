//go:build windows

package server

import (
	"os/exec"
	"time"
)

func configureTaskCommand(cmd *exec.Cmd) {
	cmd.WaitDelay = 2 * time.Second
}
