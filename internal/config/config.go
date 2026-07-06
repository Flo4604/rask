// Package config owns rask's process-edge configuration loading.
//
// Cache and command internals receive this package's values explicitly so unit
// tests cannot accidentally discover a developer's home-directory credentials.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	// configEnvVar exists so scripts and tests can choose a config file without touching $HOME.
	configEnvVar = "RASK_CONFIG"

	// cacheDirEnvVar exists so callers can keep destructive cache operations inside an explicit root.
	cacheDirEnvVar = "RASK_CACHE_DIR"

	// cacheDirName keeps the default cache root isolated from other user-cache entries.
	cacheDirName = "rask"

	// remoteBucketEnvVar lets CI opt into the shared remote tier without writing a config file.
	remoteBucketEnvVar = "RASK_REMOTE_CACHE_BUCKET"

	// remotePrefixEnvVar lets multiple projects share one bucket without key collisions.
	remotePrefixEnvVar = "RASK_REMOTE_CACHE_PREFIX"

	// remoteEndpointEnvVar supports S3-compatible stores such as R2 alongside AWS S3.
	remoteEndpointEnvVar = "RASK_REMOTE_CACHE_ENDPOINT"

	// remoteRegionEnvVar supports SigV4-compatible stores that require non-AWS regions.
	remoteRegionEnvVar = "RASK_REMOTE_CACHE_REGION"

	// remoteAccessKeyIDEnvVar keeps credentials out of committed config files.
	remoteAccessKeyIDEnvVar = "RASK_REMOTE_CACHE_ACCESS_KEY_ID"

	// remoteSecretAccessKeyEnvVar keeps credentials out of committed config files.
	remoteSecretAccessKeyEnvVar = "RASK_REMOTE_CACHE_SECRET_ACCESS_KEY"
)

// Config is the single process-edge configuration value passed into commands.
type Config struct {
	// CacheDir is resolved once so cache constructors never inspect the process environment.
	CacheDir string

	// Remote is nil unless the user explicitly opted into the shared S3-compatible tier.
	Remote *Remote
}

// Remote carries a validated S3-compatible cache configuration.
type Remote struct {
	// Bucket is required to opt into remote cache traffic.
	Bucket string

	// Prefix isolates this tool's objects inside a shared bucket.
	Prefix string

	// Region scopes SigV4 signing and defaults for S3 endpoints.
	Region string

	// Endpoint selects the S3-compatible API host.
	Endpoint string

	// AccessKeyID signs remote cache requests without relying on ambient cloud config.
	AccessKeyID string

	// SecretAccessKey signs remote cache requests without relying on ambient cloud config.
	SecretAccessKey string
}

// configFile mirrors the on-disk JSON so unknown-field errors point at user input.
type configFile struct {
	// RemoteCache stays nested to keep future non-cache config from changing the JSON shape.
	RemoteCache *configRemoteCache `json:"remote_cache"`
}

// configRemoteCache preserves raw URL detection for the one removed migration field.
type configRemoteCache struct {
	// URL remains raw so a legacy field can produce the targeted migration error.
	URL json.RawMessage `json:"url"`

	// Bucket mirrors remote_cache.bucket before environment overlays are applied.
	Bucket string `json:"bucket"`

	// Prefix mirrors remote_cache.prefix before environment overlays are applied.
	Prefix string `json:"prefix"`

	// Region mirrors remote_cache.region before environment overlays are applied.
	Region string `json:"region"`

	// Endpoint mirrors remote_cache.endpoint before environment overlays are applied.
	Endpoint string `json:"endpoint"`

	// AccessKeyID mirrors remote_cache.access_key_id before environment overlays are applied.
	AccessKeyID string `json:"access_key_id"`

	// SecretAccessKey mirrors remote_cache.secret_access_key before environment overlays are applied.
	SecretAccessKey string `json:"secret_access_key"`
}

// Load reads all process configuration once so lower layers stay hermetic.
func Load(warnings io.Writer) (Config, error) {
	cacheDir, err := resolvedCacheDir()
	if err != nil {
		return Config{}, err
	}
	remote, err := loadRemoteCacheConfig(warnings)
	if err != nil {
		return Config{}, err
	}
	return Config{CacheDir: cacheDir, Remote: remote}, nil
}

// resolvedCacheDir centralizes disk-root selection before cache code is constructed.
func resolvedCacheDir() (string, error) {
	rootDir := os.Getenv(cacheDirEnvVar)
	if rootDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolving user cache dir: %w", err)
		}
		rootDir = filepath.Join(base, cacheDirName)
	}
	absolute, err := filepath.Abs(rootDir)
	if err != nil {
		return "", fmt.Errorf("resolving cache dir: %w", err)
	}
	return absolute, nil
}

