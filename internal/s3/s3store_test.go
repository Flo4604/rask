package s3

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	// cancelDelayForTest gives the request time to reach the blocking handler.
	cancelDelayForTest = 50 * time.Millisecond

	// cancelledRequestMax proves context cancellation wins over the S3 timeout.
	cancelledRequestMax = 500 * time.Millisecond
)

func TestStoreRoundTrip(t *testing.T) {
	store := handlerStoreForTest(t, newObjectHandlerForTest(t))
	key := objectKeyForTest("a")
	if _, found, err := store.Get(context.Background(), key); err != nil || found {
		t.Fatalf("empty remote cache Get = found %v err %v", found, err)
	}
	if err := store.Put(context.Background(), key, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, found, err := store.Get(context.Background(), key); err != nil || !found {
		t.Fatalf("remote cache Get = found %v err %v", found, err)
	}
	if err := store.Delete(context.Background(), key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, err := store.Get(context.Background(), key); err != nil || found {
		t.Fatalf("deleted object Get = found %v err %v", found, err)
	}
}

func TestStoreTreatsNoSuchKeyAsMiss(t *testing.T) {
	key := objectKeyForTest("b")
	for name, handler := range map[string]http.Handler{
		"bare": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.NotFound(w, nil)
		}),
		"xml": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
		}),
	} {
		t.Run(name, func(t *testing.T) {
			store := handlerStoreForTest(t, handler)
			if _, found, err := store.Get(context.Background(), key); err != nil || found {
				t.Fatalf("Get = found %v err %v, want miss without error", found, err)
			}
		})
	}
}

func TestStoreFailurePolicy(t *testing.T) {
	key := objectKeyForTest("c")
	for name, store := range map[string]*Client{
		"refused":   refusedStoreForTest(t),
		"timeout":   timeoutStoreForTest(t),
		"non-2xx":   statusStoreForTest(t, http.StatusInternalServerError),
		"oversized": oversizedStoreForTest(t),
	} {
		t.Run(name, func(t *testing.T) {
			if _, found, err := store.Get(context.Background(), key); err != nil || found {
				t.Fatalf("Get = found %v err %v, want miss without error", found, err)
			}
		})
	}
	if err := refusedStoreForTest(t).Put(context.Background(), key, []byte(`{"ok":true}`)); err == nil {
		t.Fatalf("Put network failure must be a counted write error")
	}
}

