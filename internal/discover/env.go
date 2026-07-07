package discover

import (
	"strconv"
	"strings"
)

// GoWorkPath centralizes GOWORK semantics so root discovery stays deterministic.
func GoWorkPath(environment []byte) string {
	value := goEnvValue(environment, "GOWORK")
	if value == "off" {
		return ""
	}
	return value
}

// goEnvValue extracts one KEY=value line while preserving absent as empty.
func goEnvValue(environment []byte, key string) string {
	prefix := key + "="
	for _, raw := range strings.Split(string(environment), "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		return unquoteGoEnvValue(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
	}
	return ""
}

// unquoteGoEnvValue accepts the shell-style quoting emitted by go env.
func unquoteGoEnvValue(value string) string {
	if value == "" {
		return ""
	}
	unquoted, err := strconv.Unquote(value)
	if err == nil {
		return unquoted
	}
	return strings.Trim(value, `'"`)
}
