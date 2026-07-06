package cmd

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/chronark/rask/internal/assert"
	"github.com/chronark/rask/internal/frontend"
	"github.com/chronark/rask/internal/gotool"
)

// commandEnvironment groups process-bound dependencies for one Execute call.
type commandEnvironment struct {
	tool   gotool.Tool
	stdout io.Writer
	stderr io.Writer
}

// buildCommandEnvironment returns process dependencies and is replaceable by hermetic tests.
var buildCommandEnvironment = newCommandEnvironment

// Execute runs rask with process-shaped arguments and returns the desired exit code.
func Execute(arguments []string) int {
	environment := buildCommandEnvironment()
	assert.NotNil(environment.tool)
	assert.NotNil(environment.stdout)
	assert.NotNil(environment.stderr)
	events := make(chan frontend.Event, eventChannelCapacity)
	ui := frontend.New(
		environment.stdout,
		environment.stderr,
		detectTerminal(environment.stdout) && os.Getenv("NO_COLOR") == "",
		events,
	)
	ctx, stop := signalRootContext()
	defer stop()
	code, err := dispatch(ctx, environment.tool, commandArguments(arguments), ui, events)
	if err != nil {
		ui.RenderError(err.Error())
		return 1
	}
	return code
}

// signalRootContext turns the first interrupt into graceful cancellation.
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

// newCommandEnvironment creates the real command dependencies used by the binary.
func newCommandEnvironment() commandEnvironment {
	return commandEnvironment{
		tool:   gotool.SystemTool{},
		stdout: os.Stdout,
		stderr: os.Stderr,
	}
}

// commandArguments drops argv[0] because dispatch receives only user arguments.
func commandArguments(arguments []string) []string {
	if len(arguments) == 0 {
		return nil
	}
	return arguments[1:]
}

// dispatch routes CLI arguments to test, clean, or help commands.
func dispatch(
	ctx context.Context,
	tool gotool.Tool,
	arguments []string,
	ui *frontend.Frontend,
	events chan<- frontend.Event,
) (int, error) {
	assert.NotNil(tool)
	if len(arguments) == 0 {
		return commandTest(ctx, tool, nil, ui, events)
	}
	switch arguments[0] {
	case "test", "t":
		return commandTest(ctx, tool, arguments[1:], ui, events)
	case "clean":
		return commandClean(tool, arguments[1:], ui)
	case "help", "-h", "--help":
		ui.RenderUsage()
		return 0, nil
	default:
		return commandTest(ctx, tool, arguments, ui, events)
	}
}

// statWriter is the part of os.File used to detect terminals.
type statWriter interface {
	Stat() (os.FileInfo, error)
}

// detectTerminal reports whether out should use terminal features.
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
