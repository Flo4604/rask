package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	// jsonContentType is the media type used for cache objects.
	jsonContentType = "application/json"

	// s3ServiceName is the SigV4 service scope for S3-compatible APIs.
	s3ServiceName = "s3"

	// s3RequestTimeout bounds remote cache latency so tests do not stall.
	s3RequestTimeout = 3 * time.Second

	// defaultObjectBytesMax bounds one cache object when callers leave Config zero-valued.
	defaultObjectBytesMax = 16 << 10

	// formatVersion is the cache object layout version this client maps to S3.
	formatVersion = "v1"

	// hashHexLength is the length of a SHA-256 digest rendered as lowercase hex.
	hashHexLength = 64

	// moduleKeyHexLength is the number of SHA-256 hex characters used for module shards.
	moduleKeyHexLength = 16
)

// Config describes one S3-compatible remote cache bucket.
type Config struct {
	Bucket          string
	Prefix          string
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	MaxObjectBytes  int
}

// Client talks to an S3-compatible bucket with stdlib SigV4 signing.
type Client struct {
	bucket          string
	prefix          string
	endpoint        *url.URL
	region          string
	accessKeyID     string
	secretAccessKey string
	maxObjectBytes  int
	client          *http.Client
	now             func() time.Time
}

// New parses the endpoint before any test run starts.
func New(cfg Config) (*Client, error) {
	endpoint, err := parseS3Endpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	maxObjectBytes := cfg.MaxObjectBytes
	if maxObjectBytes <= 0 {
		maxObjectBytes = defaultObjectBytesMax
	}
	return &Client{
		bucket: cfg.Bucket, prefix: cfg.Prefix, endpoint: endpoint, region: cfg.Region,
		accessKeyID: cfg.AccessKeyID, secretAccessKey: cfg.SecretAccessKey,
		maxObjectBytes: maxObjectBytes,
		client:         &http.Client{Timeout: s3RequestTimeout},
		now:            time.Now,
	}, nil
}

// Get returns a decoded remote object body or a miss for every remote failure.
func (s *Client) Get(ctx context.Context, key string) ([]byte, bool, error) {
	request, ok := s.newRequest(ctx, http.MethodGet, key, nil)
	if !ok {
		return nil, false, nil
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, false, nil
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound || response.StatusCode/100 != 2 {
		return nil, false, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(s.maxObjectBytes)+1))
	if err != nil || len(body) > s.maxObjectBytes {
		return nil, false, nil
	}
	return body, true, nil
}

// Put writes a remote object and reports failures for the tiered store to count.
func (s *Client) Put(ctx context.Context, key string, body []byte) error {
	request, ok := s.newRequest(ctx, http.MethodPut, key, bytes.NewReader(body))
	if !ok {
		return errors.New("malformed cache key")
	}
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("remote cache PUT returned %s", response.Status)
	}
	return nil
}

// Delete asks S3 to remove an object and ignores all runtime failures.
func (s *Client) Delete(ctx context.Context, key string) error {
	request, ok := s.newRequest(ctx, http.MethodDelete, key, nil)
	if !ok {
		return nil
	}
	response, err := s.client.Do(request)
	if err == nil {
		_ = response.Body.Close()
	}
	return nil
}

func (s *Client) newRequest(ctx context.Context, method string, key string, body io.Reader) (*http.Request, bool) {
	objectName, ok := s.objectName(key)
	if !ok {
		return nil, false
	}
	request, err := http.NewRequestWithContext(ctx, method, s.objectURL(objectName), body)
	if err != nil {
		return nil, false
	}
	var payload []byte
	if body != nil {
		payload, _ = io.ReadAll(body)
		request.Body = io.NopCloser(bytes.NewReader(payload))
	}
	if method == http.MethodPut {
		request.Header.Set("Content-Type", jsonContentType)
	}
	signS3Request(request, payload, s.accessKeyID, s.secretAccessKey, s.region, s.now().UTC())
	return request, true
}

func (s *Client) objectName(key string) (string, bool) {
	moduleKey, digest, ok := objectPartsFromKey(key)
	if !ok {
		return "", false
	}
	parts := []string{s.prefix, formatVersion, moduleKey, digest + ".json"}
	return strings.Trim(strings.Join(parts, "/"), "/"), true
}

