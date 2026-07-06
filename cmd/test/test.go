package testcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/chronark/rask/internal/assert"
	"github.com/chronark/rask/internal/cache"
	"github.com/chronark/rask/internal/cli"
	appconfig "github.com/chronark/rask/internal/config"
	"github.com/chronark/rask/internal/discover"
	"github.com/chronark/rask/internal/fingerprint"
	"github.com/chronark/rask/internal/frontend"
	"github.com/chronark/rask/internal/gotool"
	"github.com/chronark/rask/internal/run"
	"github.com/chronark/rask/internal/setup"
)

const (
	// packageChannelCapacity bounds discovery-to-fingerprinter buffering.
	packageChannelCapacity = 64

	// suiteChannelCapacity bounds fingerprinter-to-orchestration buffering.
	suiteChannelCapacity = 64

	// taskChannelCapacity bounds stale work queued for runner workers.
	taskChannelCapacity = 64

	// resultChannelCapacity keeps worker results rendezvous-style for fail-fast.
	resultChannelCapacity = 0

	// eventChannelCapacity bounds orchestration-to-frontend buffering.
	eventChannelCapacity = setup.EventChannelCapacity
)

const (
	// serviceDiscover identifies the discoverer service error slot.
	serviceDiscover = iota

	// serviceFingerprint identifies the fingerprinter service error slot.
	serviceFingerprint

	// serviceRunner identifies the runner service error slot.
	serviceRunner

	// serviceCache identifies the cache lifecycle service error slot.
	serviceCache

	// serviceFrontend identifies the frontend service error slot.
	serviceFrontend

	// serviceCount is the number of lifecycle services started by testCommand.
	serviceCount
)

// testCommand owns the state for one `rask test` run.
type testCommand struct {
	ctx            context.Context
	tool           gotool.Tool
	module         gotool.Module
	patternDir     string
	resultCache    suiteCache
	patterns       []string
	noCache        bool
	frontend       *frontend.Frontend
	cancelRun      context.CancelFunc
	cancelCache    context.CancelFunc
	cancelFrontend context.CancelFunc
	runCtx         context.Context
	cacheCtx       context.Context
	frontendCtx    context.Context
	packages       chan gotool.Package
	suites         chan fingerprint.Suite
	tasks          chan run.SuiteTask
	results        chan run.Event
	events         chan<- frontend.Event
	wg             sync.WaitGroup
	errors         []error
	frontendDone   chan struct{}
	frontendCode   int
	startedAt      time.Time
	summary        frontend.Summary
	interrupted    bool
	tasksClosed    bool
	resultsOpen    bool
	failFast       bool
	staleSent      int
	staleResults   int
	staleDropped   int
}

// testArguments holds parsed package patterns for the test command.
type testArguments struct {
	// patterns are package patterns passed to discovery.
	patterns []string

	// verbose reports whether passed and ignored per-test lines should render.
	verbose bool

	// noCache reports whether cache reads should be bypassed for this run.
	noCache bool
}

// suiteCache is the cache behavior the test command needs for suites.
type suiteCache interface {
	Get(digest string) (cache.Entry, bool)
	Put(digest string, entry cache.Entry) error
	Delete(digest string)
	Run(ctx context.Context) error
	HasRemoteTier() bool
	RemoteStats() cache.RemoteStats
}

// Action parses package patterns and starts the service graph.
func Action(ctx context.Context, command *cli.Command) error {
	environment := setup.FromContext(ctx)
	parsed := testArgumentsFromCommand(command)
	cfg, err := appconfig.Load(environment.Stderr)
	if err != nil {
		environment.ExitCode = 1
		return err
	}
	code, err := runTest(ctx, environment.Tool, cfg, parsed, environment.Frontend, environment.Events)
	environment.ExitCode = code
	return err
}

// Command returns the test command definition.
func Command() *cli.Command {
	return &cli.Command{
		Name:        "test",
		Usage:       "Run Go tests with cached suite results",
		AcceptsArgs: true,
		Aliases:     []string{"t"},
		Flags:       Flags(),
		Action:      Action,
	}
}

// Flags returns the root test flags.
func Flags() []cli.Flag {
	return []cli.Flag{
		cli.Bool("v", "render passing and ignored test lines"),
		cli.Bool("verbose", "render passing and ignored test lines"),
		cli.Bool("no-cache", "bypass cache reads and refresh passing suites"),
	}
}

func runTest(
	ctx context.Context,
	tool gotool.Tool,
	cfg appconfig.Config,
	parsed testArguments,
	ui *frontend.Frontend,
	events chan<- frontend.Event,
) (int, error) {
	userDir, err := os.Getwd()
	if err != nil {
		return 1, fmt.Errorf("resolving working directory: %w", err)
	}
	module, err := tool.CurrentModule()
	if err != nil {
		return 1, err
	}
	tool, err = environmentTool(tool, module)
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
	command := newTestCommand(ctx, tool, module, userDir, resultCache, parsed, ui, events)
	return command.run()
}

