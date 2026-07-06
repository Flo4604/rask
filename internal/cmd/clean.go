package cmd

import (
	"fmt"

	"github.com/chronark/rask/internal/cache"
	"github.com/chronark/rask/internal/frontend"
	"github.com/chronark/rask/internal/gotool"
)

// commandClean removes this module's cache shard or the whole cache store.
func commandClean(
	tool gotool.Tool,
	arguments []string,
	ui *frontend.Frontend,
) (int, error) {
	if len(arguments) > 0 {
		if arguments[0] == "--all" && len(arguments) == 1 {
			return 0, cache.CleanAll()
		}
		return 1, fmt.Errorf("unknown clean argument %q (did you mean --all?)", arguments[0])
	}
	module, err := tool.CurrentModule()
	if err != nil {
		return 1, err
	}
	resultCache, err := cache.OpenResultCache(module.Path)
	if err != nil {
		return 1, err
	}
	if err := resultCache.RemoveModule(); err != nil {
		return 1, err
	}
	ui.RenderRemoved(module.Path)
	return 0, nil
}
