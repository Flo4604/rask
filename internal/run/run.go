// Package run executes stale suites and folds `go test -json` streams.
//
// The package owns the worker pool, per-suite process execution, JSON event
// folding, output bounds, and a per-suite no-output watchdog that kills child
// processes stalled before the test binary can enforce `go test -timeout`. It
// consumes [gotool.Tool] but does not know about result caching or terminal
// rendering.
//
// [Runner.Run] closes the result channel when all workers stop. Cancellation
// stops queued work, kills in-flight `go test` processes through the supplied
// context, and may stop ctx-guarded result emits so orchestration can count
// unsent stale work as skipped.
package run

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chronark/rask/internal/assert"
	"github.com/chronark/rask/internal/gotool"
)

const (
	// testCountPerSuiteMax bounds the number of test names tracked for one suite.
	testCountPerSuiteMax = 65536

	// eventCountPerSuiteMax bounds the number of JSON events read for one suite.
	eventCountPerSuiteMax = 1 << 22

	// defaultSuiteStallAfter bounds a child `go test` process that stops producing output.
	//
	// A production hang showed workers blocked in stdout reads for more than
	// five minutes while rask itself stayed healthy. `go test -timeout` only
	// bounds the test binary after it starts, not compile-phase or lock-wait
	// stalls, so the runner supervises every child process directly.
	defaultSuiteStallAfter = 5 * time.Minute

	// outputLinesPerTestKept bounds retained diagnostic output per test and package.
	outputLinesPerTestKept = 4096

	// lineBytesMax bounds one line read from `go test -json` stdout.
	lineBytesMax = 4 << 20

	// workerCountMax caps suite parallelism regardless of GOMAXPROCS.
	workerCountMax = 32
)

// TestOutcome is the terminal state of one test reported by `go test -json`.
type TestOutcome string

const (
	// TestPassed means the test reported a pass event.
	TestPassed TestOutcome = "pass"

	// TestFailed means the test reported a fail event or was still running when the suite failed.
	TestFailed TestOutcome = "fail"

	// TestIgnored means the test reported a skip event.
	TestIgnored TestOutcome = "skip"

	// testRunning marks tests that have started but not reached a terminal event.
	testRunning TestOutcome = "run"
)

// SuiteTask identifies one package test suite that needs execution.
type SuiteTask struct {
	// Package is the Go package to test.
	Package gotool.Package

	// Digest is the content digest associated with the suite.
	Digest string
}

// TestResult records the folded result for one test name.
type TestResult struct {
	// Name is the test name reported by `go test -json`.
	Name string

	// Outcome is the folded terminal state for the test.
	Outcome TestOutcome

	// Output contains retained diagnostic output for the test.
	Output []string

	// DroppedOutput is the number of output lines omitted after the retention bound.
	DroppedOutput int
}

// SuiteResult records the folded result for one package suite.
type SuiteResult struct {
	// Package is the package that was tested.
	Package gotool.Package

	// Digest is the suite content digest used by callers for cache decisions.
	Digest string

	// Tests contains folded test results in start order.
	Tests []TestResult

	// PackageOutput contains retained package-level and build diagnostic output.
	PackageOutput []string

	// ElapsedSeconds is the package elapsed time reported by `go test -json`.
	ElapsedSeconds float64

	// Failed reports whether the suite completed with a failing package result or runner error.
	Failed bool

	// Completed reports whether a package-level pass, fail, or skip event was seen.
	Completed bool

	// Killed reports whether fail-fast cancellation stopped the suite.
	Killed bool

	// Err is the runner or process error text, or empty when no extra error occurred.
	Err string
}

// EventKind identifies the runner event payload carried by Event.
type EventKind int

const (
	// EventSuiteStarted reports a suite entering the runner pool.
	EventSuiteStarted EventKind = iota + 1

	// EventSuiteProgress reports live terminal test counts for a running suite.
	EventSuiteProgress

	// EventSuiteResult reports one terminal folded suite result.
	EventSuiteResult
)

