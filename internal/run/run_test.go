package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/chronark/rask/internal/gotool"
)

// TestRunSuiteHandlesStreamLimitsAndNonTerminalTests protects stream draining after event limits.
func TestRunSuiteHandlesStreamLimitsAndNonTerminalTests(t *testing.T) {
	suite := suiteForTest("example.com/m/pkg")
	reader := &chunkReader{chunks: [][]byte{
		[]byte(`{"Action":"run","Test":"TestA"}` + "\n"),
		[]byte(`{"Action":"run","Test":"TestB"}` + "\n"),
		[]byte(`{"Action":"fail","Elapsed":0.01}` + "\n"),
	}}
	state := newSuiteState(suite, 0, false, nil)
	readEvents(reader, state, 1, func() {})
	if reader.index != len(reader.chunks) {
		t.Fatalf("reader stopped at %d, want drained", reader.index)
	}
	finishSuiteResult(context.Background(), state, nil, "")
	result := state.result()
	if !result.Failed || len(result.Tests) != 1 || result.Tests[0].Outcome != TestFailed {
		t.Fatalf("result = %+v, want failed limited suite", result)
	}
}

// TestRunSuiteLongLineFailsWithoutPanic protects scanner-bound handling for huge output lines.
func TestRunSuiteLongLineFailsWithoutPanic(t *testing.T) {
	longLine := strings.Repeat("x", lineBytesMax+1) + "\n"
	tool := &fakeTool{streams: map[string]string{"example.com/m/pkg": longLine}}
	result := runSuite(context.Background(), tool, "/tmp", suiteForTest("example.com/m/pkg"),
		defaultSuiteStallAfter, nil)
	if !result.Failed || result.Err == "" {
		t.Fatalf("result = %+v, want long-line failure", result)
	}
}

// TestRunSuiteWatchdogKillsSilentChild protects the per-suite stall watchdog.
func TestRunSuiteWatchdogKillsSilentChild(t *testing.T) {
	suite := suiteForTest("example.com/m/stalled")
	tool := &fakeTool{readers: map[string]readerFactory{
		suite.Package.ImportPath: func(ctx context.Context) io.Reader {
			return &blockingReader{ctx: ctx, chunks: [][]byte{
				[]byte(`{"Action":"run","Test":"TestSlow"}` + "\n"),
			}}
		},
	}}
	started := time.Now()
	result := runSuite(context.Background(), tool, "/tmp", suite, 20*time.Millisecond, nil)
	if time.Since(started) > time.Second {
		t.Fatalf("watchdog took too long to kill stalled suite")
	}
	if !result.Failed || result.Killed || !strings.Contains(result.Err, "suite produced no output for 20ms") {
		t.Fatalf("result = %+v, want stall failure", result)
	}
}

// TestRunSuiteWatchdogResetsOnActivity protects long but active child streams.
func TestRunSuiteWatchdogResetsOnActivity(t *testing.T) {
	suite := suiteForTest("example.com/m/active")
	tool := &fakeTool{readers: map[string]readerFactory{
		suite.Package.ImportPath: func(ctx context.Context) io.Reader {
			return steadyReader(ctx, 10*time.Millisecond, []string{
				`{"Action":"run","Test":"TestSlow"}` + "\n",
				`{"Action":"output","Test":"TestSlow","Output":"still running\n"}` + "\n",
				`{"Action":"pass","Test":"TestSlow"}` + "\n",
				`{"Action":"pass","Elapsed":0.04}` + "\n",
			})
		},
	}}
	result := runSuite(context.Background(), tool, "/tmp", suite, 30*time.Millisecond, nil)
	if result.Failed || result.Killed || strings.Contains(result.Err, "child process stalled") {
		t.Fatalf("result = %+v, want active suite to pass", result)
	}
}

