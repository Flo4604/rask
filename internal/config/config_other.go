//go:build !unix

package config

import "io/fs"

// configPermissionWarning is a no-op where Unix permission bits are not the secret boundary.
func configPermissionWarning(string, fs.FileMode) string {
	return ""
}
