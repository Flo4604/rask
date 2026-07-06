package frontend

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// TestFrontendProgressRegionTicks protects synchronized ticker frames.
func TestFrontendProgressRegionTicks(t *testing.T) {
	out := new(recordingWriter)
	ticks := make(chan time.Time)
	events := make(chan Event)
	frontend := New(out, new(bytes.Buffer), true, events)
	frontend.ConfigureRun(false, "example.com/m")
	frontend.color = false
	frontend.width = 120
	now := time.Unix(100, 0)
	frontend.now = func() time.Time { return now }
	frontend.startedAt = now.Add(-time.Second)
	frontend.ticks = ticks
	t.Setenv("COLUMNS", "120")
	done := make(chan error, 1)
	go func() {
		_, err := frontend.Run(context.Background())
		done <- err
	}()
	sendEvent(t, events, Event{Kind: EventRunPlan, RunPlan: RunPlan{Suites: 1}})
	sendEvent(t, events, Event{Kind: EventStarted, ImportPath: "example.com/m/pkg"})
	sendTick(t, ticks, now)
	writes := waitForWrites(t, out, 1, time.Second)
	rows := frontend.progressLines(now)
	want := terminalFrame(0, "", rows)
	if writes[0] != want {
		t.Fatalf("terminal frame mismatch\nwant:\n%q\ngot:\n%q", want, writes[0])
	}
	sendEvent(t, events, Event{Kind: EventSummary, Summary: Summary{Suites: 1, Elapsed: 10 * time.Millisecond}})
	close(events)
	if err := <-done; err != nil {
		t.Fatalf("frontend run: %v", err)
	}
	writes = waitForWrites(t, out, 2, time.Second)
	want = terminalFrame(len(rows), "Finished 1 suite, 0 tests: 0 passed; 0 failed; 0 ignored in 10ms\n", nil)
	if writes[1] != want {
		t.Fatalf("completion frame mismatch\nwant:\n%q\ngot:\n%q", want, writes[1])
	}
}

// TestFrontendProgressRegionDebouncesEventBursts protects event batching cadence.
func TestFrontendProgressRegionDebouncesEventBursts(t *testing.T) {
	out := new(recordingWriter)
	ticks := make(chan time.Time)
	events := make(chan Event)
	frontend := New(out, new(bytes.Buffer), true, events)
	frontend.ConfigureRun(false, "example.com/m")
	frontend.color = false
	frontend.width = 120
	now := time.Unix(200, 0)
	frontend.now = func() time.Time { return now }
	frontend.startedAt = now.Add(-1500 * time.Millisecond)
	frontend.ticks = ticks
	done := runFrontendForTest(frontend)
	sendEvent(t, events, Event{Kind: EventRunPlan, RunPlan: RunPlan{Suites: 2}})
	sendEvent(t, events, Event{Kind: EventStarted, ImportPath: "example.com/m/run"})
	sendEvent(t, events, Event{Kind: EventTestProgress, TestProgress: TestProgress{
		ImportPath: "example.com/m/run", Completed: 1, Total: 3, TotalKnown: true,
	}})
	sendEvent(t, events, Event{Kind: EventTestProgress, TestProgress: TestProgress{
		ImportPath: "example.com/m/run", Completed: 2, Total: 3, TotalKnown: true,
	}})
	sendEvent(t, events, Event{Kind: EventCached, Cached: CachedEvent{ImportPath: "example.com/m/cached", Passed: 1}})
	if writes := out.Writes(); len(writes) != 0 {
		t.Fatalf("writes before tick = %d, want 0:\n%q", len(writes), writes)
	}
	sendTick(t, ticks, now)
	writes := waitForWrites(t, out, 1, time.Second)
	rows := frontend.progressLines(now)
	want := terminalFrame(0, "Cached cached\n", rows)
	if writes[0] != want {
		t.Fatalf("batched frame mismatch\nwant:\n%q\ngot:\n%q", want, writes[0])
	}
	assertFrameClearBeforeContent(t, writes[0], "Cached cached")
	assertFrameClearBeforeContent(t, writes[0], rows[0])
	sendEvent(t, events, Event{Kind: EventSummary, Summary: Summary{
		Suites: 2, Tests: 1, Passed: 1, Cached: 1, Elapsed: time.Second,
	}})
	close(events)
	if err := <-done; err != nil {
		t.Fatalf("frontend run: %v", err)
	}
	writes = waitForWrites(t, out, 2, time.Second)
	if got := strings.Count(strings.Join(writes, ""), "Cached cached"); got != 1 {
		t.Fatalf("cached line count = %d, want 1:\n%q", got, writes)
	}
}

// TestFrontendProgressRegionShrinkFrameClearsLeftovers protects in-place shrink repaint bytes.
func TestFrontendProgressRegionShrinkFrameClearsLeftovers(t *testing.T) {
	out := new(recordingWriter)
	ticks := make(chan time.Time)
	events := make(chan Event)
	frontend := New(out, new(bytes.Buffer), true, events)
	frontend.ConfigureRun(false, "example.com/m")
	frontend.color = false
	frontend.width = 120
	now := time.Unix(300, 0)
	frontend.now = func() time.Time { return now }
	frontend.startedAt = now
	frontend.ticks = ticks
	done := runFrontendForTest(frontend)
	sendEvent(t, events, Event{Kind: EventRunPlan, RunPlan: RunPlan{Suites: 2}})
	sendEvent(t, events, Event{Kind: EventStarted, ImportPath: "example.com/m/a"})
	sendEvent(t, events, Event{Kind: EventStarted, ImportPath: "example.com/m/b"})
	sendTick(t, ticks, now)
	waitForWrites(t, out, 1, time.Second)
	sendEvent(t, events, Event{Kind: EventSuiteResult, SuiteResult: SuiteResult{ImportPath: "example.com/m/a"}})
	sendTick(t, ticks, now)
	writes := waitForWrites(t, out, 2, time.Second)
	rows := frontend.progressLines(now)
	want := terminalFrame(3, suiteLine("Passed", "a", "", "0s")+"\n", rows)
	if writes[1] != want {
		t.Fatalf("shrink frame mismatch\nwant:\n%q\ngot:\n%q", want, writes[1])
	}
	if got := strings.Count(writes[1], eraseToEndOfScreen); got != 1 {
		t.Fatalf("shrink frame clear-screen count = %d, want 1: %q", got, writes[1])
	}
	close(events)
	if err := <-done; err != nil {
		t.Fatalf("frontend run: %v", err)
	}
}