// TestRunSuiteFailFastKillRemainsSkipped protects fail-fast versus stall errors.
func TestRunSuiteFailFastKillRemainsSkipped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	suite := suiteForTest("example.com/m/canceled")
	tool := &fakeTool{readers: map[string]readerFactory{
		suite.Package.ImportPath: func(ctx context.Context) io.Reader {
			return &blockingReader{ctx: ctx}
		},
	}}
	resultCh := make(chan SuiteResult, 1)
	go func() {
		resultCh <- runSuite(ctx, tool, "/tmp", suite, time.Second, nil)
	}()
	cancel()
	select {
	case result := <-resultCh:
		if result.Failed || !result.Killed || result.Err != "" {
			t.Fatalf("result = %+v, want silent fail-fast skip", result)
		}
	case <-time.After(time.Second):
		t.Fatalf("fail-fast cancellation did not stop suite")
	}
}

// TestRunSuiteEmitsProgress protects live per-suite terminal-event counters.
func TestRunSuiteEmitsProgress(t *testing.T) {
	suite := suiteForTest("example.com/m/progress")
	tool := &fakeTool{
		listed: map[string][]string{suite.Package.ImportPath: {"TestA", "TestB"}},
		streams: map[string]string{suite.Package.ImportPath: `{"Action":"run","Test":"TestA"}` + "\n" +
			`{"Action":"pass","Test":"TestA"}` + "\n" +
			`{"Action":"run","Test":"TestB"}` + "\n" +
			`{"Action":"skip","Test":"TestB"}` + "\n" +
			`{"Action":"pass","Elapsed":0.01}` + "\n"},
	}
	var progress []SuiteProgress
	result := runSuite(context.Background(), tool, "/tmp", suite, time.Second, func(p SuiteProgress) bool {
		progress = append(progress, p)
		return true
	})
	if result.Failed {
		t.Fatalf("result = %+v, want passing suite", result)
	}
	wantCompleted := []int{0, 1, 2}
	if len(progress) != len(wantCompleted) {
		t.Fatalf("progress count = %d, want %d: %+v", len(progress), len(wantCompleted), progress)
	}
	for index, want := range wantCompleted {
		got := progress[index]
		if got.Completed != want || got.Total != 2 || !got.TotalKnown {
			t.Fatalf("progress[%d] = %+v, want completed=%d total=2 known", index, got, want)
		}
		if index > 0 && progress[index].Completed < progress[index-1].Completed {
			t.Fatalf("progress regressed: %+v", progress)
		}
	}
}

// TestRunSuiteProgressCountsOnlyTopLevelCompletions protects x/y unit matching.
func TestRunSuiteProgressCountsOnlyTopLevelCompletions(t *testing.T) {
	suite := suiteForTest("example.com/m/subtests")
	tool := &fakeTool{
		listed: map[string][]string{suite.Package.ImportPath: {"TestA"}},
		streams: map[string]string{suite.Package.ImportPath: `{"Action":"run","Test":"TestA"}` + "\n" +
			`{"Action":"run","Test":"TestA/one"}` + "\n" +
			`{"Action":"pass","Test":"TestA/one"}` + "\n" +
			`{"Action":"run","Test":"TestA/two"}` + "\n" +
			`{"Action":"pass","Test":"TestA/two"}` + "\n" +
			`{"Action":"pass","Test":"TestA"}` + "\n" +
			`{"Action":"pass","Elapsed":0.01}` + "\n"},
	}
	var progress []SuiteProgress
	result := runSuite(context.Background(), tool, "/tmp", suite, time.Second, func(p SuiteProgress) bool {
		progress = append(progress, p)
		return true
	})
	if result.Failed {
		t.Fatalf("result = %+v, want passing suite", result)
	}
	wantCompleted := []int{0, 1}
	if len(progress) != len(wantCompleted) {
		t.Fatalf("progress count = %d, want %d: %+v", len(progress), len(wantCompleted), progress)
	}
	for index, want := range wantCompleted {
		got := progress[index]
		if got.Completed != want || got.Total != 1 || !got.TotalKnown {
			t.Fatalf("progress[%d] = %+v, want completed=%d total=1 known", index, got, want)
		}
		if got.TotalKnown && got.Completed > got.Total {
			t.Fatalf("progress[%d] exceeded known total: %+v", index, got)
		}
	}
	if len(result.Tests) != 3 {
		t.Fatalf("result tests = %d, want top-level plus subtests: %+v", len(result.Tests), result.Tests)
	}
}

