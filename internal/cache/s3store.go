package cache

import (
	"github.com/chronark/rask/internal/config"
	"github.com/chronark/rask/internal/s3"
)

// openS3ObjectStore adapts validated process config into the remote object API.
func openS3ObjectStore(cfg *config.Remote) (*s3.Client, error) {
	if cfg == nil {
		return nil, nil
	}
	return s3.New(s3.Config{
		Bucket:          cfg.Bucket,
		Prefix:          cfg.Prefix,
		Endpoint:        cfg.Endpoint,
		Region:          cfg.Region,
		AccessKeyID:     cfg.AccessKeyID,
		SecretAccessKey: cfg.SecretAccessKey,
		MaxObjectBytes:  objectBytesMax,
	})
}