// TestFrontendPermanentRowsDoNotWrapAtNarrowTerminal protects cursor-up math.
func TestFrontendPermanentRowsDoNotWrapAtNarrowTerminal(t *testing.T) {
	out := new(recordingWriter)
	ticks := make(chan time.Time)
	events := make(chan Event)
	frontend := New(out, new(bytes.Buffer), true, events)
	frontend.ConfigureRun(false, "example.com/m")
	frontend.color = false
	frontend.width = 60
	now := time.Unix(350, 0)
	frontend.now = func() time.Time { return now }
	frontend.startedAt = now.Add(-17 * time.Second)
	frontend.ticks = ticks
	done := runFrontendForTest(frontend)
	longPath := "example.com/m/internal/" + strings.Repeat("nested/", 8) + "leaf"
	sendEvent(t, events, Event{Kind: EventRunPlan, RunPlan: RunPlan{
		Suites:             2,
		MaxImportPathWidth: runeLen(ShortenImportPath(longPath, "example.com/m")),
	}})
	sendEvent(t, events, Event{Kind: EventStarted, ImportPath: longPath})
	sendTick(t, ticks, now)
	writes := waitForWrites(t, out, 1, time.Second)
	assertFrameVisualLinesWithinWidth(t, writes[0], frontend.width)
	sendEvent(t, events, Event{Kind: EventSuiteResult, SuiteResult: suiteWithCounts(longPath, 16369, 0, 2330)})
	sendTick(t, ticks, now)
	writes = waitForWrites(t, out, 2, time.Second)
	assertFrameVisualLinesWithinWidth(t, writes[1], frontend.width)
	if !strings.Contains(writes[1], "\r\x1b[1A"+eraseToEndOfScreen) {
		t.Fatalf("second frame did not erase from counted header row: %q", writes[1])
	}
	visible := visibleFrameText(writes[1])
	if strings.Count(visible, "Testing ") != 1 || strings.Contains(visible, "Testing 0/2") {
		t.Fatalf("stale progress header survived in second frame: %q", visible)
	}
	sendEvent(t, events, Event{Kind: EventSummary, Summary: Summary{Suites: 2, Elapsed: time.Second}})
	close(events)
	if err := <-done; err != nil {
		t.Fatalf("frontend run: %v", err)
	}
}

// TestFrontendProgressRegionDrawsRunningRowsLongestFirst protects row ordering.
func TestFrontendProgressRegionDrawsRunningRowsLongestFirst(t *testing.T) {
	out := new(bytes.Buffer)
	frontend := New(out, new(bytes.Buffer), true, nil)
	frontend.ConfigureRun(false, "example.com/m")
	frontend.color = false
	frontend.width = 120
	now := time.Unix(100, 0)
	frontend.now = func() time.Time { return now }
	frontend.startedAt = now.Add(-8300 * time.Millisecond)
	frontend.total, frontend.done, frontend.passed, frontend.ignored = 74, 12, 40, 1
	frontend.running = []runningSuite{
		{importPath: "example.com/m/pkg/cache", startedAt: now.Add(-4100 * time.Millisecond),
			completed: 3, total: 9, totalKnown: true},
		{importPath: "example.com/m/svc/api/integration", startedAt: now.Add(-14200 * time.Millisecond),
			completed: 12, total: 45, totalKnown: true},
		{importPath: "example.com/m/pkg/uid", startedAt: now.Add(-800 * time.Millisecond),
			completed: 0, totalKnown: false},
	}
	got := progressRegionOutput(frontend)
	lines := strings.Split(got, "\n")
	if len(lines) != 4 {
		t.Fatalf("progress lines = %d, want 4:\n%q", len(lines), got)
	}
	want := "Testing 12/74 suites | 41 tests: 40 passed; 0 failed; 1 ignored | 8.3s"
	if lines[0] != want {
		t.Fatalf("progress header mismatch\nwant:\n%q\ngot:\n%q", want, lines[0])
	}
	if !strings.Contains(lines[1], "svc/api/integration") ||
		!strings.Contains(lines[1], "(12/45)") || !strings.HasSuffix(lines[1], "14.2s") {
		t.Fatalf("first row = %q, want oldest suite", lines[1])
	}
	if !strings.Contains(lines[2], "pkg/cache") ||
		!strings.Contains(lines[2], "(3/9)") || !strings.HasSuffix(lines[2], "4.1s") {
		t.Fatalf("second row = %q, want next-oldest suite", lines[2])
	}
	if !strings.Contains(lines[3], "pkg/uid") || !strings.Contains(lines[3], "(0/?)") ||
		strings.Contains(lines[3], "<1s") || strings.Contains(lines[3], "0.8s") {
		t.Fatalf("third row = %q, want newest suite without sub-second elapsed", lines[3])
	}
	if strings.Index(lines[1], "(12/45)") != strings.Index(lines[2], "(3/9)") ||
		strings.LastIndex(lines[1], "14.2s") != strings.LastIndex(lines[2], "4.1s") {
		t.Fatalf("columns not aligned:\n%q\n%q", lines[1], lines[2])
	}
	if strings.HasSuffix(got, "\n") {
		t.Fatalf("progress region has trailing newline: %q", got)
	}
}

