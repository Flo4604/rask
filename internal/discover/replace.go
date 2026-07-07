package discover

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/chronark/rask/internal/assert"
)

// LocalReplaceRoots avoids x/mod so discovery keeps its zero-dependency contract.
func LocalReplaceRoots(root string) ([]string, error) {
	body, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("enumerating local replaces in %s: %w", root, err)
	}
	targets, err := localReplaceTargets(string(body))
	if err != nil {
		return nil, fmt.Errorf("enumerating local replaces in %s: %w", root, err)
	}
	roots := make([]string, 0, len(targets))
	for _, target := range targets {
		roots = append(roots, resolveReplaceRoot(root, target))
	}
	return roots, nil
}

// localReplaceTargets scans the replace forms rask needs without x/mod.
func localReplaceTargets(body string) ([]string, error) {
	targets := make([]string, 0)
	inBlock := false
	for _, raw := range strings.Split(body, "\n") {
		line := replaceLine(raw)
		switch {
		case line == "":
			continue
		case isReplaceBlockStart(line):
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		}
		target, ok, err := localReplaceTarget(line, inBlock)
		if err != nil {
			return nil, err
		}
		if ok {
			targets = append(targets, target)
		}
	}
	return targets, nil
}

// replaceLine strips line comments before tokenization by design.
func replaceLine(raw string) string {
	line := strings.SplitN(raw, "//", 2)[0]
	return strings.TrimSpace(line)
}

// isReplaceBlockStart recognizes the go.mod replace block opener.
func isReplaceBlockStart(line string) bool {
	return line == "replace (" || line == "replace("
}

// localReplaceTarget returns a local path target from one replace directive.
func localReplaceTarget(line string, inBlock bool) (string, bool, error) {
	if !inBlock {
		if !strings.HasPrefix(line, "replace ") {
			return "", false, nil
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "replace "))
	}
	target, ok, err := replacementTarget(strings.Fields(line))
	if err != nil || !ok {
		return "", false, err
	}
	return target, isFilesystemPath(target), nil
}

// replacementTarget unquotes the token immediately after =>.
func replacementTarget(fields []string) (string, bool, error) {
	for index, field := range fields {
		if field != "=>" || index+1 >= len(fields) {
			continue
		}
		target, err := unquotePath(fields[index+1])
		if err != nil {
			return "", false, err
		}
		return target, true, nil
	}
	return "", false, nil
}

// unquotePath turns go.mod string literals into filesystem path tokens.
func unquotePath(path string) (string, error) {
	if path == "" || (path[0] != '"' && path[0] != '`' && path[0] != '\'') {
		return path, nil
	}
	unquoted, err := strconv.Unquote(path)
	if err != nil {
		return "", fmt.Errorf("parsing quoted replace target %s: %w", path, err)
	}
	return unquoted, nil
}

// isFilesystemPath excludes version-suffixed module replace targets.
func isFilesystemPath(path string) bool {
	return strings.HasPrefix(path, "./") || strings.HasPrefix(path, "../") || filepath.IsAbs(path)
}

// resolveReplaceRoot resolves local replace targets against the owning module.
func resolveReplaceRoot(root string, target string) string {
	assert.True(isFilesystemPath(target), "replace target %s must be a filesystem path", target)
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Clean(filepath.Join(root, target))
}
