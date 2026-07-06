package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadCacheDir protects the process-edge cache directory selection contract.
func TestLoadCacheDir(t *testing.T) {
	t.Run("env override", func(t *testing.T) {
		clearRemoteEnv(t)
		cacheDir := t.TempDir()
		t.Setenv(cacheDirEnvVar, cacheDir)
		t.Setenv(configEnvVar, filepath.Join(t.TempDir(), "missing.json"))
		cfg, err := Load(io.Discard)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		if cfg.CacheDir != mustAbs(t, cacheDir) || cfg.Remote != nil {
			t.Fatalf("cfg=%+v, want explicit disk-only dir", cfg)
		}
	})
	t.Run("user cache fallback", func(t *testing.T) {
		clearRemoteEnv(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv(configEnvVar, filepath.Join(t.TempDir(), "missing.json"))
		t.Setenv(cacheDirEnvVar, "")
		cfg, err := Load(io.Discard)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		if !strings.HasSuffix(filepath.ToSlash(cfg.CacheDir), "/"+cacheDirName) {
			t.Fatalf("cache dir=%q, want rask suffix", cfg.CacheDir)
		}
	})
}

// TestRemoteCacheConfig protects file parsing and per-field environment overlays.
func TestRemoteCacheConfig(t *testing.T) {
	t.Run("missing file is local only", func(t *testing.T) {
		clearRemoteEnv(t)
		t.Setenv(configEnvVar, filepath.Join(t.TempDir(), "missing.json"))
		cfg, err := loadRemoteCacheConfig(io.Discard)
		if err != nil || cfg != nil {
			t.Fatalf("cfg=%+v err=%v, want local only", cfg, err)
		}
	})
	t.Run("file only", func(t *testing.T) {
		clearRemoteEnv(t)
		path := writeConfigForTest(t, `{"remote_cache":{"bucket":"bucket","prefix":"p","region":"auto","endpoint":"http://e","access_key_id":"a","secret_access_key":"s"}}`)
		t.Setenv(configEnvVar, path)
		cfg, err := loadRemoteCacheConfig(io.Discard)
		if err != nil || cfg.Bucket != "bucket" || cfg.Prefix != "p" || cfg.Endpoint != "http://e" {
			t.Fatalf("cfg=%+v err=%v", cfg, err)
		}
	})
	t.Run("env overrides file per field", func(t *testing.T) {
		clearRemoteEnv(t)
		path := writeConfigForTest(t, `{"remote_cache":{"bucket":"file","prefix":"file","region":"r1","endpoint":"http://file","access_key_id":"file","secret_access_key":"file"}}`)
		t.Setenv(configEnvVar, path)
		setS3Env(t, "env", "p", "http://env")
		cfg, err := loadRemoteCacheConfig(io.Discard)
		if err != nil || cfg.Bucket != "env" || cfg.Prefix != "p" || cfg.AccessKeyID != "access" {
			t.Fatalf("cfg=%+v err=%v", cfg, err)
		}
	})
	t.Run("old url field rejected", func(t *testing.T) {
		clearRemoteEnv(t)
		path := writeConfigForTest(t, `{"remote_cache":{"url":"s3://b"}}`)
		t.Setenv(configEnvVar, path)
		_, err := loadRemoteCacheConfig(io.Discard)
		if err == nil || !strings.Contains(err.Error(), "remote_cache.bucket") {
			t.Fatalf("err=%v, want old url pointer", err)
		}
	})
	t.Run("strict unknown field", func(t *testing.T) {
		clearRemoteEnv(t)
		path := writeConfigForTest(t, `{"remote_cache":{"bucket":"bucket","typo":true}}`)
		t.Setenv(configEnvVar, path)
		_, err := loadRemoteCacheConfig(io.Discard)
		if err == nil || !strings.Contains(err.Error(), `unknown field "typo"`) {
			t.Fatalf("err=%v, want unknown field", err)
		}
	})
}

// TestRemoteCacheConfigValidation protects startup errors for unusable remote configs.
func TestRemoteCacheConfigValidation(t *testing.T) {
	t.Run("bucket requires explicit credentials", func(t *testing.T) {
		clearRemoteEnv(t)
		t.Setenv(configEnvVar, filepath.Join(t.TempDir(), "missing.json"))
		t.Setenv(remoteBucketEnvVar, "bucket")
		_, err := loadRemoteCacheConfig(io.Discard)
		for _, want := range []string{
			"remote_cache.access_key_id or " + remoteAccessKeyIDEnvVar,
			"remote_cache.secret_access_key or " + remoteSecretAccessKeyEnvVar,
		} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err=%v, want %q", err, want)
			}
		}
	})
	t.Run("bucket charset", func(t *testing.T) {
		clearRemoteEnv(t)
		t.Setenv(configEnvVar, filepath.Join(t.TempDir(), "missing.json"))
		setS3Env(t, "Bad_Bucket", "", "http://rask.test")
		_, err := loadRemoteCacheConfig(io.Discard)
		if err == nil || !strings.Contains(err.Error(), "invalid character") {
			t.Fatalf("err=%v, want bucket charset error", err)
		}
	})
	t.Run("bucket length", func(t *testing.T) {
		clearRemoteEnv(t)
		t.Setenv(configEnvVar, filepath.Join(t.TempDir(), "missing.json"))
		setS3Env(t, "ab", "", "http://rask.test")
		_, err := loadRemoteCacheConfig(io.Discard)
		if err == nil || !strings.Contains(err.Error(), "3-63") {
			t.Fatalf("err=%v, want bucket length error", err)
		}
	})
}