// TestFrontendProgressRegionCapsOverflow protects the fixed-height invariant.
func TestFrontendProgressRegionCapsOverflow(t *testing.T) {
	out := new(bytes.Buffer)
	frontend := New(out, new(bytes.Buffer), true, nil)
	frontend.ConfigureRun(false, "example.com/m")
	frontend.color = false
	frontend.width = 80
	now := time.Unix(100, 0)
	frontend.now = func() time.Time { return now }
	frontend.startedAt = now
	frontend.total = 40
	for i := 0; i < runningSuiteRowsMax; i++ {
		frontend.running = append(frontend.running, runningSuite{
			importPath: fmt.Sprintf("example.com/m/pkg/%02d", i),
			startedAt:  now.Add(-time.Duration(i) * time.Second),
		})
	}
	got := progressRegionOutput(frontend)
	lines := strings.Split(got, "\n")
	if len(lines) != runningSuiteRowsMax+1 {
		t.Fatalf("progress lines at cap = %d, want cap + header:\n%q", len(lines), got)
	}
	if strings.Contains(got, "... and") {
		t.Fatalf("progress at cap should not overflow:\n%q", got)
	}
	frontend.linesDrawn = 0
	frontend.running = append(frontend.running, runningSuite{
		importPath: "example.com/m/pkg/overflow",
		startedAt:  now.Add(-time.Minute),
	})
	got = progressRegionOutput(frontend)
	lines = strings.Split(got, "\n")
	if len(lines) != runningSuiteRowsMax+1 {
		t.Fatalf("progress lines past cap = %d, want cap + header:\n%q", len(lines), got)
	}
	if lines[len(lines)-1] != "    ... and 2 more" {
		t.Fatalf("overflow row = %q, want and-more row", lines[len(lines)-1])
	}
}

// TestFrontendProgressRegionEraseMovesToTop protects multi-row erase bytes.
func TestFrontendProgressRegionEraseMovesToTop(t *testing.T) {
	out := new(bytes.Buffer)
	frontend := New(out, new(bytes.Buffer), true, nil)
	frontend.ConfigureRun(false, "example.com/m")
	frontend.color = false
	frontend.width = 120
	now := time.Unix(100, 0)
	frontend.now = func() time.Time { return now }
	frontend.startedAt = now
	frontend.total = 4
	frontend.running = []runningSuite{
		{importPath: "example.com/m/a", startedAt: now},
		{importPath: "example.com/m/b", startedAt: now},
		{importPath: "example.com/m/c", startedAt: now},
	}
	frontend.linesDrawn = len(frontend.terminalProgressRows(false))
	out.Reset()
	frontend.erase()
	want := "\r\x1b[3A\x1b[0J"
	if out.String() != want {
		t.Fatalf("erase bytes mismatch\nwant: %q\ngot:  %q", want, out.String())
	}
}

// TestFrontendProgressRegionRemovesFinishedSuites protects row lifecycle.
func TestFrontendProgressRegionRemovesFinishedSuites(t *testing.T) {
	out := new(bytes.Buffer)
	frontend := New(out, new(bytes.Buffer), true, nil)
	frontend.ConfigureRun(false, "example.com/m")
	frontend.color = false
	frontend.width = 120
	now := time.Unix(100, 0)
	frontend.now = func() time.Time { return now }
	frontend.startedAt = now
	frontend.total = 3
	frontend.running = []runningSuite{
		{importPath: "example.com/m/pkg/one", startedAt: now.Add(-2 * time.Second)},
		{importPath: "example.com/m/pkg/two", startedAt: now.Add(-time.Second)},
	}
	progressRegionOutput(frontend)
	frontend.suiteFinished(SuiteResult{ImportPath: "example.com/m/pkg/one"})
	out.Reset()
	got := progressRegionOutput(frontend)
	if strings.Contains(got, "pkg/one") {
		t.Fatalf("finished suite still visible:\n%q", got)
	}
	if !strings.Contains(got, "pkg/two") {
		t.Fatalf("running suite missing:\n%q", got)
	}
}