func testArgumentsFromCommand(command *cli.Command) testArguments {
	parsed := testArguments{
		patterns: append([]string(nil), command.Args()...),
		verbose:  command.Bool("v") || command.Bool("verbose"),
		noCache:  command.Bool("no-cache"),
	}
	if len(parsed.patterns) == 0 {
		parsed.patterns = []string{"./..."}
	}
	return parsed
}

// environmentTool reuses one full go-env read for discovery and fingerprint salting.
func environmentTool(tool gotool.Tool, module gotool.Module) (gotool.Tool, error) {
	environment, err := tool.Environment(module.Dir)
	if err != nil {
		return nil, err
	}
	return envTool{Tool: tool, environment: environment}, nil
}

// envTool returns cached environment bytes so one run salts from one go-env output.
type envTool struct {
	gotool.Tool
	environment []byte
}

// Environment returns the cached full go-env output for fingerprint salting.
func (t envTool) Environment(string) ([]byte, error) {
	return append([]byte(nil), t.environment...), nil
}

// newTestCommand wires channels and rendering for one test command.
func newTestCommand(
	ctx context.Context,
	tool gotool.Tool,
	module gotool.Module,
	userDir string,
	resultCache suiteCache,
	parsed testArguments,
	ui *frontend.Frontend,
	events chan<- frontend.Event,
) *testCommand {
	assert.NotNil(tool)
	assert.NotNil(resultCache)
	assert.NotNil(ui)
	ui.ConfigureRun(parsed.verbose, module.Path)
	return &testCommand{
		ctx: ctx, tool: tool, module: module, resultCache: resultCache, patterns: parsed.patterns,
		noCache:    parsed.noCache,
		frontend:   ui,
		patternDir: userDir,
		packages:   make(chan gotool.Package, packageChannelCapacity),
		suites:     make(chan fingerprint.Suite, suiteChannelCapacity),
		tasks:      make(chan run.SuiteTask, taskChannelCapacity),
		results:    make(chan run.Event, resultChannelCapacity),
		events:     events,
		startedAt:  time.Now(), errors: make([]error, serviceCount),
		frontendDone: make(chan struct{}),
	}
}

// run executes setup, service startup, orchestration, and final verdict.
func (c *testCommand) run() (int, error) {
	c.setup()
	defer c.cancelRun()
	defer c.cancelCache()
	defer c.cancelFrontend()
	c.startServices()
	c.orchestrate()
	c.cancelCache()
	// Service error slots are owned by their goroutines until the WaitGroup
	// completes; read them only after this point.
	c.wg.Wait()
	if c.errors[serviceCache] != nil {
		c.summary.CacheErrors++
	}
	if err := serviceFailure(c.errors, c.failFast || c.interrupted); err != nil {
		close(c.events)
		<-c.frontendDone
		return 1, err
	}
	c.finishSummary()
	close(c.events)
	<-c.frontendDone
	return c.finish()
}

// setup creates the service lifetimes for this command.
func (c *testCommand) setup() {
	runCtx, cancelRun := context.WithCancel(c.ctx)
	cacheCtx, cancelCache := context.WithCancel(c.ctx)
	frontendCtx, cancelFrontend := context.WithCancel(context.Background())
	c.runCtx, c.cancelRun = runCtx, cancelRun
	c.cacheCtx, c.cancelCache = cacheCtx, cancelCache
	c.frontendCtx, c.cancelFrontend = frontendCtx, cancelFrontend
}

// startServices starts each lifecycle service.
func (c *testCommand) startServices() {
	discoverer := discover.New(c.tool, c.module, c.patternDir, c.patterns, c.packages)
	fingerprinter := fingerprint.New(c.tool, c.module, c.packages, c.suites)
	runner := run.New(c.tool, c.module.Dir, c.tasks, c.results)
	c.startService(serviceDiscover, func() error { return discoverer.Run(c.runCtx) })
	c.startService(serviceFingerprint, func() error { return fingerprinter.Run(c.runCtx) })
	c.startService(serviceRunner, func() error { return runner.Run(c.runCtx) })
	c.startService(serviceCache, func() error { return c.resultCache.Run(c.cacheCtx) })
	c.startFrontend()
}

// startFrontend starts rendering outside the producer-service WaitGroup so the
// summary event sent after wg.Wait still has a consumer.
func (c *testCommand) startFrontend() {
	go func() {
		defer close(c.frontendDone)
		code, err := c.frontend.Run(c.frontendCtx)
		c.frontendCode = code
		c.errors[serviceFrontend] = err
	}()
}

