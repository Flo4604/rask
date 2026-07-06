//go:build !unix

package gotool

import "os/exec"

// configureTestCommandCancel leaves default exec cancellation on unsupported targets.
//
// rask targets darwin and linux, where the unix implementation kills process
// groups. This fallback keeps the package buildable elsewhere without claiming
// process-tree cleanup semantics that the platform-specific code has not set up.
func configureTestCommandCancel(command *exec.Cmd) {}