// TestFrontendProgressRegionTruncatesRunes protects multi-byte import paths.
func TestFrontendProgressRegionTruncatesRunes(t *testing.T) {
	out := new(bytes.Buffer)
	frontend := New(out, new(bytes.Buffer), true, nil)
	frontend.ConfigureRun(false, "example.com/m")
	frontend.color = false
	frontend.width = 20
	now := time.Unix(100, 0)
	frontend.now = func() time.Time { return now }
	frontend.startedAt = now
	frontend.total = 2
	frontend.running = []runningSuite{
		{importPath: "example.com/m/påkage", startedAt: now},
	}
	got := progressRegionOutput(frontend)
	if !strings.Contains(got, "…") {
		t.Fatalf("progress output = %q, want ellipsis", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("progress output split a rune: %q", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if runeLen(line) > frontend.noWrapWidth() {
			t.Fatalf("line wrapped width %d > %d: %q", runeLen(line), frontend.noWrapWidth(), line)
		}
	}
}

// TestFrontendProgressRegionKeepsCountersOnNarrowRows protects path-first truncation.
func TestFrontendProgressRegionKeepsCountersOnNarrowRows(t *testing.T) {
	out := new(bytes.Buffer)
	frontend := New(out, new(bytes.Buffer), true, nil)
	frontend.ConfigureRun(false, "example.com/m")
	frontend.color = false
	frontend.width = 38
	now := time.Unix(100, 0)
	frontend.now = func() time.Time { return now }
	frontend.startedAt = now
	frontend.total = 2
	frontend.running = []runningSuite{
		{
			importPath: "example.com/m/really/long/package/name",
			startedAt:  now.Add(-12300 * time.Millisecond),
			completed:  7, total: 11, totalKnown: true,
		},
	}
	got := progressRegionOutput(frontend)
	lines := strings.Split(got, "\n")
	if !strings.Contains(lines[1], "(7/11)") || !strings.HasSuffix(lines[1], "12.3s") {
		t.Fatalf("row lost progress or elapsed: %q", lines[1])
	}
	if runeLen(lines[1]) > frontend.noWrapWidth() || !utf8.ValidString(lines[1]) {
		t.Fatalf("row overflowed or split rune: %q", lines[1])
	}
}

// TestFrontendProgressEventsDoNotAffectNonTerminalOutput protects CI output.
func TestFrontendProgressEventsDoNotAffectNonTerminalOutput(t *testing.T) {
	out := renderEvents(t,
		false,
		Event{Kind: EventStarted, ImportPath: "example.com/m/pkg"},
		Event{Kind: EventTestProgress, TestProgress: TestProgress{
			ImportPath: "example.com/m/pkg", Completed: 1, Total: 2, TotalKnown: true,
		}},
		Event{Kind: EventSuiteResult, SuiteResult: happySuiteFixture()},
	)
	want := suiteLine("Passed", "pkg", "2 passed; 1 ignored", "250ms") + "\n"
	if out != want {
		t.Fatalf("non-terminal output mismatch\nwant:\n%q\ngot:\n%q", want, out)
	}
}

// TestFrontendSuiteBlockQuietHappyPathIsDense protects quiet successful suite rendering.
func TestFrontendSuiteBlockQuietHappyPathIsDense(t *testing.T) {
	out := renderEvents(t, false, Event{Kind: EventSuiteResult, SuiteResult: happySuiteFixture()})
	want := suiteLine("Passed", "pkg", "2 passed; 1 ignored", "250ms") + "\n"
	if out != want {
		t.Fatalf("quiet happy suite output mismatch\nwant:\n%q\ngot:\n%q", want, out)
	}
}

// TestFrontendSuiteBlockVerboseHappyPathIsDense protects verbose successful suite rendering.
func TestFrontendSuiteBlockVerboseHappyPathIsDense(t *testing.T) {
	out := renderEvents(t, true, Event{Kind: EventSuiteResult, SuiteResult: happySuiteFixture()})
	want := suiteLine("Passed", "pkg", "2 passed; 1 ignored", "250ms") + "\n" +
		"test TestOK ... ok\n" +
		"test TestMore ... ok\n" +
		"test TestSkipped ... ignored\n"
	if out != want {
		t.Fatalf("verbose happy suite output mismatch\nwant:\n%q\ngot:\n%q", want, out)
	}
}

// TestFrontendSuiteBlockQuietFailureMatchesCargoShape protects quiet failure rendering.
func TestFrontendSuiteBlockQuietFailureMatchesCargoShape(t *testing.T) {
	out := renderEvents(t, false, Event{Kind: EventSuiteResult, SuiteResult: suiteResultFixture()})
	want := suiteLine("Failed", "pkg", "1 failed; 1 passed; 1 ignored", "250ms") + "\n" +
		"test TestBad ... FAILED\n" +
		"\nfailures:\n" +
		"\n" +
		"---- TestBad stdout ----\n" +
		"boom\n" +
		"\n" +
		"failures:\n" +
		"    TestBad\n" +
		"\n"
	if out != want {
		t.Fatalf("quiet suite output mismatch\nwant:\n%q\ngot:\n%q", want, out)
	}
}

// TestFrontendFailurePrintsShuffleSeed protects replay instructions in failures.
func TestFrontendFailurePrintsShuffleSeed(t *testing.T) {
	result := suiteResultFixture()
	result.PackageOutput = []string{"-test.shuffle 12345\n", "FAIL\n"}
	out := renderEvents(t, false, Event{Kind: EventSuiteResult, SuiteResult: result})
	want := suiteLine("Failed", "pkg", "1 failed; 1 passed; 1 ignored", "250ms") + "\n" +
		"test TestBad ... FAILED\n" +
		"-test.shuffle 12345\n" +
		"\nfailures:\n" +
		"\n" +
		"---- TestBad stdout ----\n" +
		"boom\n" +
		"\n" +
		"failures:\n" +
		"    TestBad\n" +
		"\n"
	if out != want {
		t.Fatalf("shuffle failure output mismatch\nwant:\n%q\ngot:\n%q", want, out)
	}
}

// TestFrontendSuiteBlockVerboseFailureMatchesCargoShape protects verbose failure rendering.
func TestFrontendSuiteBlockVerboseFailureMatchesCargoShape(t *testing.T) {
	out := renderEvents(t, true, Event{Kind: EventSuiteResult, SuiteResult: suiteResultFixture()})
	want := suiteLine("Failed", "pkg", "1 failed; 1 passed; 1 ignored", "250ms") + "\n" +
		"test TestOK ... ok\n" +
		"test TestSkipped ... ignored\n" +
		"test TestBad ... FAILED\n" +
		"\nfailures:\n" +
		"\n" +
		"---- TestBad stdout ----\n" +
		"boom\n" +
		"\n" +
		"failures:\n" +
		"    TestBad\n" +
		"\n"
	if out != want {
		t.Fatalf("suite output mismatch\nwant:\n%q\ngot:\n%q", want, out)
	}
}

// TestFrontendPackageFailureSpacingMatchesFailureBlocks protects stall output spacing.
func TestFrontendPackageFailureSpacingMatchesFailureBlocks(t *testing.T) {
	out := renderEvents(t,
		false,
		Event{Kind: EventSuiteResult, SuiteResult: SuiteResult{
			ImportPath: "example.com/m/stalled",
			Failed:     true,
			Err:        "suite produced no output for 5m0s; killed (child process stalled)",
		}},
		Event{Kind: EventSummary, Summary: Summary{
			Suites: 1, Failed: 1, Elapsed: 5 * time.Minute,
		}},
	)
	want := suiteLine("Failed", "stalled", "", "0s") + "\n" +
		"suite produced no output for 5m0s; killed (child process stalled)\n" +
		"Finished 1 suite, 0 tests: 0 passed; 1 failed; 0 ignored in 5m0s\n"
	if out != want {
		t.Fatalf("package failure output mismatch\nwant:\n%q\ngot:\n%q", want, out)
	}
}

// TestFrontendCachedLineMatchesContract protects cached-suite rendering.
func TestFrontendCachedLineMatchesContract(t *testing.T) {
	out := renderEvents(t,
		false,
		Event{Kind: EventCached, Cached: CachedEvent{
			ImportPath: "example.com/m/pkg", Passed: 2, Ignored: 0,
		}},
		Event{Kind: EventSummary, Summary: Summary{
			Suites: 1, Tests: 2, Passed: 2, Cached: 1, Elapsed: 50 * time.Millisecond,
		}},
	)
	want := "Cached pkg\n" +
		"Finished 1 suite, 2 tests: 2 passed; 0 failed; 0 ignored in 50ms, 1 cached\n"
	if out != want {
		t.Fatalf("cached output mismatch\nwant:\n%q\ngot:\n%q", want, out)
	}
}

// TestFrontendRemoteSummaryMatchesContract protects remote summary rendering.
func TestFrontendRemoteSummaryMatchesContract(t *testing.T) {
	out := renderEvents(t,
		false,
		Event{Kind: EventSummary, Summary: Summary{
			Suites: 12, Tests: 300, Passed: 300, Cached: 8, RemoteCached: 3,
			RemoteConfigured: true, RemoteReads: 3, RemoteWrites: 4,
			Elapsed: 4200 * time.Millisecond,
		}},
	)
	want := "Finished 12 suites, 300 tests: 300 passed; 0 failed; 0 ignored in 4.2s, " +
		"8 cached (3 remote) | remote: 3 reads, 4 writes\n"
	if out != want {
		t.Fatalf("remote summary mismatch\nwant:\n%q\ngot:\n%q", want, out)
	}
}

// TestFrontendRemoteSummaryShowsConfiguredZeroes protects explicit zero traffic.
func TestFrontendRemoteSummaryShowsConfiguredZeroes(t *testing.T) {
	out := renderEvents(t,
		false,
		Event{Kind: EventSummary, Summary: Summary{
			Suites: 1, Tests: 1, Passed: 1, RemoteConfigured: true,
			Elapsed: time.Second,
		}},
	)
	want := "Finished 1 suite, 1 test: 1 passed; 0 failed; 0 ignored in 1s" +
		" | remote: 0 reads, 0 writes\n"
	if out != want {
		t.Fatalf("remote zero summary mismatch\nwant:\n%q\ngot:\n%q", want, out)
	}
}

// TestTruncateMiddleProtectsPathBoundaries pins rune-safe path truncation.
func TestTruncateMiddleProtectsPathBoundaries(t *testing.T) {
	tests := []struct {
		path     string
		widthMax int
		want     string
	}{
		{path: "abcdef", widthMax: 0, want: ""},
		{path: "abcdef", widthMax: 1, want: "…"},
		{path: "abcdef", widthMax: 2, want: "…f"},
		{path: "abcdef", widthMax: 3, want: "a…f"},
		{path: "abcdef", widthMax: 4, want: "a…ef"},
		{path: "abcdef", widthMax: 5, want: "a…def"},
		{path: "abcdefg", widthMax: 6, want: "a…defg"},
		{path: "abcdefghij", widthMax: 7, want: "ab…ghij"},
		{path: "åbcdef界", widthMax: 5, want: "å…ef界"},
		{path: "exact", widthMax: 5, want: "exact"},
	}
	for _, test := range tests {
		got := truncateMiddle(test.path, test.widthMax)
		if got != test.want {
			t.Fatalf("truncateMiddle(%q, %d) = %q, want %q",
				test.path, test.widthMax, got, test.want)
		}
		if runeLen(got) > test.widthMax || !utf8.ValidString(got) {
			t.Fatalf("truncateMiddle(%q, %d) broke invariant with %q",
				test.path, test.widthMax, got)
		}
	}
}

// TestFrontendSuiteRowsUseFixedDurationBeforeCounts protects compact row layout.
func TestFrontendSuiteRowsUseFixedDurationBeforeCounts(t *testing.T) {
	frontend := New(new(bytes.Buffer), new(bytes.Buffer), false, nil)
	frontend.ConfigureRun(false, "example.com/m")
	frontend.color = false
	frontend.runPlan(RunPlan{Suites: 3, MaxImportPathWidth: len("pkg/broken")})
	rows := []string{
		frontend.suiteResultRow(SuiteResult{
			ImportPath: "example.com/m/mathx", ElapsedSeconds: 0.96,
			Tests: []TestResult{
				{Name: "TestAdd", Outcome: TestPassed},
				{Name: "TestMul", Outcome: TestPassed},
				{Name: "TestSkipped", Outcome: TestIgnored},
			},
		}),
		frontend.suiteResultRow(SuiteResult{
			ImportPath:     "example.com/m/app",
			ElapsedSeconds: 1.28,
			Tests:          []TestResult{{Name: "TestOK", Outcome: TestPassed}},
		}),
		frontend.suiteResultRow(SuiteResult{
			ImportPath:     "example.com/m/pkg/broken",
			ElapsedSeconds: 0.58,
			Failed:         true,
			Tests:          []TestResult{{Name: "TestBad", Outcome: TestFailed}},
		}),
	}
	out := strings.Join(rows, "\n") + "\n"
	want := "Passed mathx        960ms  2 passed; 1 ignored\n" +
		"Passed app          1.28s  1 passed\n" +
		"Failed pkg/broken   580ms  1 failed\n"
	if out != want {
		t.Fatalf("suite row layout mismatch\nwant:\n%q\ngot:\n%q", want, out)
	}
}

func TestFrontendSuiteRowsKeepDurationAlignedWithLargeCounts(t *testing.T) {
	frontend := New(new(bytes.Buffer), new(bytes.Buffer), false, nil)
	frontend.ConfigureRun(false, "example.com/m")
	frontend.runPlan(RunPlan{Suites: 2, MaxImportPathWidth: len("pkg/clock")})
	short := frontend.alignedSuiteRow("Passed", "strx", "2 passed", "430ms")
	long := frontend.alignedSuiteRow("Failed", "pkg/clock",
		"1 failed; 16369 passed; 2330 ignored", "17.1s")
	if runeIndex(short, "430ms") != runeIndex(long, "17.1s") {
		t.Fatalf("durations drifted:\n%q\n%q", short, long)
	}
	if !strings.HasSuffix(long, "17.1s  1 failed; 16369 passed; 2330 ignored") {
		t.Fatalf("large counts did not trail unpadded: %q", long)
	}
}

func TestFrontendTerminalSuiteRowBudgetPreservesCountsAndDuration(t *testing.T) {
	frontend := New(new(bytes.Buffer), new(bytes.Buffer), true, nil)
	frontend.ConfigureRun(false, "example.com/m")
	frontend.color = false
	frontend.width = 60
	longPath := "internal/" + strings.Repeat("deep/", 10) + "suite"
	frontend.runPlan(RunPlan{Suites: 1, MaxImportPathWidth: runeLen(longPath)})
	counts := "16369 passed; 2330 ignored"
	row := frontend.alignedSuiteRow("Passed", longPath, counts, "17.1s")
	if runeLen(row) > frontend.noWrapWidth() {
		t.Fatalf("terminal row width = %d, want <= %d: %q",
			runeLen(row), frontend.noWrapWidth(), row)
	}
	if !strings.Contains(row, "…") {
		t.Fatalf("terminal row did not truncate path: %q", row)
	}
	if !strings.HasSuffix(row, "17.1s  "+counts) {
		t.Fatalf("terminal row lost duration or counts: %q", row)
	}
}

func TestFrontendNonTerminalSuiteRowsAreNotWidthBudgeted(t *testing.T) {
	frontend := New(new(bytes.Buffer), new(bytes.Buffer), false, nil)
	frontend.ConfigureRun(false, "example.com/m")
	frontend.width = 60
	longPath := "internal/" + strings.Repeat("deep/", 20) + "suite"
	frontend.runPlan(RunPlan{Suites: 1, MaxImportPathWidth: runeLen(longPath)})
	counts := "16369 passed; 2330 ignored"
	row := frontend.alignedSuiteRow("Passed", longPath, counts, "17.1s")
	if runeLen(row) <= frontend.width {
		t.Fatalf("non-terminal row was budgeted to terminal width %d: %q", frontend.width, row)
	}
	if !strings.HasSuffix(row, "17.1s  "+counts) {
		t.Fatalf("non-terminal row lost duration or counts: %q", row)
	}
}

func TestFormatStableDurationUsesTieredText(t *testing.T) {
	tests := []struct {
		duration time.Duration
		want     string
	}{
		{duration: 0, want: "0.0s"},
		{duration: 999 * time.Millisecond, want: "1.0s"},
		{duration: time.Second, want: "1.0s"},
		{duration: 29*time.Second + 900*time.Millisecond, want: "29.9s"},
		{duration: 30 * time.Second, want: "30.0s"},
		{duration: 59*time.Second + 940*time.Millisecond, want: "59.9s"},
		{duration: 59*time.Second + 960*time.Millisecond, want: "59.9s"},
		{duration: time.Minute, want: "1m00s"},
		{duration: time.Minute + 4*time.Second + 900*time.Millisecond, want: "1m04s"},
		{duration: 12*time.Minute + 59*time.Second, want: "12m59s"},
		{duration: time.Hour, want: "1h00m"},
		{duration: time.Hour + 2*time.Minute + 59*time.Second, want: "1h02m"},
	}
	for _, test := range tests {
		if got := formatStableDuration(test.duration); got != test.want {
			t.Fatalf("formatStableDuration(%s) = %q, want %q", test.duration, got, test.want)
		}
	}
}

func TestFormatStableDurationRejectsNegativeDurations(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("formatStableDuration(-1ns) did not panic")
		}
	}()
	_ = formatStableDuration(-time.Nanosecond)
}