// startService records one service error in its owned index after Run returns.
func (c *testCommand) startService(index int, run func() error) {
	assert.GreaterOrEqual(index, 0)
	assert.Less(index, serviceCount)
	c.wg.Go(func() {
		c.errors[index] = run()
	})
}

// orchestrate plans all suites before results stream, then dispatches work.
func (c *testCommand) orchestrate() {
	c.resultsOpen = true
	suites := c.collectSuites()
	c.sendRunPlan(suites)
	c.dispatchPlannedSuites(suites)
}

// collectSuites drains fingerprinted suites so permanent row layout is stable.
func (c *testCommand) collectSuites() []fingerprint.Suite {
	suites := make([]fingerprint.Suite, 0)
	done := c.ctx.Done()
	for {
		select {
		case suite, ok := <-c.suites:
			if !ok {
				return suites
			}
			suites = append(suites, suite)
		case <-done:
			c.interrupted = true
			c.cancelRun()
			done = nil
		}
	}
}

// sendRunPlan tells the frontend total suites and widest selected import path.
func (c *testCommand) sendRunPlan(suites []fingerprint.Suite) {
	c.summary.Suites = len(suites)
	width := 0
	for _, suite := range suites {
		path := frontend.ShortenImportPath(suite.Package.ImportPath, c.module.Path)
		width = max(width, len([]rune(path)))
	}
	c.sendFrontend(frontend.Event{
		Kind: frontend.EventRunPlan,
		RunPlan: frontend.RunPlan{
			Suites:             len(suites),
			MaxImportPathWidth: width,
		},
	})
}

// dispatchPlannedSuites partitions planned suites and drains runner results.
func (c *testCommand) dispatchPlannedSuites(suites []fingerprint.Suite) {
	for _, suite := range suites {
		c.processSuite(suite)
	}
	c.closeTasks()
	for c.resultsOpen {
		result, ok := <-c.results
		c.resultsOpen = c.handleRunnerReceive(ok, result)
	}
}

// handleRunnerReceive processes one runner event or records runner completion.
func (c *testCommand) handleRunnerReceive(ok bool, event run.Event) bool {
	if !ok {
		return false
	}
	c.handleRunnerEvent(event)
	return true
}

// processSuite partitions one suite through cache and runner policy.
func (c *testCommand) processSuite(suite fingerprint.Suite) {
	if !c.noCache {
		entry, found := c.resultCache.Get(suite.Digest)
		if found {
			c.summary.Cached++
			c.summary.Passed += entry.Passed
			c.summary.Ignored += entry.Ignored
			c.sendCached(suite, entry)
			return
		}
	}
	if c.failFast || c.tasksClosed {
		c.staleDropped++
		return
	}
	if !c.sendTask(run.SuiteTask{Package: suite.Package, Digest: suite.Digest}) {
		c.staleDropped++
		return
	}
	c.staleSent++
}

// handleRunnerEvent forwards runner notifications and applies fail-fast.
func (c *testCommand) handleRunnerEvent(event run.Event) {
	switch event.Kind {
	case run.EventSuiteStarted:
		c.sendFrontend(frontend.Event{
			Kind: frontend.EventStarted, ImportPath: event.Suite.Package.ImportPath,
		})
		return
	case run.EventSuiteProgress:
		c.sendFrontend(frontend.Event{
			Kind: frontend.EventTestProgress,
			TestProgress: frontend.TestProgress{
				ImportPath: event.Progress.Package.ImportPath,
				Completed:  event.Progress.Completed,
				Total:      event.Progress.Total,
				TotalKnown: event.Progress.TotalKnown,
			},
		})
		return
	case run.EventSuiteResult:
	default:
		assert.Unreachable("runner event kind %d", event.Kind)
	}
	result := event.Result
	c.staleResults++
	recordSuiteResult(c.resultCache, result, &c.summary)
	if !result.Killed {
		c.sendFrontend(frontend.Event{Kind: frontend.EventSuiteResult, SuiteResult: frontendSuiteResult(result)})
	}
	if result.Failed && !c.failFast {
		c.failFast = true
		c.cancelRun()
		c.closeTasks()
	}
}

// sendCached forwards one cache-hit suite event to the frontend.
func (c *testCommand) sendCached(suite fingerprint.Suite, entry cache.Entry) {
	event := frontend.Event{Kind: frontend.EventCached, Cached: frontend.CachedEvent{
		ImportPath: suite.Package.ImportPath,
		Passed:     entry.Passed,
		Ignored:    entry.Ignored,
	}}
	c.sendFrontend(event)
}

