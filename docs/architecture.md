# Architecture

rask is a small Go test runner with content-addressed suite caching. It mirrors what `go list` and `go test` already do, then wires discovery, fingerprinting, running, caching, and rendering as channel-connected services with explicit lifetimes.

A run is a straight line down, two cache questions, and a frontend at the end.

```
                            rask
                             │
                       load config once       RASK_CONFIG, cache dir,
                             │                 remote-cache env
                             ▼
                         open cache           disk dir + remote config
                             │
                         discover              every package in the module
                             │
                        fingerprint            sources + dependency closure +
                             │                 toolchain → one digest per suite
                             │
                   ╭── seen it pass? ──╮       the cache remembers
               yes │                   │ no
                   ▼                   ▼
                Cached            run go test      
               (skipped)               │           
                   │                   │
                   │         ╭── did it pass? ──╮
                   │     yes │                  │ no
                   │         ▼                  ▼
                   │  write to cache        fail fast          
                   │         │                  │              
                   │         │                  │
                   ╰─────────┴────────┬─────────╯
                                      ▼
                                  frontend         print progress
                                      │
                                      ▼
                                 exit 0 · 1
```

## Services

`cmd/test` owns orchestration. At the command edge it loads process configuration once, resolves the current module, opens the cache with an explicit disk directory and remote tier, creates five bounded channels, and starts five services:

1. `discover.Discoverer.Run(ctx)` lists packages and closes the package channel.
2. `fingerprint.Fingerprinter.Run(ctx)` consumes packages, fingerprints suites, and closes the suite channel.
3. `run.Runner.Run(ctx)` consumes stale suite tasks, runs a bounded worker pool, and closes the result channel.
4. `cache.Cache.Run(ctx)` waits for lifecycle cancellation and prunes once.
5. `frontend.Frontend.Run(ctx)` consumes render events and owns stdout and stderr.

Orchestration drains all fingerprints first so row layout is stable. It sends the frontend a run plan, checks cache hits, forwards cached events, sends stale tasks to the runner, consumes runner results, writes passing cache entries, deletes failing entries, cancels on the first failure, sends the final summary, closes the channels it produces, and then reads service errors after the `WaitGroup` completes.

## Channels and seam rules

There is one typed Go channel per message kind: packages, suites, runner tasks, runner results, and frontend events. Every channel has exactly one sender and one logical receiver. Runner workers are the only competing consumers; they share the single task channel so stale suites are divided across the worker pool.

Channel capacities are named constants in `cmd/test` and `internal/setup`. Package, suite, task, and event channels are bounded. Runner results are unbuffered so a worker cannot start queued work before orchestration sees a failure.

Close ownership follows the single-sender rule. Discoverer closes packages, Fingerprinter closes suites, orchestration closes tasks after planning and dispatch, Runner closes results after workers exit, and orchestration closes events after the final summary. Sends select on the owning context's `Done` channel.

The seam rules are:

1. Every channel has one sender, and that sender closes it when its `Run` method returns, except orchestration-owned task and event channels.
2. Every send selects on context cancellation.
3. Service errors are written to per-service slots and read only after the `WaitGroup` completes.
4. Fail-fast `context.Canceled` is clean shutdown after the first failed suite.
5. In-flight suites are killed or dropped and counted once as skipped.
6. Channel capacities are named constants, not inline literals.

## Packages

| Package | Responsibility |
| --- | --- |
| `main.go` | Executable entrypoint that assembles the root CLI command. |
| `cmd/test` | Root test action, channel setup, orchestration loop, cache policy, exit codes. |
| `cmd/clean` | Cache-cleaning command. |
| `internal/cli` | Trimmed command parser, boolean flags, generated help. |
| `internal/config` | Process-edge config loader for `RASK_CONFIG`, `RASK_CACHE_DIR`, remote-cache env overlays, and `~/.rask.json`. |
| `internal/setup` | Process-bound command dependencies, signal context, frontend setup. |
| `internal/gotool` | Go tool boundary, system implementation, `go list` data shapes. |
| `internal/discover` | Package listing service, pattern selection, load-error checks. |
| `internal/fingerprint` | Package stream consumer, build graph ordering, suite digest computation. |
| `internal/cache` | Disk cache store, optional remote tier wiring, entry validation, prune policy. |
| `internal/run` | Suite-task service, worker pool, `go test -json` stream folding. |
| `internal/s3` | Minimal stdlib-only SigV4 S3 client for cache `Get`, `Put`, and `Delete`; not a general SDK. |
| `internal/frontend` | Event consumer, suite blocks, sticky TTY progress region, final summary. |
| `internal/assert` | Always-on assertions for programmer errors. |

## Discovery

Discovery needs the full local package closure so dependency hashes are correct. When patterns are exactly `./...`, one main-module `go list -json` call is enough. For narrower patterns, rask first lists the selected packages, then lists `./...` from the main module so fingerprinting still sees the closure.

When `GOWORK` names an active workspace, discovery reads every module reported by `go list -m -json`. It also line-scans local filesystem `replace` directives in each known root's `go.mod`, follows those roots transitively, and lists each additional root with `go list ./...`. Additional roots feed fingerprints only; they are not selected as suites.

Package `Error` and `Incomplete` fields fail the run even when `go list` exits zero. Syntax parsing adds a second guard so broken local files do not become green runs.

## Fingerprints

Fingerprinting uses two phases. Build digests are ordered with iterative Kahn sorting over `Imports` only. Each build digest covers the salt, import path, non-test files, embed files, and build digests of discovered build imports.

Suite digests are terminal values. Each suite digest covers the salt, import path, test files, test embed files, `testdata`, its own build digest, and build digests of discovered test-only imports. `TestImports` and `XTestImports` are never graph edges, which keeps external-test import cycles finite. Membership in the local dependency graph is discovery closure membership, not import-path prefix matching against the main module path.

The salt and retention details are in [Caching](caching.md).

## Runner

The worker count is `min(GOMAXPROCS-1, 32)`, with a minimum of one. Each stale suite runs:

```sh
go test -list '^Test' <package>
go test -json -count=1 -failfast -shuffle=on <package>
```

The list command supplies the cosmetic per-suite total for the sticky progress region. It uses the same process-group cancellation and wait-delay rules as the streaming test process, with the suite stall watchdog as its timeout. List failures degrade to an unknown total and never fail the suite.

The JSON stream is folded into test outcomes, live completed-test progress, and bounded output. A test still in `run` when the suite completes or crashes is failed. Package output and build output are kept for diagnostics. Scanner lines are capped at 4 MiB, and the remaining stdout is always drained before `Wait` so a stopped scanner cannot deadlock the child.

Each suite also has a five-minute no-output watchdog. It kills a child process that stalls before `go test -timeout` can apply, including compile-phase and lock-wait hangs.

On the first failed suite, orchestration cancels the run context. In-flight test processes are killed by `exec.CommandContext`; queued or unsent stale suites are counted as skipped and not printed.

## Testing tiers

The test suite has three tiers. Hermetic unit and golden tests cover pure package behavior and rendering without touching subprocesses or the user's machine state. Seam tests use fakes at package boundaries such as the Go tool, runner, cache, and frontend services to exercise orchestration behavior that would be slow or brittle through a real process tree.

The `e2e` package is the outer tier. It runs as part of ordinary `go test ./...`. Those tests build the real `rask` binary once, scaffold temporary Go modules, run real subprocesses through the real Go toolchain, and isolate every run with a temporary `RASK_CACHE_DIR`.

Output rendering details live in [Output](output.md). Commands and flags live in [CLI reference](cli.md).
