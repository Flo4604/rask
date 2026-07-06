# rask

A fast, zero-dependency Go test runner with content-addressed result caching. *rask* is Norwegian for fast.

Instead of wrapping `go test ./...`, rask discovers every package with
`go list`, hashes each package's sources together with its discovered local
dependency graph, runs suites in parallel with fail-fast, and skips any suite
whose exact content already passed.

Default output is dense — one merged result row per suite; a warm rerun
collapses to cache hits and the summary. The result rows below are shortened
for display; actual output pads paths to the selected shortened-path column,
then uses a fixed duration column before trailing, unpadded counts.

```text
Passed pkg/strx   120ms  2 passed
Passed mathx      250ms  2 passed; 1 ignored
Finished 2 suites, 5 tests: 4 passed; 0 failed; 1 ignored in 430ms
```

```text
Cached mathx
Cached pkg/strx
Finished 2 suites, 5 tests: 4 passed; 0 failed; 1 ignored in 90ms, 2 cached
```

Pass `-v` or `--verbose` for per-test result lines:

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

Failures are the exception to the density: they render in full, with each
failing test's captured output, and always — quiet mode never hides a failure:

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

While suites run, a sticky progress region at the bottom shows live aggregate
progress and the currently executing suites, longest-running first; finished
suite blocks stream above it. On a non-terminal (CI, pipes) output degrades to
plain sequential blocks.

```text
Testing 12/74 suites | 41 tests: 40 passed; 0 failed; 1 ignored | 8.3s
    svc/api/integration                   (12/45) 14.2s
    internal/generated/r…/ratelimit/store  (7/12)  2.6s
    pkg/uid                                (0/?)
    ... and 5 more
```

## Install

```sh
go install github.com/chronark/rask@latest
```

## Usage

```sh
rask              # test the whole module, skipping unchanged suites
rask ./pkg/...    # test only the matched packages
rask -v ./pkg/... # also print passing and ignored per-test lines
rask --no-cache   # run selected suites even on cache hits, then refresh passes
rask clean        # drop this module's cached results
rask clean --all  # drop all rask-owned cache generations
```

Testing is the default action, and rask takes only package patterns plus
`-v`/`--verbose` and `--no-cache` anywhere among them. It always runs whole
suites and always fails fast. Cached results only make sense for complete suite
runs, so filtered or customized runs (`-run`, bench, fuzz, coverage) belong to
plain `go test`.

## Fail fast

Failures stop the run as early as possible by default:

- within a suite, the first failing test stops that suite
  (`go test -failfast`)
- across suites, the first failure kills in-flight suites and skips queued
  ones — silently, so the failure stays at the bottom of the output; the
  final error line carries the skip count
- per suite, five minutes without child output kills that child and reports a
  stall failure, covering compile-phase and lock-wait hangs outside
  `go test -timeout`

## How caching works

Results live in a content-addressable object store, sharded by module:

```
~/Library/Caches/rask/
  CACHEDIR.TAG                  # backup tools skip the store
  v1/                           # format version; v2 never misreads v1
    <module-key>/               # sha256(modulePath)[:8] — checkout-independent
      <hh>/<digest>.json        # one passing suite, hh = digest[:2]
```

- A suite's digest covers its own `.go`/test/embed file contents plus,
  recursively, the digests of every discovered local package it imports
  (including test-only imports). That includes packages from active `go.work`
  modules and local filesystem `replace` targets, so editing a local
  dependency invalidates it *and* all selected suites that depend on it, and
  nothing else.
- Full `go env` output, active `go.work` content, and each discovered local
  module's `go.mod` and `go.sum` are mixed into the digest, so toolchain,
  flag, experiment, race, cgo, workspace membership, replacement, and
  dependency changes never hit stale entries. This intentionally
  over-invalidates because extra reruns are safer than stale greens.
- Keys are content, not import paths: switching branches back and forth never
  evicts anything, and identical checkouts (git worktrees) share results.
- Only **passing** suites are stored; failures always rerun. Tests run with
  `-count=1`, so rask owns caching end to end.
- `--no-cache` bypasses cache reads for the selected suites, but passing
  results are still written back to refresh later normal runs.
- External test packages may legally import a package that imports the
  package under test back; rask models this the way the compiler does (build
  digests ordered by build imports only, suite digests as sinks), so such
  cycles neither panic nor over-invalidate.
- The store is doubly bounded: objects unused for 30 days are pruned, and
  each module keeps at most 4096 objects (LRU). Hits refresh an object's
  timestamp.

## How rask relates to other tools

The idea — hash the inputs, skip what already passed — is not new. What
differs between tools is how much of your build they demand to own in
exchange for it.

| | `go test` | rask | gotestsum | Bazel / Buck2 / Pants |
| --- | --- | --- | --- | --- |
| Config required | none | none | none | BUILD files for every package |
| Test result caching | per checkout | content-addressed | none | content-addressed |
| Shared across worktrees / branches | no | yes | — | yes |
| Remote / CI cache | no | not yet (backend seam exists) | — | yes |
| Hermetic execution | no | no | no | sandboxed |
| Scope | everything (bench, fuzz, cover, filters) | whole-suite runs only | output formatting | multi-language builds + tests |

**`go test`** already caches test results, but the cache is easy to bust
(flag changes, environment access) and is keyed per checkout — two worktrees
of the same commit each pay full price, and flipping between branches
invalidates back and forth. rask's digests depend only on content, so
identical code hits the cache no matter where or when it is checked out.
Everything beyond whole-suite runs — `-run` filters, benchmarks, fuzzing,
coverage — deliberately stays with plain `go test`; rask does less on purpose.

**gotestsum** improves `go test` output, but runs everything every time. rask
overlaps with it on presentation and adds the memory.

**Bazel (rules_go), Buck2, and Pants** implement the same content-addressed
skipping more rigorously than rask ever will: declared inputs, sandboxed
hermetic actions, remote caching and remote execution across languages. The
price is owning the build — BUILD files, toolchain wiring, and keeping the
graph in sync. rask is the other end of that trade: `go list` already knows
the dependency graph, so the adoption cost is one binary and zero
configuration files.

Honest limitations of the lightweight trade: rask is not hermetic. Package
sources, embeds, `testdata/`, dependency metadata, and the full Go environment
salt the digest, but undeclared inputs outside the package tree — network,
databases, clocks, system state, or files reached by custom paths — remain
outside the cache key. Bazel's sandbox catches exactly this class; rask keeps
Go's normal execution model and reruns only when its observed inputs change.

## Configuration

| Variable            | Meaning                                | Default                       |
| ------------------- | -------------------------------------- | ----------------------------- |
| `RASK_CACHE_BACKEND` | Result store backend                   | `disk`                        |
| `RASK_CACHE_DIR`     | Root directory of the disk backend     | user cache dir + `/rask`       |
| `NO_COLOR`          | Disable colors and the progress region | unset                         |

Storage is currently a local disk object store rooted at `RASK_CACHE_DIR` or
the user cache dir plus `/rask`.

## Architecture

A run is a straight line down, two questions, and a memory. The first
question reads the memory; the second one writes it. Internally, `internal/cmd`
wires the services with five bounded Go channels: discovered packages,
fingerprinted suites, stale runner tasks, runner results, and frontend events.
Each channel has one sender and that sender owns closing it.

```
                            rask
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
