//go:build unix

package server

import (
	"os/exec"
	"syscall"
	"time"
)

func configureTaskCommand(cmd *exec.Cmd) {
	// sh -s 会再拉起 systemctl/sleep 等子进程。只 Kill sh 时孙子进程
	// 仍占着 stdout，cmd.Wait 会一直等到它们自己退出。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
}