func (s *Client) objectURL(objectName string) string {
	next := *s.endpoint
	next.Path = joinURLPath(s.endpoint.Path, s.bucket, objectName)
	next.RawQuery = ""
	next.Fragment = ""
	return next.String()
}

func parseS3Endpoint(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing S3 endpoint: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("parsing S3 endpoint: URL must include scheme and host")
	}
	return parsed, nil
}

func joinURLPath(parts ...string) string {
	escaped := make([]string, 0, len(parts))
	for _, part := range parts {
		for _, segment := range strings.Split(strings.Trim(part, "/"), "/") {
			if segment != "" {
				escaped = append(escaped, url.PathEscape(segment))
			}
		}
	}
	return "/" + strings.Join(escaped, "/")
}

// signS3Request follows SigV4 task 1-4: canonical request, string-to-sign, key derivation, Authorization.
func signS3Request(req *http.Request, payload []byte, accessKey string, secretKey string, region string, now time.Time) {
	payloadHash := sha256Hex(payload)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	scope := credentialScope(now, region, s3ServiceName)
	canonical, signedHeaders := canonicalRequest(req, payloadHash)
	toSign := stringToSign(now, scope, canonical)
	signature := hex.EncodeToString(signingHMAC(secretKey, now, region, s3ServiceName, toSign))
	req.Header.Set("Authorization", authorizationHeader(accessKey, scope, signedHeaders, signature))
}

func canonicalRequest(req *http.Request, payloadHash string) (string, string) {
	headers, signedHeaders := canonicalHeaders(req)
	parts := []string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.Query().Encode(),
		headers,
		signedHeaders,
		payloadHash,
	}
	return strings.Join(parts, "\n"), signedHeaders
}

func canonicalHeaders(req *http.Request) (string, string) {
	values := map[string]string{"host": req.URL.Host}
	for name, list := range req.Header {
		lower := strings.ToLower(name)
		values[lower] = strings.Join(list, ",")
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var headers strings.Builder
	for _, name := range names {
		fmt.Fprintf(&headers, "%s:%s\n", name, trimHeaderValue(values[name]))
	}
	return headers.String(), strings.Join(names, ";")
}

func stringToSign(now time.Time, scope string, canonical string) string {
	return strings.Join([]string{
		"AWS4-HMAC-SHA256",
		now.Format("20060102T150405Z"),
		scope,
		sha256Hex([]byte(canonical)),
	}, "\n")
}

func signingHMAC(secret string, now time.Time, region string, service string, text string) []byte {
	dateKey := hmacSHA256([]byte("AWS4"+secret), now.Format("20060102"))
	regionKey := hmacSHA256(dateKey, region)
	serviceKey := hmacSHA256(regionKey, service)
	signingKey := hmacSHA256(serviceKey, "aws4_request")
	return hmacSHA256(signingKey, text)
}

func hmacSHA256(key []byte, text string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(text))
	return mac.Sum(nil)
}

func credentialScope(now time.Time, region string, service string) string {
	return now.Format("20060102") + "/" + region + "/" + service + "/aws4_request"
}

func authorizationHeader(accessKey string, scope string, signedHeaders string, signature string) string {
	return "AWS4-HMAC-SHA256 Credential=" + accessKey + "/" + scope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + signature
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func trimHeaderValue(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func objectPartsFromKey(key string) (string, string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 4 || parts[0] != formatVersion {
		return "", "", false
	}
	digest := strings.TrimSuffix(parts[3], ".json")
	if digest == parts[3] || !validDigest(digest) {
		return "", "", false
	}
	if !validModuleKey(parts[1]) || parts[2] != digest[:2] {
		return "", "", false
	}
	return parts[1], digest, true
}

func validModuleKey(key string) bool {
	return validHex(key, moduleKeyHexLength)
}

func validDigest(digest string) bool {
	return validHex(digest, hashHexLength)
}

func validHex(value string, length int) bool {
	if len(value) != length || strings.ContainsAny(value, "/\\.") {
		return false
	}
	for _, r := range value {
		if !('0' <= r && r <= '9') && !('a' <= r && r <= 'f') {
			return false
		}
	}
	return true
}
