//go:build !unix

package catalog

import (
	"os"
	"os/exec"
)

func configureHelperProcessGroup(command *exec.Cmd) {
}

func killHelperProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	return command.Process.Kill()
}