// TestRunSuiteProgressIgnoresUnlistedTopLevelEvents protects x/y unit matching.
func TestRunSuiteProgressIgnoresUnlistedTopLevelEvents(t *testing.T) {
	suite := suiteForTest("example.com/m/examples-fuzz")
	tool := &fakeTool{
		listed: map[string][]string{suite.Package.ImportPath: {"TestA", "TestB"}},
		streams: map[string]string{suite.Package.ImportPath: `{"Action":"pass","Test":"TestA"}` + "\n" +
			`{"Action":"pass","Test":"TestA/sub"}` + "\n" +
			`{"Action":"pass","Test":"ExampleDoc"}` + "\n" +
			`{"Action":"pass","Test":"FuzzParse"}` + "\n" +
			`{"Action":"pass","Test":"TestB"}` + "\n" +
			`{"Action":"pass","Elapsed":0.01}` + "\n"},
	}
	var progress []SuiteProgress
	result := runSuite(context.Background(), tool, "/tmp", suite, time.Second, func(p SuiteProgress) bool {
		progress = append(progress, p)
		return true
	})
	if result.Failed {
		t.Fatalf("result = %+v, want passing suite", result)
	}
	wantCompleted := []int{0, 1, 2}
	if len(progress) != len(wantCompleted) {
		t.Fatalf("progress count = %d, want %d: %+v", len(progress), len(wantCompleted), progress)
	}
	for index, want := range wantCompleted {
		got := progress[index]
		if got.Completed != want || got.Total != 2 || !got.TotalKnown {
			t.Fatalf("progress[%d] = %+v, want completed=%d total=2 known", index, got, want)
		}
	}
	if len(result.Tests) != 5 {
		t.Fatalf("result tests = %d, want tests, subtest, example, and fuzz: %+v", len(result.Tests), result.Tests)
	}
}

// TestRunSuiteProgressSurvivesListFailure protects cosmetic enumeration fallback.
func TestRunSuiteProgressSurvivesListFailure(t *testing.T) {
	suite := suiteForTest("example.com/m/unknown")
	tool := &fakeTool{
		listErrs: map[string]error{suite.Package.ImportPath: fmt.Errorf("list failed")},
		streams: map[string]string{suite.Package.ImportPath: `{"Action":"run","Test":"TestA"}` + "\n" +
			`{"Action":"pass","Test":"TestA"}` + "\n" +
			`{"Action":"pass","Elapsed":0.01}` + "\n"},
	}
	var progress []SuiteProgress
	result := runSuite(context.Background(), tool, "/tmp", suite, time.Second, func(p SuiteProgress) bool {
		progress = append(progress, p)
		return true
	})
	if result.Failed || len(progress) != 2 {
		t.Fatalf("result=%+v progress=%+v, want passing suite with progress", result, progress)
	}
	for _, got := range progress {
		if got.TotalKnown || got.Total != 0 {
			t.Fatalf("progress = %+v, want unknown total", got)
		}
	}
}

// TestRunnerFixedWorkerCountRunsSequentially protects caller-owned suite parallelism.
func TestRunnerFixedWorkerCountRunsSequentially(t *testing.T) {
	first := suiteForTest("example.com/m/first")
	second := suiteForTest("example.com/m/second")
	releaseFirst := make(chan struct{})
	tool := &fakeTool{readers: map[string]readerFactory{
		first.Package.ImportPath: func(context.Context) io.Reader {
			return &releasedReader{release: releaseFirst, body: passStream()}
		},
		second.Package.ImportPath: func(context.Context) io.Reader {
			return strings.NewReader(passStream())
		},
	}}
	tasks := make(chan SuiteTask, 2)
	results := make(chan Event, 16)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	runner := New(tool, "/tmp", tasks, results, 1)
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	tasks <- first
	tasks <- second
	close(tasks)
	wantStartedSuite(t, ctx, results, first)
	assertNoSecondStartBeforeFirstResult(t, results, second)
	close(releaseFirst)
	wantResultBeforeStart(t, ctx, results, first, second)
	wantStartedSuite(t, ctx, results, second)
	waitRunnerDone(t, ctx, done)
}