func TestFormatRegionRowDurationHidesSubSecondElapsed(t *testing.T) {
	if got := formatRegionRowDuration(999 * time.Millisecond); got != "" {
		t.Fatalf("formatRegionRowDuration(999ms) = %q, want empty", got)
	}
	if got := formatRegionRowDuration(time.Second); got != "1.0s" {
		t.Fatalf("formatRegionRowDuration(1s) = %q, want 1.0s", got)
	}
}

func TestFormatPermanentDurationUsesRoundedGoText(t *testing.T) {
	tests := []struct {
		duration time.Duration
		want     string
	}{
		{duration: 426 * time.Millisecond, want: "430ms"},
		{duration: 2844 * time.Millisecond, want: "2.84s"},
		{duration: time.Minute + 4*time.Second + 986*time.Millisecond, want: "1m4.99s"},
	}
	for _, test := range tests {
		if got := formatPermanentDuration(test.duration); got != test.want {
			t.Fatalf("formatPermanentDuration(%s) = %q, want %q", test.duration, got, test.want)
		}
	}
}

// TestFrontendRunPlanAlignsSuiteRows protects run-wide permanent row layout.
func TestFrontendRunPlanAlignsSuiteRows(t *testing.T) {
	longPath := "example.com/m/" + strings.Repeat("x", 90)
	out := renderEvents(t,
		false,
		Event{Kind: EventRunPlan, RunPlan: RunPlan{
			Suites:             3,
			MaxImportPathWidth: runeLen(ShortenImportPath(longPath, "example.com/m")),
		}},
		Event{Kind: EventSuiteResult, SuiteResult: happySuiteFixture()},
		Event{Kind: EventCached, Cached: CachedEvent{ImportPath: "example.com/m/cached", Passed: 1}},
		Event{Kind: EventSuiteResult, SuiteResult: SuiteResult{
			ImportPath:     longPath,
			ElapsedSeconds: 0.16,
			Tests:          []TestResult{{Name: "TestOK", Outcome: TestPassed}},
		}},
	)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3:\n%q", len(lines), out)
	}
	if runeIndex(lines[0], "250ms") != runeIndex(lines[2], "160ms") {
		t.Fatalf("durations not column-aligned:\n%q\n%q", lines[0], lines[2])
	}
	if runeLen(lines[0]) >= 120 || runeLen(lines[2]) >= 120 {
		t.Fatalf("rows are still terminal-width anchored:\n%q\n%q", lines[0], lines[2])
	}
	if !strings.Contains(lines[2], "…") || runeLen(lines[2]) > 120 {
		t.Fatalf("long path row did not truncate safely: %q", lines[2])
	}
	if lines[1] != "Cached cached" {
		t.Fatalf("cached row = %q, want verb and path only", lines[1])
	}
}