// SuiteProgress carries cosmetic live progress for one running suite.
type SuiteProgress struct {
	// Package is the suite package whose row should update.
	Package gotool.Package

	// Completed counts top-level terminal events so units match the -list total.
	Completed int

	// Total is the lister-reported total when TotalKnown is true.
	Total int

	// TotalKnown reports whether test enumeration succeeded before execution.
	TotalKnown bool
}

// Event is the runner output stream consumed by orchestration.
type Event struct {
	// Kind selects which payload field is meaningful.
	Kind EventKind

	// Suite carries the task metadata for EventSuiteStarted.
	Suite SuiteTask

	// Progress carries live suite counters for EventSuiteProgress.
	Progress SuiteProgress

	// Result carries the folded terminal result for EventSuiteResult.
	Result SuiteResult
}

// Runner consumes stale suite tasks and streams worker results.
//
// Runner is safe to run once. The bounded worker pool shares one task channel,
// so workers compete for tasks instead of duplicating them. Run owns the result
// channel and closes it after every worker exits.
type Runner struct {
	tool      gotool.Tool
	moduleDir string
	in        <-chan SuiteTask
	out       chan<- Event
}

// New returns a runner connected to task and result channels.
func New(
	tool gotool.Tool,
	moduleDir string,
	in <-chan SuiteTask,
	out chan<- Event,
) *Runner {
	return &Runner{tool: tool, moduleDir: moduleDir, in: in, out: out}
}

// Run starts the bounded worker pool, consumes tasks, emits results, and closes them.
//
// Run is the result channel's only sender and closes it with defer before
// returning. Every result send selects on ctx.Done so cancellation can unblock
// orchestration backpressure. Runner never closes the task channel because
// orchestration is its single sender and close owner.
func (r *Runner) Run(ctx context.Context) error {
	defer close(r.out)
	var wg sync.WaitGroup
	for range workerPoolSize() {
		wg.Go(func() { r.runWorker(ctx) })
	}
	wg.Wait()
	return nil
}

// workerPoolSize returns GOMAXPROCS-1 clamped to [1, workerCountMax].
//
// One CPU stays free so orchestration and rendering remain responsive while
// suites run, and the cap bounds process fan-out on very wide machines.
func workerPoolSize() int {
	return min(workerCountMax, max(1, runtime.GOMAXPROCS(0)-1))
}

// runWorker consumes tasks until the task subscription closes or ctx cancellation stops the pool.
func (r *Runner) runWorker(ctx context.Context) {
	for {
		suite, ok := r.receiveTask(ctx)
		if !ok {
			return
		}
		if !r.sendEvent(ctx, Event{Kind: EventSuiteStarted, Suite: suite}) {
			return
		}
		emit := func(progress SuiteProgress) bool {
			return r.sendEvent(ctx, Event{Kind: EventSuiteProgress, Progress: progress})
		}
		result := runSuite(ctx, r.tool, r.moduleDir, suite, defaultSuiteStallAfter, emit)
		if !r.sendEvent(ctx, Event{Kind: EventSuiteResult, Result: result}) {
			return
		}
	}
}

// receiveTask waits for one task or cancellation.
func (r *Runner) receiveTask(ctx context.Context) (SuiteTask, bool) {
	select {
	case suite, ok := <-r.in:
		return suite, ok
	case <-ctx.Done():
		return SuiteTask{}, false
	}
}

