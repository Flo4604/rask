package clean

import (
	"context"
	"fmt"

	"github.com/chronark/rask/internal/cache"
	"github.com/chronark/rask/internal/cli"
	appconfig "github.com/chronark/rask/internal/config"
	"github.com/chronark/rask/internal/setup"
)

// Action removes this module's cache shard or the whole cache store.
func Action(ctx context.Context, command *cli.Command) error {
	environment := setup.FromContext(ctx)
	code, err := run(ctx, environment, command)
	environment.ExitCode = code
	return err
}

// Command returns the clean command definition.
func Command() *cli.Command {
	return &cli.Command{
		Name:        "clean",
		Usage:       "Remove cached test results",
		AcceptsArgs: true,
		Flags:       []cli.Flag{cli.Bool("all", "remove every rask cache shard")},
		Action:      Action,
	}
}

func run(ctx context.Context, environment *setup.Environment, command *cli.Command) (int, error) {
	if len(command.Args()) > 0 {
		return 1, fmt.Errorf("unknown clean argument %q (did you mean --all?)", command.Args()[0])
	}
	cfg, err := appconfig.Load(environment.Stderr)
	if err != nil {
		return 1, err
	}
	if command.Bool("all") {
		err := cache.CleanAll(cfg.CacheDir)
		if err == nil && cfg.Remote != nil {
			fmt.Fprintln(environment.Stderr, "remote cache retention is owned by bucket lifecycle rules")
		}
		return 0, err
	}
	module, err := environment.Tool.CurrentModule(ctx)
	if err != nil {
		return 1, err
	}
	resultCache, err := cache.Open(cache.Config{
		ModulePath: module.Path,
		DiskDir:    cfg.CacheDir,
		Remote:     cfg.Remote,
	})
	if err != nil {
		return 1, err
	}
	if err := resultCache.RemoveModule(); err != nil {
		return 1, err
	}
	if resultCache.HasRemoteTier() {
		fmt.Fprintln(environment.Stderr, "remote cache retention is owned by bucket lifecycle rules")
	}
	environment.Frontend.RenderRemoved(module.Path)
	return 0, nil
}