// loadRemoteCacheConfig keeps remote opt-in validation at the config boundary.
func loadRemoteCacheConfig(warnings io.Writer) (*Remote, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	cfg, err := readConfigFile(path, warnings)
	if err != nil {
		return nil, err
	}
	cfg.applyEnv()
	if cfg.Bucket == "" {
		return nil, nil
	}
	cfg.applyDefaults()
	if err := cfg.validate(path); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// configPath preserves the historical RASK_CONFIG override for per-run config isolation.
func configPath() (string, error) {
	if path := os.Getenv(configEnvVar); path != "" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home for ~/.rask.json: %w", err)
	}
	return filepath.Join(home, ".rask.json"), nil
}

// readConfigFile separates absent configs from malformed configs for local-only defaults.
func readConfigFile(path string, warnings io.Writer) (Remote, error) {
	body, info, err := readConfigBody(path)
	if err != nil {
		return Remote{}, err
	}
	if body == nil {
		return Remote{}, nil
	}
	if warning := configPermissionWarning(path, info.Mode()); warning != "" {
		fmt.Fprintln(normalizeWarnings(warnings), warning)
	}
	parsed, err := parseConfigFile(body)
	if err != nil {
		return Remote{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return parsed, nil
}

// readConfigBody keeps filesystem errors tied to the path the user configured.
func readConfigBody(path string) ([]byte, os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return body, info, nil
}

// parseConfigFile is strict because misspelled credential fields silently disable sharing.
func parseConfigFile(body []byte) (Remote, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var file configFile
	if err := decoder.Decode(&file); err != nil {
		return Remote{}, err
	}
	if file.RemoteCache == nil {
		return Remote{}, nil
	}
	if file.RemoteCache.URL != nil {
		return Remote{}, fmt.Errorf("remote_cache.url is no longer supported; use remote_cache.bucket and remote_cache.prefix")
	}
	return Remote{
		Bucket:          file.RemoteCache.Bucket,
		Prefix:          file.RemoteCache.Prefix,
		Region:          file.RemoteCache.Region,
		Endpoint:        file.RemoteCache.Endpoint,
		AccessKeyID:     file.RemoteCache.AccessKeyID,
		SecretAccessKey: file.RemoteCache.SecretAccessKey,
	}, nil
}

// applyEnv overlays fields independently so secrets can stay outside JSON files.
func (r *Remote) applyEnv() {
	// Read only explicit rask remote-cache variables here. Ambient cloud
	// credentials often belong to unrelated tooling, and silently using them
	// could write cache objects to the wrong account.
	setFromEnv(&r.Bucket, remoteBucketEnvVar)
	setFromEnv(&r.Prefix, remotePrefixEnvVar)
	setFromEnv(&r.Endpoint, remoteEndpointEnvVar)
	setFromEnv(&r.Region, remoteRegionEnvVar)
	setFromEnv(&r.AccessKeyID, remoteAccessKeyIDEnvVar)
	setFromEnv(&r.SecretAccessKey, remoteSecretAccessKeyEnvVar)
}

// applyDefaults preserves existing remote-cache defaults once a bucket opts in.
func (r *Remote) applyDefaults() {
	if r.Region == "" {
		r.Region = "auto"
	}
	if r.Endpoint == "" {
		r.Endpoint = "https://s3." + r.Region + ".amazonaws.com"
	}
}

// validate catches startup-only remote mistakes before tests begin running.
func (r Remote) validate(path string) error {
	if err := validateBucketName(r.Bucket); err != nil {
		return err
	}
	missing := r.missingCredentialFields()
	if len(missing) > 0 {
		return fmt.Errorf("%s remote_cache.bucket requires credentials; missing %s",
			path, strings.Join(missing, " and "))
	}
	return nil
}

// missingCredentialFields names both file and environment locations to make fixes direct.
func (r Remote) missingCredentialFields() []string {
	var missing []string
	if r.AccessKeyID == "" {
		missing = append(missing, "remote_cache.access_key_id or "+remoteAccessKeyIDEnvVar)
	}
	if r.SecretAccessKey == "" {
		missing = append(missing, "remote_cache.secret_access_key or "+remoteSecretAccessKeyEnvVar)
	}
	return missing
}

// validateBucketName rejects ambiguous bucket names before S3 client construction.
func validateBucketName(bucket string) error {
	if len(bucket) < 3 || len(bucket) > 63 {
		return fmt.Errorf("remote_cache.bucket must be 3-63 characters")
	}
	for _, r := range bucket {
		if !('a' <= r && r <= 'z') && !('0' <= r && r <= '9') && r != '.' && r != '-' {
			return fmt.Errorf("remote_cache.bucket contains invalid character %q", r)
		}
	}
	return nil
}

// setFromEnv allows empty environment variables to intentionally clear file fields.
func setFromEnv(target *string, name string) {
	if value, ok := os.LookupEnv(name); ok {
		*target = value
	}
}

// normalizeWarnings lets tests pass nil when they only care about parsed values.
func normalizeWarnings(warnings io.Writer) io.Writer {
	if warnings == nil {
		return io.Discard
	}
	return warnings
}