// sendEvent sends one runner event unless ctx cancellation wins first.
func (r *Runner) sendEvent(ctx context.Context, event Event) bool {
	select {
	case r.out <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

// testEvent mirrors the `go test -json` event fields used by the folder.
type testEvent struct {
	Action  string
	Package string
	Test    string
	Output  string
	Elapsed float64
}

// suiteState accumulates JSON events until a [SuiteResult] can be built.
type suiteState struct {
	suite                  SuiteTask
	testNames              []string
	outcomes               map[string]TestOutcome
	outputs                map[string][]string
	outputsDropped         map[string]int
	packageOutput          []string
	completedTopLevelTests int
	totalTests             int
	totalKnown             bool
	elapsedSeconds         float64
	failed                 bool
	completed              bool
	killed                 bool
	err                    error
	emitProgress           func(SuiteProgress) bool
}

// runSuite starts `go test`, drains stdout events, waits, and builds a suite result.
func runSuite(
	ctx context.Context,
	tool gotool.Tool,
	moduleDir string,
	suite SuiteTask,
	stallAfter time.Duration,
	emitProgress func(SuiteProgress) bool,
) SuiteResult {
	processCtx, cancelProcess := context.WithCancel(ctx)
	defer cancelProcess()
	total, totalKnown := listSuiteTests(processCtx, tool, moduleDir, suite, stallAfter)
	state := newSuiteState(suite, total, totalKnown, emitProgress)
	state.reportProgress()
	process, err := tool.StartTests(processCtx, moduleDir, suite.Package.ImportPath)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return SuiteResult{Package: suite.Package, Digest: suite.Digest, Killed: true}
		}
		state.err = err
		state.failed = true
		return state.result()
	}
	watchdog := startSuiteWatchdog(processCtx, cancelProcess, stallAfter)
	readEvents(process.Events(), state, eventCountPerSuiteMax, watchdog.activity)
	waitErr := process.Wait()
	if watchdog.stalled() {
		state.err = fmt.Errorf("suite produced no output for %s; killed (child process stalled)", stallAfter)
	}
	watchdog.stop()
	finishSuiteResult(ctx, state, waitErr, process.Stderr())
	return state.result()
}

// listSuiteTests returns a cosmetic test total, falling back to unknown on error.
func listSuiteTests(
	ctx context.Context,
	tool gotool.Tool,
	moduleDir string,
	suite SuiteTask,
	stallAfter time.Duration,
) (int, bool) {
	listCtx, cancel := context.WithTimeout(ctx, stallAfter)
	defer cancel()
	tests, err := tool.ListTests(listCtx, moduleDir, suite.Package.ImportPath)
	if err != nil {
		return 0, false
	}
	return len(tests), true
}

// newSuiteState initializes event-folding state for a suite with tests.
func newSuiteState(
	suite SuiteTask,
	totalTests int,
	totalKnown bool,
	emitProgress func(SuiteProgress) bool,
) *suiteState {
	assert.True(suite.Package.HasTests())
	return &suiteState{
		suite:          suite,
		outcomes:       make(map[string]TestOutcome),
		outputs:        make(map[string][]string),
		outputsDropped: make(map[string]int),
		totalTests:     totalTests,
		totalKnown:     totalKnown,
		emitProgress:   emitProgress,
	}
}

// readEvents folds stdout JSON events and always drains the stream before Wait.
//
// Draining after scanner stop is required because a full stdout pipe can
// deadlock the child process before [gotool.TestProcess.Wait] returns.
// Watchdog activity is reported at the read layer, not per parsed line, so
// mid-line progress on huge output lines and the post-stop drain both count.
func readEvents(stream io.Reader, state *suiteState, eventLimit int, activity func()) {
	events := activityReader{reader: stream, activity: activity}
	scanner := bufio.NewScanner(&events)
	scanner.Buffer(make([]byte, 64*1024), lineBytesMax)
	for eventCount := 0; scanner.Scan(); eventCount++ {
		if eventCount >= eventLimit {
			state.err = fmt.Errorf("suite %s exceeded %d events", state.suite.Package.ImportPath, eventLimit)
			break
		}
		var ev testEvent
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			state.recordPackageOutput(scanner.Text() + "\n")
			continue
		}
		if state.applyEvent(ev) {
			state.reportProgress()
		}
	}
	if scanner.Err() != nil && state.err == nil {
		state.err = fmt.Errorf("reading %s events: %w", state.suite.Package.ImportPath, scanner.Err())
	}
	_, _ = io.Copy(io.Discard, &events)
}

