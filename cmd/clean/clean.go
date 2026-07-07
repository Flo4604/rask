package clean

import (
	"context"
	"fmt"

	"github.com/chronark/rask/internal/cache"
	"github.com/chronark/rask/internal/cli"
	appconfig "github.com/chronark/rask/internal/config"
	"github.com/chronark/rask/internal/setup"
)

const (
	// remoteHint tells users how to make clean affect the shared tier.
	remoteHint = "hint: pass --remote to also purge the remote cache"
)

var (
	// cleanAllCache is swappable so command tests can prove flag routing without deleting fixtures.
	cleanAllCache = cache.CleanAll

	// openCache is swappable because clean owns command flow, not cache construction tests.
	openCache = func(cfg cache.Config) (moduleCache, error) { return cache.Open(cfg) }

	// purgeRemote is swappable so clean --all tests do not need a real S3 endpoint.
	purgeRemote = cache.PurgeRemoteAll
)

// moduleCache is the narrow cache behavior needed by the clean command.
type moduleCache interface {
	RemoveModule() error
	HasRemoteTier() bool
	PurgeRemoteModule(ctx context.Context) (int, error)
}

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
		Flags: []cli.Flag{
			cli.Bool("all", "remove every rask cache shard"),
			cli.Bool("remote", "also purge remote cache objects"),
		},
		Action: Action,
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
	remote := command.Bool("remote")
	if remote && cfg.Remote == nil {
		return 1, fmt.Errorf("no remote cache configured")
	}
	if command.Bool("all") {
		if err := cleanAllCache(cfg.CacheDir); err != nil {
			return 1, err
		}
		if remote {
			count, err := purgeRemote(ctx, cfg.Remote)
			if err != nil {
				return 1, err
			}
			environment.Frontend.RenderRemoteRemoved(count)
		} else if cfg.Remote != nil {
			fmt.Fprintln(environment.Stderr, remoteHint)
		}
		return 0, nil
	}
	module, err := environment.Tool.CurrentModule(ctx)
	if err != nil {
		return 1, err
	}
	resultCache, err := openCache(cache.Config{
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
	environment.Frontend.RenderRemoved(module.Path)
	if remote {
		count, err := resultCache.PurgeRemoteModule(ctx)
		if err != nil {
			return 1, err
		}
		environment.Frontend.RenderRemoteRemoved(count)
	} else if resultCache.HasRemoteTier() {
		fmt.Fprintln(environment.Stderr, remoteHint)
	}
	return 0, nil
}
