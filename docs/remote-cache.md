# Remote cache

The remote cache is an optional S3-compatible tier for sharing passing suite verdicts. The local disk cache stays on. Local hits make no network request; remote hits are validated and backfilled to disk. rask loads configuration once at startup, before cache construction.

There is one remote tier: an S3-compatible bucket.

## Cloudflare R2 setup

Create an R2 bucket, then create an R2 API token that can read and write that bucket. Cloudflare shows the access key ID, secret access key, and account endpoint:

```text
https://<account>.r2.cloudflarestorage.com
```

Write `~/.rask.json`:

```json
{
  "remote_cache": {
    "bucket": "rask-cache",
    "prefix": "rask",
    "region": "auto",
    "endpoint": "https://<account>.r2.cloudflarestorage.com",
    "access_key_id": "<access-key-id>",
    "secret_access_key": "<secret-access-key>"
  }
}
```

Fix the file mode if other users can read it:

```sh
chmod 600 ~/.rask.json
```

A missing config file is fine and means local-only caching. Malformed JSON, unknown fields, an old `remote_cache.url` field, a configured bucket without credentials, an invalid bucket name, or an invalid endpoint fail loudly at startup. On Unix, if the file is group- or world-readable, rask prints one `chmod 600` warning and continues.

`RASK_CONFIG` overrides the config path exactly:

```sh
RASK_CONFIG=/path/to/rask.json rask
```

## CI environment

Environment variables override the file field by field. In CI, set rask's explicit remote-cache variables instead of writing a config file:

```yaml
env:
  RASK_REMOTE_CACHE_BUCKET: rask-cache
  RASK_REMOTE_CACHE_PREFIX: rask
  RASK_REMOTE_CACHE_REGION: auto
  RASK_REMOTE_CACHE_ENDPOINT: https://<account>.r2.cloudflarestorage.com
  RASK_REMOTE_CACHE_ACCESS_KEY_ID: ${{ secrets.RASK_R2_ACCESS_KEY_ID }}
  RASK_REMOTE_CACHE_SECRET_ACCESS_KEY: ${{ secrets.RASK_R2_SECRET_ACCESS_KEY }}
```

Precedence:

| Config field | Environment override | Default |
| --- | --- | --- |
| `remote_cache.bucket` | `RASK_REMOTE_CACHE_BUCKET` | unset |
| `remote_cache.prefix` | `RASK_REMOTE_CACHE_PREFIX` | empty |
| `remote_cache.endpoint` | `RASK_REMOTE_CACHE_ENDPOINT` | `https://s3.<region>.amazonaws.com` |
| `remote_cache.region` | `RASK_REMOTE_CACHE_REGION` | `auto` when remote is configured |
| `remote_cache.access_key_id` | `RASK_REMOTE_CACHE_ACCESS_KEY_ID` | unset |
| `remote_cache.secret_access_key` | `RASK_REMOTE_CACHE_SECRET_ACCESS_KEY` | unset |

Setting `remote_cache.bucket` or `RASK_REMOTE_CACHE_BUCKET` opts into the remote tier. The old URL field and scheme parsing are gone; use `bucket` plus optional `prefix`.

## Object layout

rask uses path-style S3 requests against the configured endpoint:

```text
<endpoint>/<bucket>/<prefix>/v1/<moduleKey>/<digest>.json
```

`moduleKey` is `sha256(modulePath)[:16]`. `digest` is the 64-character suite digest. Both are lowercase hex. Each object is the same JSON cache entry used by the local disk store and is capped at 16 KiB.

`Get` treats network errors, auth errors, non-2xx responses, oversized bodies, bare `404`, and S3 `NoSuchKey` responses as misses. `Put` and `Delete` are best-effort remote writes after the local disk operation. A misconfigured or unavailable bucket must not fail a test run after startup; affected suites rerun and local caching still works. Each request has a three-second timeout.

The final run summary shows successful remote cache hits and writes when the remote tier is configured:

```text
Finished 12 suites, 174 tests: 172 passed; 0 failed; 2 ignored in 320ms, 12 cached (12 remote) | remote: 12 reads, 0 writes
```

Startup config errors are different: malformed config, old URL fields, invalid bucket names, configured buckets without credentials, or invalid endpoints fail before the run starts.

## Retention

There is no rask process pruning the bucket. Remote retention belongs to bucket lifecycle rules. `rask clean` and local pruning remove only local disk objects; remote cache objects age out according to the bucket policy.

Lifecycle TTL counts from object creation, not last access. S3 and R2 do not track cache-object access time for lifecycle rules, so unlike the local LRU cache, an actively used verdict eventually expires. That is benign: the next run misses, reruns that suite, and writes a fresh object with a new creation time.

Use a 30-90 day TTL scoped to the rask prefix.

Cloudflare R2 lifecycle rule example:

```json
{
  "rules": [
    {
      "id": "expire-rask-cache",
      "enabled": true,
      "conditions": {
        "prefix": "rask/"
      },
      "delete_objects_transition": {
        "condition": {
          "max_age": 7776000
        }
      }
    }
  ]
}
```

Equivalent AWS S3 lifecycle configuration:

```json
{
  "Rules": [
    {
      "ID": "expire-rask-cache",
      "Status": "Enabled",
      "Filter": {
        "Prefix": "rask/"
      },
      "Expiration": {
        "Days": 90
      }
    }
  ]
}
```

```xml
<LifecycleConfiguration>
  <Rule>
    <ID>expire-rask-cache</ID>
    <Status>Enabled</Status>
    <Filter>
      <Prefix>rask/</Prefix>
    </Filter>
    <Expiration>
      <Days>90</Days>
    </Expiration>
  </Rule>
</LifecycleConfiguration>
```

rask does not emulate LRU by self-copying objects on read. That would double request cost to avoid an occasional benign rerun.

## Trust model

Anyone with write access to the bucket can write passing verdicts for known module keys and suite digests. Use a bucket and prefix where that is acceptable. TLS, IAM policy design, and sandboxed test execution are outside rask.

More details on local cache keys and validation live in [Caching](caching.md). Command flags and environment variables live in [CLI reference](cli.md).
