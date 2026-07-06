# Caching

rask stores passing suite verdicts in a content-addressed local object store. Cache hits skip whole suites. Failures always rerun.

## Disk layout

```text
~/Library/Caches/rask/
  CACHEDIR.TAG
  v1/
    <module-key>/
      <hh>/
        <digest>.json
```

Configuration is loaded once at startup. `RASK_CACHE_DIR` overrides the local root. Otherwise rask uses the user cache directory plus `/rask`. The module key is `sha256(modulePath)[:16]`, and `hh` is the first two characters of the 64-character suite digest.

Each object is a small JSON verdict with the import path, passed count, ignored count, elapsed seconds, and creation time. One object is capped at 16 KiB. The disk store writes through a temporary file and renames it into place.

## Configuration

`internal/config` is the only package that reads process configuration. Each command loads it once at startup. `RASK_CONFIG` selects the config file path, otherwise rask reads `~/.rask.json`. The remote config fields are `remote_cache.bucket`, `prefix`, `endpoint`, `region`, `access_key_id`, and `secret_access_key`; the matching `RASK_REMOTE_CACHE_*` variables override those fields one by one. `region` defaults to `auto` after a bucket opts in, and `endpoint` defaults to `https://s3.<region>.amazonaws.com`.

The JSON decoder rejects unknown fields. The old `remote_cache.url` field is a startup error. On Unix, a group- or world-readable config file prints one `chmod 600` warning and continues.

## Digests

The fingerprint has two phases. First, rask computes build digests for every discovered local package in build-import order. Then selected suites become terminal digests over their test files, test embeds, `testdata`, their package build digest, and build digests for discovered test-only imports.

The salt includes:

- the fingerprint format version
- the `go env` output, except derived `GOGCCFLAGS`
- the active `go.work` file content, when `GOWORK` is active
- every discovered local module root path, `go.mod`, and `go.sum`

The discovered local graph includes the main module, active `go.work` modules, and local filesystem `replace` targets. Editing a local dependency invalidates that package and selected suites that depend on it. External test import cycles stay finite because test imports are not graph edges; they are added only while hashing suite sinks.

## Cache policy

Only passing suites are stored. Failing suites delete the entry for their digest. Tests run with `-count=1`, so rask owns cache hits instead of layering on top of the Go test cache.

`--no-cache` bypasses cache reads for selected suites, but passing results still write back. That makes it useful when you want a fresh run now and a warm cache later.

Randomized ordering does not change cache keys. A pass under any suite or test order is still the same passing verdict, and a cache hit is served without rerunning. Every cache miss draws a fresh inter-suite order and passes `-shuffle=on` to `go test`, so order-dependent failures surface over time at zero added cost while cached passing suites stay fast.

Reads are self-healing. Missing, corrupt, invalid, oversized, or unreadable objects are treated as misses; corrupt and invalid objects are deleted. Hits refresh the object's modification time for LRU retention.

## Retention

The local cache keeps each module shard bounded by age and count:

- objects unused for 30 days are pruned
- each module keeps at most 4096 objects
- hits refresh the object timestamp

Remote S3-compatible buckets are retained by bucket lifecycle rules, not by rask. See [Remote cache](remote-cache.md) for lifecycle examples.

When a remote tier is configured, the final summary includes the number of cached suites served by the remote tier and the successful remote read and write counts:

```text
Finished 12 suites, 174 tests: 172 passed; 0 failed; 2 ignored in 320ms, 12 cached (12 remote) | remote: 12 reads, 0 writes
```

## Limitations

rask is not hermetic. Package sources, embeds, `testdata`, local dependency metadata, and the Go environment salt the digest, but undeclared inputs outside the package tree remain outside the cache key. Network calls, databases, clocks, system state, and files reached by custom paths can still affect tests without changing the digest. If a suite depends on those inputs, use `--no-cache` for that run or keep it on plain `go test`.
