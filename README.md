# rask

A fast, zero-dependency Go test runner with content-addressed result caching. *rask* is Norwegian for fast. It discovers Go packages, fingerprints each suite's local dependency closure, runs stale suites in parallel, and skips any unchanged suite that already passed.

## Install

```sh
go install github.com/chronark/rask@latest
```

```sh
VERSION=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/chronark/rask/releases/latest | sed 's#.*/##')
curl -LO https://github.com/chronark/rask/releases/download/${VERSION}/rask_${VERSION}_darwin_amd64.tar.gz
curl -LO https://github.com/chronark/rask/releases/download/${VERSION}/rask_${VERSION}_darwin_arm64.tar.gz
curl -LO https://github.com/chronark/rask/releases/download/${VERSION}/rask_${VERSION}_linux_amd64.tar.gz
curl -LO https://github.com/chronark/rask/releases/download/${VERSION}/rask_${VERSION}_linux_arm64.tar.gz
tar -xzf rask_${VERSION}_$(go env GOOS)_$(go env GOARCH).tar.gz
install -m 0755 rask /usr/local/bin/rask
```

## Quickstart

```sh
cd path/to/any/go/module
rask
```

The first run tests selected suites and stores passing results by content digest.

```sh
rask
```

Run it again and unchanged suites come back as `Cached`.

```sh
$EDITOR pkg/foo/foo.go && rask
```

Only suites whose source or local dependency closure changed rerun.

```sh
rask -v
```

Prints passing and ignored per-test lines too.

```sh
rask clean
```

Forgets this module's local cache shard.

## What it looks like

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

## How it works

rask fingerprints each selected suite with the contents of its package, its `testdata`, the local packages it imports, and the Go workspace and environment salt. If that exact suite passed before, rask skips it; otherwise it runs `go test -json -count=1 -failfast -shuffle=on` and records only passing verdicts. The runner fails fast across suites so the first real failure stays near the bottom of the output. An optional remote cache tier lets trusted machines share those passing verdicts while keeping the local disk cache on. When configured, the summary shows remote read and write counts.

## Docs

- [Output](docs/output.md)
- [Caching](docs/caching.md)
- [Remote cache](docs/remote-cache.md)
- [Comparison](docs/comparison.md)
- [Architecture](docs/architecture.md)
- [CLI reference](docs/cli.md)
