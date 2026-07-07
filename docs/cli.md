# CLI reference

Testing is the default action. `rask`, `rask test`, and `rask t` all run test suites.

## `rask`

```text
rask [options] [command] [arguments...]
```

With no package patterns, rask uses `./...`.

```sh
rask
rask ./pkg/...
rask -v ./pkg/... --no-cache
rask --concurrency 4 ./...
```

Options:

| Flag | Meaning |
| --- | --- |
| `-v` | Render passing and ignored test lines. |
| `--verbose` | Render passing and ignored test lines. |
| `--no-cache` | Bypass cache reads for this run and refresh passing suites. |
| `--concurrency` | Set how many stale suites run at once. `0` or unset uses automatic sizing. |
| `--version` | Print the rask version and exit before running tests. |

rask always runs whole suites, always fails fast, and always randomizes order. Suites are scheduled in a randomized dependency-respecting order, and each stale suite runs `go test` with `-shuffle=on`. `--concurrency` controls how many suites are in flight at once — each in-flight suite is one bounded worker doing its cache check and, when stale, one suite-level `go test` process; it does not change intra-suite Go test behavior. When `--concurrency` is unset or `0`, rask uses `min(32, max(1, GOMAXPROCS-1))`. Explicit values must be between `1` and `32`. Failed suite output includes Go's `-test.shuffle <seed>` line when Go emits one, so a shuffled failure can be replayed directly with `go test -shuffle=<seed> <pkg>`. Filtered or customized runs such as `-run`, benchmarks, fuzzing, and coverage belong to plain `go test`. Unknown test flags are rejected with that hint.

## `rask clean`

```text
rask clean [options]
```

```sh
rask clean
rask clean --all
```

Options:

| Flag | Meaning |
| --- | --- |
| `--all` | Remove every rask-owned cache shard under the local cache root. |

Without `--all`, `clean` removes only the current module's local cache shard. Remote S3-compatible buckets are not pruned by the client; configure bucket lifecycle rules instead.

## Environment

| Variable | Meaning | Default |
| --- | --- | --- |
| `RASK_CONFIG` | Override the config file path. | `~/.rask.json` |
| `RASK_CACHE_DIR` | Root directory of the local disk cache. | user cache dir + `/rask` |
| `RASK_REMOTE_CACHE_BUCKET` | Optional S3-compatible bucket. Setting it opts into the remote cache. | unset |
| `RASK_REMOTE_CACHE_PREFIX` | Optional object key prefix inside the bucket. | empty |
| `RASK_REMOTE_CACHE_ENDPOINT` | S3-compatible endpoint, for example Cloudflare R2. | `https://s3.<region>.amazonaws.com` |
| `RASK_REMOTE_CACHE_REGION` | SigV4 signing region. | `auto` when remote is configured |
| `RASK_REMOTE_CACHE_ACCESS_KEY_ID` | S3-compatible access key. | config file or unset |
| `RASK_REMOTE_CACHE_SECRET_ACCESS_KEY` | S3-compatible secret key. | config file or unset |
| `NO_COLOR` | Disable colors and the sticky progress region. | unset |

Configuration file schema:

```json
{
  "remote_cache": {
    "bucket": "bucket",
    "prefix": "prefix",
    "region": "auto",
    "endpoint": "https://<account>.r2.cloudflarestorage.com",
    "access_key_id": "...",
    "secret_access_key": "..."
  }
}
```

Commands load configuration once at startup.

Global help is available with `--help`, `-h`, or `help`.
