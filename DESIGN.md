# rask Design

`rask` is a small Go test runner with content-addressed suite caching.
It intentionally mirrors what `go list` and `go test` already do while wiring
the run as channel-connected services with uniform lifecycles.

## Commands

`rask [-v|--verbose] [--no-cache] [patterns]` runs test suites. With no
patterns it uses `./...`. `rask clean` removes the current module's cache shard.
`rask clean --all` removes all store contents. The test command takes package
patterns plus `-v`/`--verbose` and `--no-cache` anywhere among them.
`--no-cache` bypasses cache reads while preserving passing write-back. Other
flags are rejected with a pointer to plain `go test`.

## Execution

The `internal/cmd` test command resolves the current module, opens the cache,
creates five bounded channels, and starts five services:

1. `discover.Discoverer.Run(ctx)` lists packages and closes the package channel.
2. `fingerprint.Fingerprinter.Run(ctx)` consumes packages, fingerprints suites,
   and closes the suite channel.
3. `run.Runner.Run(ctx)` consumes stale suite tasks, runs a bounded worker
   pool, and closes the result channel.
4. `cache.Cache.Run(ctx)` waits for lifecycle cancellation and prunes once.
5. `frontend.Frontend.Run(ctx)` consumes render events and owns stdout/stderr.

`internal/cmd` owns orchestration. It drains all fingerprints first, sends the
frontend a run plan with the suite total and widest selected import path, then
checks the cache, forwards cached events, sends stale tasks, consumes runner
results, writes passing cache entries, deletes failing entries, cancels on the
first failure, sends the final summary, closes the channels it produces, then
waits before reading service errors.

## Channels

`internal/cmd` owns the run topology directly with one typed Go channel per
message kind: packages, suites, runner tasks, runner results, and frontend
events. No fan-out exists. Every channel has exactly one sender and one
logical receiver. Runner workers are the only competing consumers: they share
the single task channel so stale suites are divided across the worker pool.

Channel capacities are named constants in `internal/cmd`. Package, suite, task,
and event channels are bounded buffers. Runner results remain a zero-capacity
rendezvous channel so a worker cannot start queued work before orchestration
sees a failure.

Close ownership follows the single-sender rule. Discoverer closes packages,
Fingerprinter closes suites, orchestration closes tasks after suite planning
and dispatch, Runner closes results after workers exit, and orchestration
closes events after the final summary. Sends always select on the owning
context's Done channel. Close is therefore coordinated by construction rather
than hidden behind a shared primitive.

## Seam rules

1. Every channel has exactly one sender, and that sender closes it with defer
   when its `Run` method returns, except orchestration-owned task and event
   channels which are closed at their lifecycle boundary.
2. Every send selects on context cancellation as its escape path.
3. Service errors are written to per-service slots and read only after the
   `WaitGroup` completes; fail-fast `context.Canceled` is clean shutdown.
4. The first failed suite cancels discovery, fingerprinting, and running.
   In-flight suites are killed or dropped and counted once as skipped.
5. Channel capacities are named constants in `internal/cmd`, not inline
   literals.

## Layout

| Package | Responsibility |
| --- | --- |
| `main.go` | Tiny executable entrypoint that passes process arguments to `internal/cmd`. |
| `internal/cmd` | CLI dispatch, subcommands, channel setup, orchestration loop, cache policy, exit codes. |
| `internal/gotool` | Go tool boundary, system implementation, `go list` data shapes. |
| `internal/discover` | Package listing service, pattern selection, load-error checks. |
| `internal/fingerprint` | Package stream consumer, build graph ordering, suite digest computation. |
| `internal/cache` | Method-based cache store and lifecycle prune hook. |
| `internal/run` | Suite-task service, worker pool, `go test -json` stream folding. |
| `internal/frontend` | Event consumer, suite blocks, sticky TTY progress region, final summary. |
| `internal/assert` | Always-on assertions for programmer errors. |

## Discovery

Discovery always needs the full local package closure so dependency hashes are
correct. When patterns are exactly `./...`, one main-module `go list -json`
call is enough for selected packages. For narrower patterns, rask first lists
the selected packages and then lists `./...`; the pattern result only decides
which suites run.

When `GOWORK` names an active workspace, discovery also reads every module
reported by `go list -m -json`. It also line-scans local filesystem `replace`
directives in each known root's `go.mod` and adds targets beginning with
`./`, `../`, or `/`. The parser intentionally handles ordinary single-line
and block-form directives, not the whole `go.mod` grammar. Each additional
root is listed with `go list ./...` from that root. Those packages are never
selected as suites; they exist only as fingerprint inputs. Missing roots and
additional-root listing errors fail the run.

Package `Error` and `Incomplete` fields fail the run even when `go list`
exits zero. Syntax parsing is an extra guard so broken local files do not
become green runs.

## Fingerprints

The salt is:

- cache format version
- full `go env` output
- active `go.work` file content
- every discovered local module root path, `go.mod`, and `go.sum`

Build digests are ordered with iterative Kahn sorting over `Imports` only.
Each build digest covers the salt, import path, non-test files, embed files,
and build digests of discovered build imports.