// sendTask sends one runner task unless run cancellation wins.
//
// The non-blocking send followed by a nested select is load-bearing: workers
// may be blocked sending results while the task channel is full, so waiting
// on the task send alone would deadlock the producer-consumer pair. Draining
// results between send attempts keeps both sides moving.
func (c *testCommand) sendTask(task run.SuiteTask) bool {
	for c.runCtx.Err() == nil && c.resultsOpen {
		select {
		case c.tasks <- task:
			return true
		default:
		}
		select {
		case result, ok := <-c.results:
			c.resultsOpen = c.handleRunnerReceive(ok, result)
		case <-c.runCtx.Done():
			return false
		}
	}
	return false
}

// sendFrontend sends one render event using the frontend lifetime.
func (c *testCommand) sendFrontend(event frontend.Event) bool {
	select {
	case c.events <- event:
		return true
	case <-c.frontendCtx.Done():
		return false
	}
}

// closeTasks closes the task channel exactly once by its single producer.
func (c *testCommand) closeTasks() {
	if c.tasksClosed {
		return
	}
	close(c.tasks)
	c.tasksClosed = true
}

// finishSummary computes dropped-suite counts and sends the final summary.
func (c *testCommand) finishSummary() {
	if c.ctx.Err() != nil {
		c.interrupted = true
	}
	c.summary.Skipped += c.staleDropped + c.staleSent - c.staleResults
	c.summary.Tests = c.summary.Passed + c.summary.Failed + c.summary.Ignored
	c.summary.Elapsed = time.Since(c.startedAt)
	c.summary.Interrupted = c.interrupted
	c.summary.RemoteConfigured = c.resultCache.HasRemoteTier()
	stats := c.resultCache.RemoteStats()
	c.summary.RemoteCached = stats.Reads
	c.summary.RemoteReads = stats.Reads
	c.summary.RemoteWrites = stats.Writes
	sort.Strings(c.summary.FailedSuites)
	c.sendFrontend(frontend.Event{Kind: frontend.EventSummary, Summary: c.summary})
}

// finish returns after all service errors have been written and waited on.
func (c *testCommand) finish() (int, error) {
	if err := serviceFailure(c.errors, c.failFast || c.interrupted); err != nil {
		return 1, err
	}
	if c.frontendCode != 0 {
		return c.frontendCode, nil
	}
	if len(c.summary.FailedSuites) > 0 {
		return 1, nil
	}
	return 0, nil
}

// serviceFailure returns the first service error that should fail the command.
func serviceFailure(errorsByService []error, failFast bool) error {
	for index, err := range errorsByService {
		if err == nil {
			continue
		}
		if index == serviceCache {
			continue
		}
		if failFast && errors.Is(err, context.Canceled) {
			continue
		}
		return err
	}
	return nil
}

// recordSuiteResult updates the summary and cache for one terminal suite result.
// Only passing suites are cached; failing suites evict any stale entry.
func recordSuiteResult(resultCache suiteCache, suite run.SuiteResult, summary *frontend.Summary) {
	assert.NotNil(resultCache)
	if suite.Killed {
		summary.Skipped++
		return
	}
	passed, failed, ignored := countRunnerTests(suite.Tests)
	summary.Passed += passed
	summary.Failed += failed
	summary.Ignored += ignored
	if suite.Failed {
		summary.FailedSuites = append(summary.FailedSuites, suite.Package.ImportPath)
		resultCache.Delete(suite.Digest)
		return
	}
	entry := cache.Entry{
		ImportPath: suite.Package.ImportPath, Passed: passed, Ignored: ignored,
		ElapsedSeconds: suite.ElapsedSeconds, CreatedAt: time.Now(),
	}
	if err := resultCache.Put(suite.Digest, entry); err != nil {
		summary.CacheErrors++
	}
}

// countRunnerTests returns pass, fail, and skip totals for folded runner tests.
func countRunnerTests(tests []run.TestResult) (passed int, failed int, ignored int) {
	for _, test := range tests {
		switch test.Outcome {
		case run.TestPassed:
			passed++
		case run.TestFailed:
			failed++
		case run.TestIgnored:
			ignored++
		default:
			assert.Unreachable("test outcome %q", test.Outcome)
		}
	}
	return passed, failed, ignored
}

// frontendSuiteResult adapts a runner result to the frontend's rendering shape.
func frontendSuiteResult(result run.SuiteResult) frontend.SuiteResult {
	return frontend.SuiteResult{
		ImportPath: result.Package.ImportPath, Tests: frontendTests(result.Tests),
		PackageOutput: result.PackageOutput, ElapsedSeconds: result.ElapsedSeconds,
		Failed: result.Failed, Err: result.Err,
	}
}

// frontendTests adapts runner test results to the frontend's rendering shape.
func frontendTests(tests []run.TestResult) []frontend.TestResult {
	out := make([]frontend.TestResult, 0, len(tests))
	for _, test := range tests {
		out = append(out, frontend.TestResult{
			Name: test.Name, Outcome: frontend.TestOutcome(test.Outcome),
			Output: test.Output, DroppedOutput: test.DroppedOutput,
		})
	}
	return out
}
