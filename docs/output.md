# Output

rask keeps the happy path dense. Quiet output prints one merged row per executed suite, one row per cached suite, and a final summary. Failure output is deliberately louder: quiet mode still prints every failed test line, retained output for each failed test, and the final error on stderr.

The examples below match the frontend's non-terminal rendering. Terminal runs may add color and a sticky progress region.

## Quiet runs

```text
Passed pkg/strx   120ms  2 passed
Passed mathx      250ms  2 passed; 1 ignored
Finished 2 suites, 5 tests: 4 passed; 0 failed; 1 ignored in 430ms
```

The permanent row layout is fixed before results stream. It uses a six-character verb, a shortened import path column, an eight-character duration column, then unpadded counts. Counts omit zero segments and render failed, passed, then ignored.

Warm cache hits collapse to the suite path and the summary. Cached rows do not print counts, but cached counts still feed the final summary.

```text
Cached mathx
Cached pkg/strx
Finished 2 suites, 5 tests: 4 passed; 0 failed; 1 ignored in 90ms, 2 cached
```

When the remote cache is configured, the cached count is qualified with the
number served by the remote tier, and the summary ends with successful remote
read and write counts. Without a configured remote tier, this counter segment is
omitted.

```text
Finished 12 suites, 174 tests: 172 passed; 0 failed; 2 ignored in 320ms, 12 cached (12 remote) | remote: 12 reads, 0 writes
```

## Verbose mode

Pass `-v` or `--verbose` to print passing and ignored per-test lines below each executed suite row.

```text
Passed pkg/strx   120ms  2 passed
test TestUpper ... ok
test TestBroken ... ok
Passed mathx      250ms  2 passed; 1 ignored
test TestAdd ... ok
test TestMul ... ok
test TestSkipped ... ignored
Finished 2 suites, 5 tests: 4 passed; 0 failed; 1 ignored in 430ms
```

Verbose mode does not change cache behavior. Cached suites still render as `Cached <path>`.

## Failures

Failures get the space they need. Quiet mode hides passing and ignored test lines, but never hides failed test lines or captured failure output.

```text
Failed pkg/mathx    80ms  1 failed
test TestAdd ... FAILED

failures:

---- TestAdd stdout ----
    mathx_test.go:7: expected 3

failures:
    TestAdd

error: test failed in pkg/mathx
```

Package-level failures, including compile errors and stall watchdog failures, print package diagnostics when there is no individual failed test. The runner keeps up to 4096 diagnostic lines per test and per package. If more output arrives, rask reports how many lines it dropped.

## Progress region

On a terminal, rask paints a sticky progress region at the bottom. Finished suite blocks stream above it. On a non-terminal, such as CI or a pipe, progress events do not print.

```text
Testing 12/74 suites | 41 tests: 40 passed; 0 failed; 1 ignored | 8.3s
    svc/api/integration        (12/45) 14.2s
    pkg/cache                   (3/9)   4.1s
    pkg/uid                     (0/?)
```

The header shows completed suites, total selected suites, folded test counts, and elapsed time. Running rows are sorted longest-running first. Each row shows per-suite progress as `(completed/total)` when the preflight `go test -list '^Test'` succeeds, or `(completed/?)` when that cosmetic listing fails. The region is capped at 32 rows including the overflow row.

`NO_COLOR=1` disables colors and the sticky progress region. See [CLI reference](cli.md) for the full environment table and [Architecture](architecture.md) for the event stream design.