func TestStoreRequestHeaders(t *testing.T) {
	store := handlerStoreForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHeader(t, r, "Authorization", "AWS4-HMAC-SHA256 ")
		assertHeader(t, r, "Content-Type", jsonContentType)
		assertHeader(t, r, "X-Amz-Date", "")
		assertHeader(t, r, "X-Amz-Content-Sha256", "")
		if !signedHeadersContain(r.Header.Get("Authorization"), "content-type") {
			t.Fatalf("Authorization=%q, want signed content-type", r.Header.Get("Authorization"))
		}
		if got, want := r.URL.Path, "/bucket/prefix/v1/"+moduleKeyForTest()+"/"+digestForTest("d")+".json"; got != want {
			t.Fatalf("path=%q, want %q", got, want)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := store.Put(context.Background(), objectKeyForTest("d"), []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
}

// TestStoreGetHonorsContextCancellation protects prompt Ctrl+C shutdown.
func TestStoreGetHonorsContextCancellation(t *testing.T) {
	listener := loopbackListenerForTest(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	store, err := New(Config{
		Bucket: "bucket", Prefix: "prefix", Region: "auto", Endpoint: server.URL,
		AccessKeyID: "access", SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(cancelDelayForTest, cancel)
	startedAt := time.Now()
	_, found, err := store.Get(ctx, objectKeyForTest("e"))
	elapsed := time.Since(startedAt)
	if err != nil || found {
		t.Fatalf("Get = found %v err %v, want silent miss", found, err)
	}
	if elapsed > cancelledRequestMax {
		t.Fatalf("cancelled Get took %s, want under %s", elapsed, cancelledRequestMax)
	}
}

// loopbackListenerForTest keeps httptest usable in IPv6-restricted sandboxes.
func loopbackListenerForTest(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("loopback bind unavailable in this sandbox: %v", err)
		}
		t.Fatalf("allocate loopback port: %v", err)
	}
	return listener
}

func TestSigV4SigningVectors(t *testing.T) {
	vectors := []struct {
		name      string
		method    string
		target    string
		body      string
		headers   map[string]string
		signed    string
		signature string
	}{
		{name: "get", method: "GET", target: "https://example.com/bucket/a.json", signed: "host;x-amz-content-sha256;x-amz-date", signature: "37c14061e9217baa703ee5a07792dc1b68a9e7fdaa4b2eff2cb79f6e79169e21"},
		{name: "put", method: "PUT", target: "https://example.com/bucket/a.json", body: `{"ok":true}`, headers: map[string]string{"Content-Type": jsonContentType}, signed: "content-type;host;x-amz-content-sha256;x-amz-date", signature: "f0e35646947bd9f91a6f022c7cc4a0234cb9e6472e92beb09b31a8235ae3c3e6"},
		{name: "delete", method: "DELETE", target: "https://example.com/bucket/a.json", signed: "host;x-amz-content-sha256;x-amz-date", signature: "83bfb5b25ac7e8a5ed367786355bfda156cab5e207b5c11ac6b96a064ce0dfd5"},
	}
	for _, vector := range vectors {
		t.Run(vector.name, func(t *testing.T) {
			request := newSignedVectorRequest(t, vector.method, vector.target, vector.body, vector.headers)
			if got := signedHeadersFromAuthorization(request.Header.Get("Authorization")); got != vector.signed {
				t.Fatalf("signed headers=%q, want %q", got, vector.signed)
			}
			got := signatureFromAuthorization(request.Header.Get("Authorization"))
			if got != vector.signature {
				t.Fatalf("signature=%s, want %s", got, vector.signature)
			}
		})
	}
}

func newObjectHandlerForTest(t *testing.T) http.Handler {
	t.Helper()
	var mu sync.Mutex
	objects := map[string][]byte{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validSignedRequest(r) {
			http.Error(w, "unauthorized", http.StatusForbidden)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/")
		switch r.Method {
		case http.MethodGet:
			serveGet(w, key, &mu, objects)
		case http.MethodPut:
			storePut(w, r, key, &mu, objects)
		case http.MethodDelete:
			mu.Lock()
			delete(objects, key)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func serveGet(w http.ResponseWriter, key string, mu *sync.Mutex, objects map[string][]byte) {
	mu.Lock()
	body, found := objects[key]
	mu.Unlock()
	if !found {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code></Error>`))
		return
	}
	_, _ = w.Write(append([]byte(nil), body...))
}

func storePut(w http.ResponseWriter, r *http.Request, key string, mu *sync.Mutex, objects map[string][]byte) {
	body, _ := io.ReadAll(r.Body)
	mu.Lock()
	objects[key] = append([]byte(nil), body...)
	mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func validSignedRequest(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") &&
		r.Header.Get("X-Amz-Date") != "" &&
		r.Header.Get("X-Amz-Content-Sha256") != ""
}

func handlerStoreForTest(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	store := emptyStoreForTest(t)
	store.client.Transport = handlerTransport{handler: handler}
	return store
}

func emptyStoreForTest(t *testing.T) *Client {
	t.Helper()
	store, err := New(Config{
		Bucket: "bucket", Prefix: "prefix", Region: "auto", Endpoint: "http://rask.test",
		AccessKeyID: "access", SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	store.now = func() time.Time { return time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC) }
	return store
}

func refusedStoreForTest(t *testing.T) *Client {
	t.Helper()
	store := emptyStoreForTest(t)
	store.client.Transport = errorTransport{}
	return store
}

func timeoutStoreForTest(t *testing.T) *Client {
	t.Helper()
	store := emptyStoreForTest(t)
	store.client = &http.Client{Timeout: 10 * time.Millisecond, Transport: timeoutTransport{}}
	return store
}

func statusStoreForTest(t *testing.T, status int) *Client {
	t.Helper()
	return handlerStoreForTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
}

func oversizedStoreForTest(t *testing.T) *Client {
	t.Helper()
	return handlerStoreForTest(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", defaultObjectBytesMax+1)))
	}))
}

func newSignedVectorRequest(t *testing.T, method string, target string, body string, headers map[string]string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	signS3Request(request, []byte(body), "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "us-east-1", now)
	return request
}

func objectKeyForTest(seed string) string {
	digest := digestForTest(seed)
	return formatVersion + "/" + moduleKeyForTest() + "/" + digest[:2] + "/" + digest + ".json"
}

func moduleKeyForTest() string {
	return strings.Repeat("a", moduleKeyHexLength)
}

func digestForTest(seed string) string {
	return strings.Repeat(seed, hashHexLength)
}

func signatureFromAuthorization(header string) string {
	const marker = "Signature="
	index := strings.LastIndex(header, marker)
	if index < 0 {
		return ""
	}
	return header[index+len(marker):]
}

func signedHeadersFromAuthorization(header string) string {
	const marker = "SignedHeaders="
	index := strings.Index(header, marker)
	if index < 0 {
		return ""
	}
	signed := header[index+len(marker):]
	if comma := strings.Index(signed, ","); comma >= 0 {
		signed = signed[:comma]
	}
	return signed
}

func signedHeadersContain(header string, name string) bool {
	for _, signed := range strings.Split(signedHeadersFromAuthorization(header), ";") {
		if signed == name {
			return true
		}
	}
	return false
}

func assertHeader(t *testing.T, r *http.Request, name string, prefix string) {
	t.Helper()
	value := r.Header.Get(name)
	if value == "" || (prefix != "" && !strings.HasPrefix(value, prefix)) {
		t.Fatalf("%s=%q, want prefix %q", name, value, prefix)
	}
}

type handlerTransport struct {
	handler http.Handler
}

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	t.handler.ServeHTTP(response, req)
	return response.Result(), nil
}

type errorTransport struct{}

func (t errorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

type timeoutTransport struct{}

func (t timeoutTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}
