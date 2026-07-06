//go:build unix

package config

import (
	"fmt"
	"io/fs"
)

// configPermissionWarning exists only on Unix where group/world file bits expose secrets.
func configPermissionWarning(path string, mode fs.FileMode) string {
	if mode.Perm()&0o077 == 0 {
		return ""
	}
	return fmt.Sprintf("warning: %s is group- or world-readable; run chmod 600 %s", path, path)
}