Suite digests are terminal values. Each suite digest covers the salt, import
path, test files, test embed files, `testdata`, its own build digest, and build
digests of discovered test-only imports. `TestImports` and `XTestImports` are
never graph edges, which keeps external-test import cycles finite. Membership
in the local dependency graph is discovery closure membership, not import-path
prefix matching against the main module path.

## Cache

The disk layout is:

```text
<root>/v1/<sha256(modulePath)[:16]>/<digest[:2]>/<digest>.json
```

`RASK_CACHE_DIR` overrides the root. Otherwise the root is the user cache dir
plus `/rask`. The store writes `CACHEDIR.TAG`.

Only passing suites are stored. Failures delete the suite's digest. Reads are
self-healing: oversized, corrupt, or invalid objects are deleted and treated
as misses. Read errors are misses. Writes use write-then-rename and verify by
reading back; a missing read-back is an ordinary cache error.

Each module shard is pruned by age and count: 30 days and 4096 objects, using
mtime as LRU. Cache hits touch the object. The cache does not use channels for
lookups; `Cache.Run(ctx)` exists only to align lifecycle handling and performs
one final prune when its context is cancelled.

## Runner

The worker count is `min(GOMAXPROCS-1, 32)`, with a minimum of one. Each stale
suite runs:

```sh
go test -list '^Test' <package>
go test -json -count=1 -failfast <package>
```

The list command supplies a cosmetic per-suite total for the sticky progress
region. It uses the same process-group cancellation and wait-delay rules as
the streaming test process, with the suite stall watchdog as its timeout; list
failures degrade to an unknown total and never fail the suite. The JSON stream
is folded into test outcomes, live completed-test progress, and bounded output.
A test still in `run` when the suite completes or crashes is failed. Package
output and build output are kept for diagnostics. Scanner lines are capped at
4 MiB, and the remaining stdout is always drained before `Wait` so a stopped
scanner cannot deadlock the child.
Each suite also has a five-minute no-output watchdog that kills a stalled child
process and reports the suite as failed with a stall diagnostic.

On the first failed suite, orchestration cancels the run context. In-flight
test processes are killed by `exec.CommandContext`; queued or unsent stale
suites are counted as skipped and not printed.

## Output

Non-TTY output is plain suite blocks. TTY output also has a sticky bottom
progress region refreshed by a 100 ms ticker inside `Frontend.Run`. The first
line is aggregate progress; the rows below list in-flight suites sorted
longest-running first, capped at 32 rows including the overflow row. Running
rows show `(completed/total)` between the shortened import path and elapsed
time; failed list preflight renders `(completed/?)`. Terminal render events are
batched until the next tick, then flushed as one DEC synchronized-output frame:
cursor movement to the region start, any permanent suite/cache output from the
batch, and the freshly painted region rows with per-row clear-to-end. Completion
events and channel close flush immediately, so exit does not wait for the next
tick. `NO_COLOR` disables colors and the sticky region. Region lines are
rune-safe truncated and intentionally leave one terminal cell unused so they
never wrap; row layout computes progress and elapsed columns per frame, then
truncates the path first. Path truncation uses a middle ellipsis, keeping about
one third of the remaining budget from the head and two thirds from the tail
because deep generated package paths usually differ late in the string. Wrapped
rows would corrupt the cursor-up erase arithmetic.
Permanent suite rows use six-character outcome verbs: bold green `Passed`,
bold red `Failed`, and dim `Cached`. Executed suites merge the old status and
`test result:` lines into one row:

```text
Passed mathx        960ms  2 passed; 1 ignored
Passed app          1.28s  1 passed
Failed pkg/broken   580ms  1 failed
```

Counts omit zero segments and render failed, passed, then ignored. Permanent
durations round to 10 ms and use Go's standard `time.Duration` text (`430ms`,
`2.84s`, `1m4.99s`) because completed lines render once and can afford
precision. Live progress durations favor stable display width: the header uses
one fixed decimal below a minute from `0.0s` upward, minutes as zero-padded
`XmYYs`, and hours as zero-padded `XhYYm`; running suite rows leave the elapsed
field empty below one second, then use the same stable tiers from `1.0s`.
Cached suites render only the dim verb and import path, while their cached
counts still flow into the final summary. Displayed import paths strip the
current module prefix; the module root package renders as the base name of the
module path.

The run plan fixes the permanent layout before any suite result prints:
six-character verb, one space, import path padded to the smaller of the widest
shortened selected path and 70 runes, then duration right-aligned in an 8-rune
field, two spaces, and unpadded counts. Rows are as wide as their content, not
the terminal width. Long shortened paths truncate rune-safely with a middle
ellipsis. Counts trail after all fixed-width columns, so large count strings
cannot shift durations on individual rows.

Suite blocks render all failing per-test lines. With `-v`/`--verbose`, all
passed and ignored per-test lines render below the merged row too. Successful
suite rows, cached rows, and the final summary are dense and contiguous;
failure details keep their internal blank-line spacing. The frontend has no
mutex because one goroutine owns all rendering state.
