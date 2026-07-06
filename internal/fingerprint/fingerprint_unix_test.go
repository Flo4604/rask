//go:build unix

package fingerprint

import (
	"crypto/sha256"
	"strings"
	"syscall"
	"testing"
)

// TestHashFileIntoRejectsFIFO protects hashing from blocking on special files.
func TestHashFileIntoRejectsFIFO(t *testing.T) {
	path := t.TempDir() + "/pipe.go"
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	err := hashFileInto(sha256.New(), path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want FIFO path error", err)
	}
}
