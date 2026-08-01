//go:build unix

package catalog

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureHelperProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killHelperProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	groupErr := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	processErr := command.Process.Kill()
	switch {
	case groupErr == nil:
		if processErr != nil && !errors.Is(processErr, os.ErrProcessDone) {
			return processErr
		}
		return nil
	case errors.Is(groupErr, syscall.ESRCH):
		return processErr
	case processErr == nil || errors.Is(processErr, os.ErrProcessDone):
		return groupErr
	default:
		return errors.Join(groupErr, processErr)
	}
}