// TestRemoteCacheConfigWarnings protects the secret-file permission warning.
func TestRemoteCacheConfigWarnings(t *testing.T) {
	clearRemoteEnv(t)
	configPath := writeConfigForTest(t, `{"remote_cache":{"bucket":"bucket","access_key_id":"a","secret_access_key":"s"}}`)
	if err := os.Chmod(configPath, 0o644); err != nil {
		t.Fatalf("chmod config: %v", err)
	}
	t.Setenv(configEnvVar, configPath)
	out := new(strings.Builder)
	if _, err := loadRemoteCacheConfig(out); err != nil {
		t.Fatalf("load config: %v", err)
	}
	if warning := out.String(); warning != "" && !strings.Contains(warning, "chmod 600") {
		t.Fatalf("warning=%q, want chmod 600", warning)
	}
}

// writeConfigForTest keeps each config test isolated from the developer's home directory.
func writeConfigForTest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rask.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// clearRemoteEnv removes remote overlays so subtests only see their declared inputs.
func clearRemoteEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		remoteBucketEnvVar,
		remotePrefixEnvVar,
		remoteEndpointEnvVar,
		remoteRegionEnvVar,
		remoteAccessKeyIDEnvVar,
		remoteSecretAccessKeyEnvVar,
	} {
		previous, existed := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(name, previous)
				return
			}
			_ = os.Unsetenv(name)
		})
	}
}

// setS3Env keeps remote fixture setup consistent with production overlay names.
func setS3Env(t *testing.T, bucket string, prefix string, endpoint string) {
	t.Helper()
	t.Setenv(remoteBucketEnvVar, bucket)
	t.Setenv(remotePrefixEnvVar, prefix)
	t.Setenv(remoteAccessKeyIDEnvVar, "access")
	t.Setenv(remoteSecretAccessKeyEnvVar, "secret")
	t.Setenv(remoteRegionEnvVar, "auto")
	t.Setenv(remoteEndpointEnvVar, endpoint)
}

// mustAbs lets tests compare the same resolved cache-dir shape as Load.
func mustAbs(t *testing.T, path string) string {
	t.Helper()
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs %s: %v", path, err)
	}
	return absolute
}
