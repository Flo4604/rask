<img width="1672" height="941" alt="image" src="https://github.com/user-attachments/assets/1b34b6ef-b0a4-42f7-a009-6637e1d4453c" />


# rask

rask is a Go test runner that reuses passing results, on your machine or across machines through an S3-compatible cache. It skips unchanged packages that already passed, runs the rest in parallel, and stops on the first failure. Use your existing Go tests. No test rewrites required.

Run tests during development, in CI, or through a coding agent without repeating work that a matching environment has already completed.

**Keep the cache between runs to get the speedup.** Local caching needs no configuration. Use an S3-compatible remote cache to share results across developer machines, CI runners, and agent environments. Without a shared or persisted cache, each fresh environment starts cold.

## How it works

For each selected package, rask hashes its source, tests, embedded files, `testdata`, and local dependencies, plus module metadata and the Go environment. That fingerprint identifies a cached result.

rask checks the local cache first, then the S3-compatible remote cache if configured. Remote hits populate the local cache. Passing test runs write to both configured tiers.

```text
rask ./...
    |
    v
Discover packages --> Fingerprint each test suite
                              |
                    Passed with this fingerprint?
                         /           \
                       yes            no
                        |              |
                        v              v
                  Report Cached    Run go test
                                   in parallel
                                        |
                              +---------+---------+
                              |                   |
                            pass                 fail
                              |                   |
                       Cache result       Show failure output
                                          and stop the run
```

Each suite is one package's tests. Cache misses run `go test -json -count=1 -failfast -shuffle=on`. Only passing results are cached. Changing a local dependency invalidates selected suites that depend on it. Passing test details stay hidden unless you use `-v`.

## Set up with your agent

Paste this into your coding agent:

```text
Set up rask in this Go repository. Read and follow:
https://github.com/chronark/rask/blob/main/README.md

Install rask using this repository's existing tool management conventions.
Make it available in the shell that runs tests. Add a test task using rask
for whole-package test runs, and document that task in the repository's
agent instructions (AGENTS.md or equivalent). Preserve existing workflows
that use custom go test flags, benchmarks, fuzzing, or coverage.

Cache setup is part of this task, not an optional follow-up. Determine whether
tests run on one persistent machine or should share results across developer
machines, CI runners, and agent environments.
Ask me if you cannot determine this from the repository.

For one persistent machine, use the local cache. To share results across
machines, configure an S3-compatible remote cache. Fresh environments need
remote caching or an existing mechanism that persists the local cache. Follow:
https://github.com/chronark/rask/blob/main/docs/remote-cache.md

Reuse existing bucket configuration where available. Otherwise, ask me to
choose a bucket or approve creating one. Guide me through adding credentials
to the environment's secret store; do not ask me to paste secrets into chat
or commit them. Wire the cache configuration into each environment that runs
tests. Keep checkout paths and Go environments consistent across
environments that share results.

Check for tests that depend on databases, network services, or other external
state. Keep those on go test or use rask --no-cache.

Run rask twice on a self-contained package with tests. Confirm that the first
run passes and the unchanged second run reports Cached. For remote caching,
use an empty local cache directory and confirm remote hits in the summary.
For reuse across machines, verify a hit in a second environment
when available. A warm local cache alone does not prove that setup works.

Document the chosen cache setup and report what you verified. If credentials,
cache persistence, or cross-environment verification are missing, report that
part of setup as incomplete rather than silently falling back to local-only.
```

### Installation

Supported platforms: macOS and Linux, on amd64 and arm64. Go must be on `PATH` to run tests.

With Go 1.27.1 or newer:

```bash
go install github.com/chronark/rask@latest
```

The binary goes into `GOBIN`, or `$(go env GOPATH)/bin` if `GOBIN` is unset. Add that directory to `PATH`, including in the agent's shell environment.

Alternatively, download the archive for your OS and architecture from [GitHub Releases](https://github.com/chronark/rask/releases), verify it against `checksums.txt`, and extract the `rask` binary into a directory on `PATH`. Pin a release version in shared tooling and CI for repeatable installs.

### Choose where the cache lives

- **One persistent machine:** no setup needed. rask stores results in the user cache directory. Set `RASK_CACHE_DIR` to use another location.
- **Across machines:** configure an S3-compatible bucket, such as AWS S3 or Cloudflare R2, using the [remote cache setup guide](docs/remote-cache.md). Supply the bucket, endpoint, region, and credentials through `~/.rask.json` or `RASK_REMOTE_CACHE_*` environment variables. Keep credentials in your environment's secret store, not the repository. Local caching stays on.

**A shared bucket alone does not guarantee cache hits.** Cache keys include local module root paths and the Go environment. Environments must produce the same fingerprint to reuse results. Keep checkout paths and Go environments consistent, and use `rask why` to inspect misses.

Only grant bucket write access to trusted users and jobs: a cache writer can create passing verdicts that other machines accept without running tests.

### Verify and use

From your Go module root:

```bash
rask --version
rask ./...          # Run all packages; cache passing results.
rask ./...          # Unchanged passing suites report Cached.
```

For remote caching, repeat the run with `RASK_CACHE_DIR` set to an empty directory. Confirm that the summary reports remote hits. A second run with a warm local cache only proves local reuse.

For focused work, pass a package pattern such as `rask ./pkg/...`.

Keep plain `go test` for `-run`, benchmarks, fuzzing, coverage, and other custom flags. rask runs whole suites and does not pass through arbitrary test flags.

**The cache does not track external state.** Database contents, network responses, clocks, and files outside the hashed inputs can change without invalidating a result. Use `rask --no-cache` or plain `go test` for those runs.

```bash
rask --no-cache     # Run tests even when a cached pass exists.
rask -v            # Include passing and skipped test details.
rask why ./pkg/foo  # Inspect a package's cache key and inputs.
```

See the [CLI reference](docs/cli.md) for all commands and the [caching guide](docs/caching.md) for cache inputs and limits.
