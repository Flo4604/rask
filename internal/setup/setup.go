package setup

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/chronark/rask/internal/assert"
	"github.com/chronark/rask/internal/cli"
	"github.com/chronark/rask/internal/frontend"
	"github.com/chronark/rask/internal/gotool"
)

const (
	// EventChannelCapacity bounds command-to-frontend render buffering.
	EventChannelCapacity = 256
)

// Environment groups process-bound dependencies for one command run.
type Environment struct {
	// Tool runs Go commands for command actions.
	Tool gotool.Tool

	// Stdout receives frontend-owned command output.
	Stdout io.Writer

	// Stderr receives frontend errors and startup warnings.
	Stderr io.Writer

	// Frontend renders command output.
	Frontend *frontend.Frontend

	// Events receives test-run render events.
	Events chan frontend.Event

	// ExitCode is the code selected by a command action.
	ExitCode int
}

type contextKey struct{}

// BuildEnvironment creates the dependencies used by the binary.
var BuildEnvironment = NewEnvironment

// Execute runs root with process-shaped arguments and returns the desired exit code.
func Execute(root *cli.Command, arguments []string) int {
	environment := BuildEnvironment()
	assert.NotNil(environment.Tool)
	assert.NotNil(environment.Stdout)
	assert.NotNil(environment.Stderr)
	assert.NotNil(environment.Frontend)
	cli.SetOutput(root, environment.Stdout, environment.Stderr)
	ctx, stop := signalRootContext()
	defer stop()
	ctx = context.WithValue(ctx, contextKey{}, environment)
	if err := root.Run(ctx, arguments); err != nil {
		environment.Frontend.RenderError(err.Error())
		if environment.ExitCode != 0 {
			return environment.ExitCode
		}
		return 1
	}
	return environment.ExitCode
}

// FromContext returns the command environment stored in ctx.
func FromContext(ctx context.Context) *Environment {
	environment, ok := ctx.Value(contextKey{}).(*Environment)
	if !ok || environment == nil {
		panic("missing command environment")
	}
	return environment
}

// NewEnvironment creates the real command dependencies used by the binary.
func NewEnvironment() *Environment {
	events := make(chan frontend.Event, EventChannelCapacity)
	stdout := os.Stdout
	stderr := os.Stderr
	ui := frontend.New(stdout, stderr, detectTerminal(stdout) && os.Getenv("NO_COLOR") == "", events)
	return &Environment{
		Tool: gotool.SystemTool{}, Stdout: stdout, Stderr: stderr,
		Frontend: ui, Events: events,
	}
}

func signalRootContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-signals:
			cancel()
		case <-ctx.Done():
			return
		}
		select {
		case <-signals:
			os.Exit(130)
		case <-ctx.Done():
		}
	}()
	stop := func() {
		signal.Stop(signals)
		cancel()
	}
	return ctx, stop
}

type statWriter interface {
	Stat() (os.FileInfo, error)
}

func detectTerminal(out io.Writer) bool {
	file, ok := out.(statWriter)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
