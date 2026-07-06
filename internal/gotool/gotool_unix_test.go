//go:build unix

package gotool

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestStartTestProcessCancelKillsProcessGroup protects process-tree cleanup.
//
// The shell leaves a backgrounded grandchild running when only the direct child
// is killed. rask must kill the whole group because lingering grandchildren can
// keep Go build-cache locks held and stall later suites without output.
func TestStartTestProcessCancelKillsProcessGroup(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("missing /bin/sh: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	process, err := startTestProcess(ctx, t.TempDir(), "/bin/sh", "-c", `sleep 60 & echo "$$ $!"; wait`)
	if err != nil {
		t.Fatalf("start shell: %v", err)
	}
	shellPID, grandchildPID := readProcessPIDs(t, process.Events())
	cancel()
	waitForTestProcess(t, process)
	waitForPIDsGone(t, shellPID, grandchildPID)
}

// readProcessPIDs reads the shell pid and its backgrounded child pid.
//
// The pid line proves the process tree has been created before cancellation, so
// the assertion cannot pass by racing cancellation before the grandchild exists.
func readProcessPIDs(t *testing.T, reader io.Reader) (int, int) {
	t.Helper()
	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(reader).ReadString('\n')
		if err != nil {
			errCh <- err
			return
		}
		lineCh <- line
	}()
	select {
	case line := <-lineCh:
		return parsePIDLine(t, line)
	case err := <-errCh:
		t.Fatalf("read pid line: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for pid line")
	}
	return 0, 0
}

// parsePIDLine converts the shell's pid announcement into numeric pids.
//
// The test uses numeric pids because kill(pid, 0) is the portable unix check
// for whether each process survived the group kill.
func parsePIDLine(t *testing.T, line string) (int, int) {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) != 2 {
		t.Fatalf("pid line = %q, want two fields", line)
	}
	parent, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("parse parent pid %q: %v", fields[0], err)
	}
	child, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("parse child pid %q: %v", fields[1], err)
	}
	return parent, child
}

// waitForTestProcess bounds Wait so pipe-writer leaks fail quickly.
//
// A stuck Wait here would mean cancellation did not close the command's process
// tree, which is the same failure mode that leaves later suites silent.
func waitForTestProcess(t *testing.T, process TestProcess) {
	t.Helper()
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- process.Wait()
	}()
	select {
	case <-waitCh:
	case <-time.After(testProcessWaitDelay + time.Second):
		t.Fatalf("timed out waiting for canceled process")
	}
}

// waitForPIDsGone polls until every process killed by the group signal is gone.
//
// Polling avoids coupling the test to scheduler timing while still proving the
// backgrounded grandchild did not survive as an orphan.
func waitForPIDsGone(t *testing.T, pids ...int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		allGone := true
		for _, pid := range pids {
			if processExists(pid) {
				allGone = false
				break
			}
		}
		if allGone {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("processes still exist after group kill: %v", pids)
}

// processExists reports whether pid is still visible to the current user.
//
// ESRCH is the only successful terminal condition for this regression test
// because EPERM still means the process survived the attempted group kill.
func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