// TestFrontendFailureSummaryUsesShortenedPaths protects rerun-friendly errors.
func TestFrontendFailureSummaryUsesShortenedPaths(t *testing.T) {
	out := new(bytes.Buffer)
	errOut := new(bytes.Buffer)
	events := make(chan Event, 1)
	frontend := New(out, errOut, false, events)
	frontend.ConfigureRun(false, "example.com/m")
	done := runFrontendForTest(frontend)
	sendEvent(t, events, Event{Kind: EventSummary, Summary: Summary{
		Suites:       2,
		FailedSuites: []string{"example.com/m/internal/pkg", "example.com/m"},
		Skipped:      1,
	}})
	close(events)
	if err := <-done; err != nil {
		t.Fatalf("frontend run: %v", err)
	}
	want := "error: test failed in internal/pkg, m (1 suite skipped)\n"
	if errOut.String() != want {
		t.Fatalf("errOut mismatch\nwant:\n%q\ngot:\n%q", want, errOut.String())
	}
}

// TestFrontendSummaryNotesAndInterruption protects non-fatal cache and signal output.
func TestFrontendSummaryNotesAndInterruption(t *testing.T) {
	out := new(bytes.Buffer)
	errOut := new(bytes.Buffer)
	events := make(chan Event, 2)
	frontend := New(out, errOut, false, events)
	frontend.ConfigureRun(false, "example.com/m")
	done := make(chan int, 1)
	go func() {
		code, err := frontend.Run(context.Background())
		if err != nil {
			t.Errorf("frontend run: %v", err)
		}
		done <- code
	}()
	sendEvent(t, events, Event{Kind: EventSummary, Summary: Summary{
		Suites: 3, Skipped: 2, CacheErrors: 1, Interrupted: true,
	}})
	close(events)
	if code := <-done; code != 130 {
		t.Fatalf("code = %d, want 130", code)
	}
	if !strings.Contains(out.String(), "note: 1 cache errors") {
		t.Fatalf("out = %q, want cache error note", out.String())
	}
	if !strings.Contains(errOut.String(), "interrupted; 2 suites skipped") {
		t.Fatalf("errOut = %q, want interrupted summary", errOut.String())
	}
}