// TestRunSuitePreservesShuffleSeedPackageOutput pins go's package-level shuffle event.
func TestRunSuitePreservesShuffleSeedPackageOutput(t *testing.T) {
	suite := suiteForTest("example.com/m/shuffled")
	tool := &fakeTool{streams: map[string]string{suite.Package.ImportPath: `{"Action":"output","Output":"-test.shuffle 12345\n"}` + "\n" +
		`{"Action":"run","Test":"TestBad"}` + "\n" +
		`{"Action":"output","Test":"TestBad","Output":"boom\n"}` + "\n" +
		`{"Action":"fail","Test":"TestBad"}` + "\n" +
		`{"Action":"fail","Elapsed":0.01}` + "\n"}}
	result := runSuite(context.Background(), tool, "/tmp", suite, time.Second, nil)
	if !result.Failed || len(result.PackageOutput) != 1 ||
		result.PackageOutput[0] != "-test.shuffle 12345\n" {
		t.Fatalf("result = %+v, want retained package shuffle seed", result)
	}
}

// suiteForTest returns a runner suite with a valid cache-shaped digest.
func suiteForTest(importPath string) SuiteTask {
	return SuiteTask{
		Package: gotool.Package{Dir: "/tmp", ImportPath: importPath, TestGoFiles: []string{"x_test.go"}},
		Digest:  strings.Repeat("c", 64),
	}
}

// passStream gives runner tests the smallest complete package result.
func passStream() string {
	return `{"Action":"pass","Elapsed":0.01}` + "\n"
}

// wantStartedSuite waits for a worker-start event for one suite.
func wantStartedSuite(t *testing.T, ctx context.Context, events <-chan Event, suite SuiteTask) {
	t.Helper()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatalf("results closed before %s started", suite.Package.ImportPath)
			}
			if event.Kind == EventSuiteStarted && event.Suite.Package.ImportPath == suite.Package.ImportPath {
				return
			}
		case <-ctx.Done():
			t.Fatalf("waiting for %s start: %v", suite.Package.ImportPath, ctx.Err())
		}
	}
}

// assertNoSecondStartBeforeFirstResult proves workers=1 is not consuming task two yet.
func assertNoSecondStartBeforeFirstResult(t *testing.T, events <-chan Event, second SuiteTask) {
	t.Helper()
	timer := time.NewTimer(30 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.Kind == EventSuiteStarted && event.Suite.Package.ImportPath == second.Package.ImportPath {
				t.Fatalf("second suite started before first suite completed")
			}
			if event.Kind == EventSuiteResult {
				t.Fatalf("first suite completed before release")
			}
		case <-timer.C:
			return
		}
	}
}

// wantResultBeforeStart proves task two starts only after task one emits its result.
func wantResultBeforeStart(t *testing.T, ctx context.Context, events <-chan Event, first SuiteTask, second SuiteTask) {
	t.Helper()
	for {
		select {
		case event := <-events:
			if event.Kind == EventSuiteStarted && event.Suite.Package.ImportPath == second.Package.ImportPath {
				t.Fatalf("second suite started before first suite result")
			}
			if event.Kind == EventSuiteResult && event.Result.Package.ImportPath == first.Package.ImportPath {
				return
			}
		case <-ctx.Done():
			t.Fatalf("waiting for first result: %v", ctx.Err())
		}
	}
}

// waitRunnerDone waits for the runner to close after all controlled streams finish.
func waitRunnerDone(t *testing.T, ctx context.Context, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runner error: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("runner did not finish: %v", ctx.Err())
	}
}

// fakeTool provides process streams for runner tests.
type fakeTool struct {
	streams  map[string]string
	readers  map[string]readerFactory
	stderr   map[string]string
	listed   map[string][]string
	listErrs map[string]error
}