// activityReader reports child stdout bytes before handing them to a caller.
type activityReader struct {
	reader   io.Reader
	activity func()
}

// Read proxies one stream read and reports activity when bytes are received.
func (r *activityReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.activity()
	}
	return n, err
}

// suiteWatchdog owns the per-suite no-output timer and child-process cancel.
type suiteWatchdog struct {
	activityCh chan struct{}
	stopCh     chan struct{}
	doneCh     chan struct{}
	stalledNow atomic.Bool
}

// startSuiteWatchdog starts a timer that kills the child on stdout silence.
//
// The scanner remains a normal blocking read loop. When this timer fires it
// cancels the child process context; the killed process closes stdout, which
// unblocks the scanner with EOF or a read error.
func startSuiteWatchdog(
	ctx context.Context,
	cancel context.CancelFunc,
	stallAfter time.Duration,
) *suiteWatchdog {
	w := &suiteWatchdog{
		activityCh: make(chan struct{}, 1),
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
	go w.run(ctx, cancel, stallAfter)
	return w
}

// run waits for activity, cancellation, explicit stop, or suite silence.
func (w *suiteWatchdog) run(ctx context.Context, cancel context.CancelFunc, stallAfter time.Duration) {
	timer := time.NewTimer(stallAfter)
	defer close(w.doneCh)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			w.stalledNow.Store(true)
			cancel()
			return
		case <-w.activityCh:
			// Go 1.23+ timer semantics guarantee Reset without a drain: no
			// stale tick can be received after Reset returns.
			timer.Reset(stallAfter)
		case <-w.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// activity records child stdout activity without blocking the event reader.
func (w *suiteWatchdog) activity() {
	select {
	case w.activityCh <- struct{}{}:
	default:
	}
}

// stalled reports whether the watchdog killed the child for stdout silence.
func (w *suiteWatchdog) stalled() bool {
	return w.stalledNow.Load()
}

// stop terminates the watchdog goroutine and waits until it exits.
//
// stop is called once per suite. Closing stopCh after the goroutine already
// exited (timer fired or ctx cancelled) is harmless: nothing else touches it.
func (w *suiteWatchdog) stop() {
	close(w.stopCh)
	<-w.doneCh
}

// finishSuiteResult reconciles process exit state with folded package events.
//
// A cancelled ctx means rask killed the child on purpose (fail-fast), so the
// suite is marked killed rather than failed and kill noise is suppressed.
func finishSuiteResult(ctx context.Context, state *suiteState, waitErr error, stderr string) {
	killedByUs := errors.Is(ctx.Err(), context.Canceled)
	if stderr != "" && !killedByUs {
		state.recordPackageOutput(stderr)
	}
	if state.completed {
		if state.err != nil && !killedByUs {
			state.failed = true
		}
		return
	}
	if killedByUs {
		state.failed = false
		state.killed = true
		return
	}
	state.failed = true
	if state.err != nil {
		return
	}
	if waitErr != nil {
		state.err = fmt.Errorf("go test: %w", waitErr)
		return
	}
	state.err = errors.New("go test exited without a package result")
}

// applyEvent routes one JSON event and reports whether progress changed.
func (s *suiteState) applyEvent(ev testEvent) bool {
	if ev.Test == "" {
		s.applyPackageEvent(ev)
		return false
	}
	switch ev.Action {
	case "run":
		s.recordTestStarted(ev.Test)
	case "output":
		s.recordTestOutput(ev.Test, ev.Output)
	case "pass", "fail", "skip":
		return s.recordTerminalTest(ev)
	}
	return false
}

// applyPackageEvent folds package-level events into suite state.
func (s *suiteState) applyPackageEvent(ev testEvent) {
	switch ev.Action {
	case "output", "build-output":
		s.recordPackageOutput(ev.Output)
	case "pass", "fail", "skip":
		s.elapsedSeconds = ev.Elapsed
		s.failed = ev.Action == "fail"
		s.completed = true
	case "start", "run", "build-fail":
	}
}

// recordTerminalTest records a pass, fail, or skip event for a test once.
func (s *suiteState) recordTerminalTest(ev testEvent) bool {
	before, known := s.outcomes[ev.Test]
	if !known {
		s.recordTestStarted(ev.Test)
		before = testRunning
	}
	s.outcomes[ev.Test] = TestOutcome(ev.Action)
	if isTerminalOutcome(before) {
		return false
	}
	if !isTopLevelTestName(ev.Test) {
		return false
	}
	s.completedTopLevelTests++
	return true
}

// isTerminalOutcome reports whether an outcome has already contributed progress.
func isTerminalOutcome(outcome TestOutcome) bool {
	return outcome == TestPassed || outcome == TestFailed || outcome == TestIgnored
}

// isTopLevelTestName reports whether name is counted by go test -list.
func isTopLevelTestName(name string) bool {
	return !strings.Contains(name, "/") && gotool.MatchesListTestsPattern(name)
}

// reportProgress emits the current suite counters when a receiver exists.
func (s *suiteState) reportProgress() {
	if s.emitProgress == nil {
		return
	}
	s.emitProgress(SuiteProgress{
		Package: s.suite.Package, Completed: s.completedTopLevelTests,
		Total: s.totalTests, TotalKnown: s.totalKnown,
	})
}

// recordTestStarted records a test name once and enforces the per-suite bound.
func (s *suiteState) recordTestStarted(name string) {
	if _, known := s.outcomes[name]; known {
		return
	}
	if len(s.testNames) >= testCountPerSuiteMax {
		if s.err == nil {
			s.err = fmt.Errorf("suite %s exceeded %d tests", s.suite.Package.ImportPath, testCountPerSuiteMax)
		}
		return
	}
	s.testNames = append(s.testNames, name)
	s.outcomes[name] = testRunning
}

// recordTestOutput appends bounded diagnostic output for one test.
func (s *suiteState) recordTestOutput(name string, line string) {
	if len(s.outputs[name]) >= outputLinesPerTestKept {
		s.outputsDropped[name]++
		return
	}
	s.outputs[name] = append(s.outputs[name], line)
}

// recordPackageOutput appends bounded package-level diagnostic output.
func (s *suiteState) recordPackageOutput(line string) {
	if len(s.packageOutput) >= outputLinesPerTestKept {
		return
	}
	s.packageOutput = append(s.packageOutput, line)
}

// result converts mutable folding state into an immutable suite result.
func (s *suiteState) result() SuiteResult {
	tests := make([]TestResult, 0, len(s.testNames))
	for _, name := range s.testNames {
		outcome := s.outcomes[name]
		// A test still "running" when its suite concluded or failed crashed
		// or was cut off, so it folds to failed. Killed suites skip the remap
		// so fail-fast cancellation is not misreported as test failures.
		if outcome == testRunning && (s.completed || s.failed) {
			outcome = TestFailed
		}
		tests = append(tests, TestResult{
			Name: name, Outcome: outcome, Output: append([]string(nil), s.outputs[name]...),
			DroppedOutput: s.outputsDropped[name],
		})
	}
	errText := ""
	if s.err != nil {
		errText = s.err.Error()
	}
	return SuiteResult{
		Package: s.suite.Package, Digest: s.suite.Digest, Tests: tests,
		PackageOutput: append([]string(nil), s.packageOutput...), ElapsedSeconds: s.elapsedSeconds,
		Failed: s.failed, Completed: s.completed, Killed: s.killed, Err: errText,
	}
}