// renderEvents returns the non-terminal output for frontend events.
func renderEvents(t *testing.T, verbose bool, emitted ...Event) string {
	t.Helper()
	out := new(bytes.Buffer)
	events := make(chan Event, len(emitted))
	frontend := New(out, new(bytes.Buffer), false, events)
	frontend.ConfigureRun(verbose, "example.com/m")
	done := make(chan error, 1)
	go func() {
		_, err := frontend.Run(context.Background())
		done <- err
	}()
	for _, event := range emitted {
		sendEvent(t, events, event)
	}
	close(events)
	if err := <-done; err != nil {
		t.Fatalf("frontend run: %v", err)
	}
	return out.String()
}

// progressRegionOutput exercises the row path used by synchronized terminal frames.
func progressRegionOutput(frontend *Frontend) string {
	var out bytes.Buffer
	rows := frontend.terminalProgressRows(false)
	frontend.linesDrawn = writeProgressRows(&out, rows)
	return out.String()
}

// sendEvent sends one frontend event without blocking the test forever.
func sendEvent(t *testing.T, events chan<- Event, event Event) {
	t.Helper()
	select {
	case events <- event:
	case <-time.After(time.Second):
		t.Fatalf("send event timed out")
	}
}

// runFrontendForTest starts a frontend and returns its terminal error channel.
func runFrontendForTest(frontend *Frontend) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := frontend.Run(context.Background())
		done <- err
	}()
	return done
}

