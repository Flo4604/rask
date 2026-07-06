//go:build unix

package gotool

import (
	"os"
	"os/exec"
	"syscall"
)

// configureTestCommandCancel makes context cancellation kill the process group.
//
// `go test` can leave compiler, linker, or test-binary grandchildren behind if
// only the direct child receives SIGKILL. Those orphaned grandchildren can hold
// Go build-cache locks and make later suites stall without producing output.
func configureTestCommandCancel(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
			return command.Process.Kill()
		}
		return nil
	}
}
