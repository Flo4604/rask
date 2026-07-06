# Comparison

The idea, hash the inputs and skip what already passed, is not new. What differs between tools is how much of your build they need to own in exchange for it.

| | `go test` | rask | gotestsum | Bazel / Buck2 / Pants |
| --- | --- | --- | --- | --- |
| Config required | none | none | none | BUILD files for every package |
| Test result caching | per checkout | content-addressed | none | content-addressed |
| Shared across worktrees / branches | no | yes | no | yes |
| Remote / CI cache | no | yes, via S3-compatible buckets | no | yes |
| Hermetic execution | no | no | no | sandboxed |
| Scope | everything: bench, fuzz, cover, filters | whole-suite runs only | output formatting | multi-language builds and tests |

**`go test`.** Go already caches test results, but the cache is easy to bust through flags and environment access, and it is keyed per checkout. Two worktrees of the same commit each pay full price, and flipping between branches invalidates back and forth. rask's digests depend on content, so identical code hits the cache no matter where it is checked out. Everything beyond whole-suite runs, including `-run` filters, benchmarks, fuzzing, and coverage, stays with plain `go test`.

**gotestsum.** gotestsum improves `go test` output, but runs everything every time. rask overlaps with it on presentation and adds the cache.

**Bazel, Buck2, and Pants.** These tools implement content-addressed skipping more rigorously than rask will: declared inputs, sandboxed hermetic actions, remote caching, and remote execution across languages. The price is owning the build through BUILD files, toolchain wiring, and graph maintenance. rask is the smaller trade. It lets `go list` keep owning the Go dependency graph, so adoption is one binary and no config files.

Honest limitation of that trade: rask is not hermetic. Package sources, embeds, `testdata/`, dependency metadata, and the Go environment salt the digest, but undeclared inputs outside the package tree remain outside the cache key. Bazel's sandbox catches that class of bug. rask keeps Go's normal execution model and reruns only when its observed inputs change.

See [Caching](caching.md) for the exact cache key and [Remote cache](remote-cache.md) for the shared S3-compatible tier.