// sendTick sends one manual frontend tick without blocking forever.
func sendTick(t *testing.T, ticks chan<- time.Time, tick time.Time) {
	t.Helper()
	select {
	case ticks <- tick:
	case <-time.After(time.Second):
		t.Fatalf("send tick timed out")
	}
}

// terminalFrame builds the synchronized frame shape expected by terminal tests.
func terminalFrame(previousRows int, permanent string, rows []string) string {
	var out strings.Builder
	out.WriteString(synchronizedOutputOpen)
	out.WriteString("\r")
	if previousRows > 1 {
		fmt.Fprintf(&out, "\x1b[%dA", previousRows-1)
	}
	out.WriteString(eraseToEndOfScreen)
	out.WriteString(permanent)
	for index, row := range rows {
		if index > 0 {
			out.WriteString("\n")
		}
		out.WriteString(row)
	}
	out.WriteString(synchronizedOutputClose)
	return out.String()
}

// suiteLine returns the expected permanent suite row for golden-byte tests.
func suiteLine(verb string, path string, counts string, duration string) string {
	row := fmt.Sprintf("%-*s %s%*s", suiteVerbWidth, verb, path,
		suiteDurationColumnWidth, duration)
	if counts != "" {
		row += "  " + counts
	}
	return row
}

func runeIndex(text string, substr string) int {
	index := strings.Index(text, substr)
	if index < 0 {
		return index
	}
	return runeLen(text[:index])
}

// assertFrameClearBeforeContent protects mixed permanent/live frames from stale tails.
func assertFrameClearBeforeContent(t *testing.T, frame string, content string) {
	t.Helper()
	clearAt := strings.Index(frame, eraseToEndOfScreen)
	contentAt := strings.Index(frame, content)
	if clearAt < 0 || contentAt < 0 || clearAt > contentAt {
		t.Fatalf("frame writes content before clear\ncontent: %q\nframe:   %q", content, frame)
	}
}

func assertFrameVisualLinesWithinWidth(t *testing.T, frame string, width int) {
	t.Helper()
	for _, line := range strings.Split(visibleFrameText(frame), "\n") {
		if runeLen(line) > width {
			t.Fatalf("visual line width %d > %d:\nline:  %q\nframe: %q", runeLen(line), width, line, frame)
		}
	}
}

func visibleFrameText(frame string) string {
	var out strings.Builder
	for index := 0; index < len(frame); {
		if frame[index] == '\x1b' {
			index = skipANSI(frame, index+1)
			continue
		}
		if frame[index] == '\r' {
			index++
			continue
		}
		out.WriteByte(frame[index])
		index++
	}
	return out.String()
}

func skipANSI(text string, index int) int {
	if index < len(text) && text[index] == '[' {
		index++
		for index < len(text) && (text[index] < '@' || text[index] > '~') {
			index++
		}
	}
	if index < len(text) {
		index++
	}
	return index
}

// recordingWriter records each Write call as a separate frame.
type recordingWriter struct {
	mu     sync.Mutex
	writes []string
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, string(p))
	return len(p), nil
}

func (w *recordingWriter) Writes() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.writes...)
}

func waitForWrites(t *testing.T, out *recordingWriter, count int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if writes := out.Writes(); len(writes) >= count {
			return writes
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d writes in:\n%q", count, out.Writes())
	return nil
}

// happySuiteFixture returns one successful suite for dense rendering tests.
func happySuiteFixture() SuiteResult {
	return SuiteResult{
		ImportPath:     "example.com/m/pkg",
		ElapsedSeconds: 0.25,
		Tests: []TestResult{
			{Name: "TestOK", Outcome: TestPassed},
			{Name: "TestMore", Outcome: TestPassed},
			{Name: "TestSkipped", Outcome: TestIgnored},
		},
	}
}

func suiteWithCounts(importPath string, passed int, failed int, ignored int) SuiteResult {
	tests := make([]TestResult, 0, passed+failed+ignored)
	for i := 0; i < passed; i++ {
		tests = append(tests, TestResult{Name: fmt.Sprintf("TestPassed%d", i), Outcome: TestPassed})
	}
	for i := 0; i < failed; i++ {
		tests = append(tests, TestResult{Name: fmt.Sprintf("TestFailed%d", i), Outcome: TestFailed})
	}
	for i := 0; i < ignored; i++ {
		tests = append(tests, TestResult{Name: fmt.Sprintf("TestIgnored%d", i), Outcome: TestIgnored})
	}
	return SuiteResult{ImportPath: importPath, Tests: tests, ElapsedSeconds: 17.1, Failed: failed > 0}
}

// suiteResultFixture returns one mixed-result suite for rendering tests.
func suiteResultFixture() SuiteResult {
	return SuiteResult{
		ImportPath:     "example.com/m/pkg",
		ElapsedSeconds: 0.25,
		Failed:         true,
		PackageOutput:  nil,
		Tests: []TestResult{
			{Name: "TestOK", Outcome: TestPassed},
			{Name: "TestSkipped", Outcome: TestIgnored},
			{Name: "TestBad", Outcome: TestFailed, Output: []string{"boom\n"}},
		},
	}
}