// readerFactory builds a fake process event stream bound to StartTests context.
type readerFactory func(context.Context) io.Reader

// CurrentModule fails because runner tests pass modules directly.
func (f *fakeTool) CurrentModule() (gotool.Module, error) {
	return gotool.Module{}, fmt.Errorf("unexpected CurrentModule call")
}

// ListPackages fails because runner tests pass suites directly.
func (f *fakeTool) ListPackages(string, []string) ([]gotool.Package, error) {
	return nil, fmt.Errorf("unexpected ListPackages call")
}

// ListModules fails because runner tests pass suites directly.
func (f *fakeTool) ListModules(string) ([]gotool.Module, error) {
	return nil, fmt.Errorf("unexpected ListModules call")
}

// Environment fails because runner tests do not fingerprint.
func (f *fakeTool) Environment(string) ([]byte, error) {
	return nil, fmt.Errorf("unexpected Environment call")
}

// ListTests returns configured names or an empty successful listing.
func (f *fakeTool) ListTests(ctx context.Context, _ string, importPath string) ([]string, error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, ctx.Err()
	}
	if err := f.listErrs[importPath]; err != nil {
		return nil, err
	}
	return append([]string(nil), f.listed[importPath]...), nil
}

// StartTests returns a fake process for importPath or ctx cancellation.
func (f *fakeTool) StartTests(ctx context.Context, _ string, importPath string) (gotool.TestProcess, error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, ctx.Err()
	}
	if reader, ok := f.readers[importPath]; ok {
		return fakeProcess{
			events: reader(ctx), stderr: f.stderr[importPath],
		}, nil
	}
	stream, ok := f.streams[importPath]
	if !ok {
		return nil, fmt.Errorf("missing stream for %s", importPath)
	}
	return fakeProcess{
		events: strings.NewReader(stream), stderr: f.stderr[importPath],
	}, nil
}

// fakeProcess implements gotool.TestProcess for runner tests.
type fakeProcess struct {
	events io.Reader
	stderr string
}

// Events returns the configured event stream.
func (p fakeProcess) Events() io.Reader {
	return p.events
}

// Wait succeeds because runner tests exercise event parsing, not process errors.
func (p fakeProcess) Wait() error {
	return nil
}

// Stderr returns the configured stderr text.
func (p fakeProcess) Stderr() string {
	return p.stderr
}

// releasedReader blocks a fake suite until the test allows it to complete.
type releasedReader struct {
	release <-chan struct{}
	body    string
	sent    bool
}

// Read waits for release so tests can inspect runner scheduling first.
func (r *releasedReader) Read(p []byte) (int, error) {
	<-r.release
	if r.sent {
		return 0, io.EOF
	}
	r.sent = true
	return copy(p, r.body), nil
}

// chunkReader returns one configured chunk per Read call.
type chunkReader struct {
	chunks [][]byte
	index  int
}

// Read copies the next chunk into p or returns EOF after all chunks.
func (r *chunkReader) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.index])
	r.index++
	return n, nil
}

// blockingReader returns configured chunks and then blocks until ctx cancellation.
type blockingReader struct {
	ctx    context.Context
	chunks [][]byte
	index  int
}

// Read copies one chunk or waits for cancellation after all chunks are consumed.
func (r *blockingReader) Read(p []byte) (int, error) {
	if r.index < len(r.chunks) {
		n := copy(p, r.chunks[r.index])
		r.index++
		return n, nil
	}
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

// steadyReader writes lines to a pipe until every line is sent or ctx is cancelled.
func steadyReader(ctx context.Context, delay time.Duration, lines []string) io.Reader {
	reader, writer := io.Pipe()
	go func() {
		defer writer.Close()
		for _, line := range lines {
			select {
			case <-ctx.Done():
				_ = writer.CloseWithError(ctx.Err())
				return
			case <-time.After(delay):
			}
			if _, err := io.WriteString(writer, line); err != nil {
				return
			}
		}
	}()
	return reader
}
